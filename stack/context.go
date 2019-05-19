// Copyright 2018 Marc-Antoine Ruel. All rights reserved.
// Use of this source code is governed under the Apache License, Version 2.0
// that can be found in the LICENSE file.

package stack

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Context is a parsing context.
//
// It contains the deduced GOROOT and GOPATH, if guesspaths is true.
type Context struct {
	// Goroutines is the Goroutines found.
	//
	// They are in the order that they were printed.
	Goroutines []*Goroutine

	// GOROOT is the GOROOT as detected in the traceback, not the on the host.
	//
	// It can be empty if no root was determined, for example the traceback
	// contains only non-stdlib source references.
	//
	// Empty is guesspaths was false.
	GOROOT string
	// GOPATHs is the GOPATH as detected in the traceback, with the value being
	// the corresponding path mapped to the host.
	//
	// It can be empty if only stdlib code is in the traceback or if no local
	// sources were matched up. In the general case there is only one entry in
	// the map.
	//
	// Nil is guesspaths was false.
	GOPATHs map[string]string

	localgoroot  string
	localgopaths []string
}

// ParseDump processes the output from runtime.Stack().
//
// Returns nil *Context if no stack trace was detected.
//
// It pipes anything not detected as a panic stack trace from r into out. It
// assumes there is junk before the actual stack trace. The junk is streamed to
// out.
//
// If guesspaths is false, no guessing of GOROOT and GOPATH is done, and Call
// entites do not have LocalSrcPath and IsStdlib filled in.
func ParseDump(r io.Reader, out io.Writer, guesspaths bool) (*Context, error) {
	goroutines, err := parseDump(r, out)
	if len(goroutines) == 0 {
		return nil, err
	}
	c := &Context{
		Goroutines:   goroutines,
		localgoroot:  runtime.GOROOT(),
		localgopaths: getGOPATHs(),
	}
	nameArguments(goroutines)
	// Corresponding local values on the host for Context.
	if guesspaths {
		c.findRoots()
		for _, r := range c.Goroutines {
			// Note that this is important to call it even if
			// c.GOROOT == c.localgoroot.
			r.updateLocations(c.GOROOT, c.localgoroot, c.GOPATHs)
		}
	}
	return c, err
}

// Private stuff.

const lockedToThread = "locked to thread"

// These are effectively constants.
var (
	// TODO(maruel): Handle corrupted stack cases:
	// - missed stack barrier
	// - found next stack barrier at 0x123; expected
	// - runtime: unexpected return pc for FUNC_NAME called from 0x123

	reRoutineHeader = regexp.MustCompile("^goroutine (\\d+) \\[([^\\]]+)\\]\\:\r?\n$")
	reMinutes       = regexp.MustCompile("^(\\d+) minutes$")
	reUnavail       = regexp.MustCompile("^(?:\t| +)goroutine running on other thread; stack unavailable")
	// See gentraceback() in src/runtime/traceback.go for more information.
	// - Sometimes the source file comes up as "<autogenerated>". It is the
	//   compiler than generated these, not the runtime.
	// - The tab may be replaced with spaces when a user copy-paste it, handle
	//   this transparently.
	// - "runtime.gopanic" is explicitly replaced with "panic" by gentraceback().
	// - The +0x123 byte offset is printed when frame.pc > _func.entry. _func is
	//   generated by the linker.
	// - The +0x123 byte offset is not included with generated code, e.g. unnamed
	//   functions "func·006()" which is generally go func() { ... }()
	//   statements. Since the _func is generated at runtime, it's probably why
	//   _func.entry is not set.
	// - C calls may have fp=0x123 sp=0x123 appended. I think it normally happens
	//   when a signal is not correctly handled. It is printed with m.throwing>0.
	//   These are discarded.
	// - For cgo, the source file may be "??".
	reFile = regexp.MustCompile("^(?:\t| +)(\\?\\?|\\<autogenerated\\>|.+\\.(?:c|go|s))\\:(\\d+)(?:| \\+0x[0-9a-f]+)(?:| fp=0x[0-9a-f]+ sp=0x[0-9a-f]+)\r?\n$")
	// Sadly, it doesn't note the goroutine number so we could cascade them per
	// parenthood.
	reCreated = regexp.MustCompile("^created by (.+)\r?\n$")
	reFunc    = regexp.MustCompile("^(.+)\\((.*)\\)\r?\n$")
	reElided  = regexp.MustCompile("^\\.\\.\\.additional frames elided\\.\\.\\.\r?\n$")
)

func parseDump(r io.Reader, out io.Writer) ([]*Goroutine, error) {
	scanner := bufio.NewScanner(r)
	scanner.Split(scanLines)
	s := scanningState{}
	for scanner.Scan() {
		line, err := s.scan(scanner.Text())
		if line != "" {
			_, _ = io.WriteString(out, line)
		}
		if err != nil {
			return s.goroutines, err
		}
	}
	return s.goroutines, scanner.Err()
}

// scanLines is similar to bufio.ScanLines except that it:
//     - doesn't drop '\n'
//     - doesn't strip '\r'
//     - returns when the data is bufio.MaxScanTokenSize bytes
func scanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[0 : i+1], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	if len(data) >= bufio.MaxScanTokenSize {
		// Returns the line even if it is not at EOF nor has a '\n', otherwise the
		// scanner will return bufio.ErrTooLong which is definitely not what we
		// want.
		return len(data), data, nil
	}
	return 0, nil, nil
}

// state is the state of the scan to detect and process a stack trace.
type state int

// Initial state is normal. Other states are when a stack trace is detected.
const (
	// Outside a stack trace.
	// to: gotRoutineHeader when reRoutineHeader triggers
	normal state = iota

	// In between goroutines.
	betweenRoutine

	// Goroutine header was found, e.g. "goroutine 1 [running]:"
	// from: normal
	// to: gotUnavail, gotFunc
	gotRoutineHeader
	// Function call line was found, e.g. "main.main()"
	// from: gotRoutineHeader
	// to: gotFile
	gotFunc
	// Goroutine creation line was found, e.g. "created by main.glob..func4"
	// from: gotFileFunc
	// to: gotFileCreated
	gotCreated
	// File header was found, e.g. "\t/foo/bar/baz.go:116 +0x35"
	// from: gotFunc
	// to: gotFunc, gotCreated, betweenRoutine, normal
	gotFileFunc
	// File header was found, e.g. "\t/foo/bar/baz.go:116 +0x35"
	// from: gotCreated
	// to: betweenRoutine, normal
	gotFileCreated
	// State when the goroutine stack is instead is reUnavail.
	// from: gotRoutineHeader
	// to: betweenRoutine, gotCreated
	gotUnavail
)

// scanningState is the state of the scan to detect and process a stack trace
// and stores the traces found.
type scanningState struct {
	// goroutines contains all the goroutines found.
	goroutines []*Goroutine

	state state
}

// scan scans one line, updates goroutines and move to the next state.
func (s *scanningState) scan(line string) (string, error) {
	var cur *Goroutine
	if len(s.goroutines) != 0 {
		cur = s.goroutines[len(s.goroutines)-1]
	}
	switch s.state {
	case normal:
		// TODO(maruel): We could look for '^panic:' but this is more risky, there
		// can be a lot of junk between this and the stack dump.
		fallthrough
	case betweenRoutine:
		// Look for a goroutine header.
		if match := reRoutineHeader.FindStringSubmatch(line); match != nil {
			if id, err := strconv.Atoi(match[1]); err == nil {
				// See runtime/traceback.go.
				// "<state>, \d+ minutes, locked to thread"
				items := strings.Split(match[2], ", ")
				sleep := 0
				locked := false
				for i := 1; i < len(items); i++ {
					if items[i] == lockedToThread {
						locked = true
						continue
					}
					// Look for duration, if any.
					if match2 := reMinutes.FindStringSubmatch(items[i]); match2 != nil {
						sleep, _ = strconv.Atoi(match2[1])
					}
				}
				g := &Goroutine{
					Signature: Signature{
						State:    items[0],
						SleepMin: sleep,
						SleepMax: sleep,
						Locked:   locked,
					},
					ID:    id,
					First: len(s.goroutines) == 0,
				}
				s.goroutines = append(s.goroutines, g)
				s.state = gotRoutineHeader
				return "", nil
			}
		}
		// Fallthrough.
		s.state = normal
		return line, nil

	case gotRoutineHeader:
		if match := reUnavail.FindStringSubmatch(line); match != nil {
			// Generate a fake stack entry.
			cur.Stack.Calls = []Call{{SrcPath: "<unavailable>"}}
			// Next line is expected to be an empty line.
			s.state = gotUnavail
			return "", nil
		}
		call, err := parseFunc(line)
		if call != nil {
			cur.Stack.Calls = append(cur.Stack.Calls, *call)
			s.state = gotFunc
			return "", err
		}
		return "", fmt.Errorf("expected a function after a goroutine header, got: %q", strings.TrimSpace(line))

	case gotFunc:
		// Look for a file.
		if match := reFile.FindStringSubmatch(line); match != nil {
			num, err := strconv.Atoi(match[2])
			if err != nil {
				return "", fmt.Errorf("failed to parse int on line: %q", strings.TrimSpace(line))
			}
			// cur.Stack.Calls is guaranteed to have at least one item.
			i := len(cur.Stack.Calls) - 1
			cur.Stack.Calls[i].SrcPath = match[1]
			cur.Stack.Calls[i].Line = num
			s.state = gotFileFunc
			return "", nil
		}
		return "", fmt.Errorf("expected a file after a function, got: %q", strings.TrimSpace(line))

	case gotCreated:
		// Look for a file.
		if match := reFile.FindStringSubmatch(line); match != nil {
			num, err := strconv.Atoi(match[2])
			if err != nil {
				return "", fmt.Errorf("failed to parse int on line: %q", strings.TrimSpace(line))
			}
			cur.CreatedBy.SrcPath = match[1]
			cur.CreatedBy.Line = num
			s.state = gotFileCreated
			return "", nil
		}
		return "", fmt.Errorf("expected a file after a created line, got: %q", strings.TrimSpace(line))

	case gotFileFunc:
		if match := reCreated.FindStringSubmatch(line); match != nil {
			cur.CreatedBy.Func.Raw = match[1]
			s.state = gotCreated
			return "", nil
		}
		if match := reElided.FindStringSubmatch(line); match != nil {
			cur.Stack.Elided = true
			// TODO(maruel): New state.
			return "", nil
		}
		call, err := parseFunc(line)
		if call != nil {
			cur.Stack.Calls = append(cur.Stack.Calls, *call)
			s.state = gotFunc
			return "", err
		}
		if line == "\n" || line == "\r\n" {
			s.state = betweenRoutine
			return "", nil
		}
		// Back to normal state.
		s.state = normal
		return line, nil

	case gotFileCreated:
		if line == "\n" || line == "\r\n" {
			s.state = betweenRoutine
			return "", nil
		}
		s.state = normal
		return line, nil

	case gotUnavail:
		if line == "\n" || line == "\r\n" {
			s.state = betweenRoutine
			return "", nil
		}
		if match := reCreated.FindStringSubmatch(line); match != nil {
			cur.CreatedBy.Func.Raw = match[1]
			s.state = gotCreated
			return "", nil
		}
		return "", fmt.Errorf("expected empty line after unavailable stack, got: %q", strings.TrimSpace(line))
	default:
		return "", errors.New("internal error")
	}
}

// parseFunc only return an error if also returning a Call.
func parseFunc(line string) (*Call, error) {
	if match := reFunc.FindStringSubmatch(line); match != nil {
		call := &Call{Func: Func{Raw: match[1]}}
		for _, a := range strings.Split(match[2], ", ") {
			if a == "..." {
				call.Args.Elided = true
				continue
			}
			if a == "" {
				// Remaining values were dropped.
				break
			}
			v, err := strconv.ParseUint(a, 0, 64)
			if err != nil {
				return call, fmt.Errorf("failed to parse int on line: %q", strings.TrimSpace(line))
			}
			call.Args.Values = append(call.Args.Values, Arg{Value: v})
		}
		return call, nil
	}
	return nil, nil
}

// hasPathPrefix returns true if any of s is the prefix of p.
func hasPathPrefix(p string, s map[string]string) bool {
	for prefix := range s {
		if strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// getFiles returns all the source files deduped and ordered.
func getFiles(goroutines []*Goroutine) []string {
	files := map[string]struct{}{}
	for _, g := range goroutines {
		for _, c := range g.Stack.Calls {
			files[c.SrcPath] = struct{}{}
		}
	}
	out := make([]string, 0, len(files))
	for f := range files {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// splitPath splits a path into its components.
//
// The first item has its initial path separator kept.
func splitPath(p string) []string {
	if p == "" {
		return nil
	}
	var out []string
	s := ""
	for _, c := range p {
		if c != '/' || (len(out) == 0 && strings.Count(s, "/") == len(s)) {
			s += string(c)
		} else if s != "" {
			out = append(out, s)
			s = ""
		}
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// isFile returns true if the path is a valid file.
func isFile(p string) bool {
	// TODO(maruel): Is it faster to open the file or to stat it? Worth a perf
	// test on Windows.
	i, err := os.Stat(p)
	return err == nil && !i.IsDir()
}

// rootedIn returns a root if the file split in parts is rooted in root.
func rootedIn(root string, parts []string) string {
	//log.Printf("rootIn(%s, %v)", root, parts)
	for i := 1; i < len(parts); i++ {
		suffix := filepath.Join(parts[i:]...)
		if isFile(filepath.Join(root, suffix)) {
			return filepath.Join(parts[:i]...)
		}
	}
	return ""
}

// findRoots sets member GOROOT and GOPATHs.
func (c *Context) findRoots() {
	c.GOPATHs = map[string]string{}
	for _, f := range getFiles(c.Goroutines) {
		// TODO(maruel): Could a stack dump have mixed cases? I think it's
		// possible, need to confirm and handle.
		//log.Printf("  Analyzing %s", f)
		if c.GOROOT != "" && strings.HasPrefix(f, c.GOROOT+"/") {
			continue
		}
		if hasPathPrefix(f, c.GOPATHs) {
			continue
		}
		parts := splitPath(f)
		if c.GOROOT == "" {
			if r := rootedIn(c.localgoroot, parts); r != "" {
				c.GOROOT = r
				//log.Printf("Found GOROOT=%s", c.GOROOT)
				continue
			}
		}
		found := false
		for _, l := range c.localgopaths {
			if r := rootedIn(l, parts); r != "" {
				//log.Printf("Found GOPATH=%s", r)
				c.GOPATHs[r] = l
				found = true
				break
			}
		}
		if !found {
			// If the source is not found, just too bad.
			//log.Printf("Failed to find locally: %s / %s", f, goroot)
		}
	}
}

func getGOPATHs() []string {
	var out []string
	for _, v := range filepath.SplitList(os.Getenv("GOPATH")) {
		// Disallow non-absolute paths?
		if v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		homeDir := ""
		u, err := user.Current()
		if err != nil {
			homeDir = os.Getenv("HOME")
			if homeDir == "" {
				panic(fmt.Sprintf("Could not get current user or $HOME: %s\n", err.Error()))
			}
		} else {
			homeDir = u.HomeDir
		}
		out = []string{homeDir + "go"}
	}
	return out
}
