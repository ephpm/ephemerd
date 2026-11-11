package runnerversion

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Corroboration: a zero-work verdict says a runner got no job; the runner's
// own _diag log can say WHY. When GitHub refuses a runner for being below
// its minimum version the Listener logs
//
//	Runner version v2.337.0 is deprecated and cannot receive messages.
//
// The detector does not require this (a floor bump could change the text),
// but a hit lets the alert name the cause.

// deprecationMarkers are matched case-insensitively against each line.
var deprecationMarkers = []string{
	"deprecated and cannot receive messages",
	"runner version is deprecated",
	"minimum runner version",
}

// DiagHit is a corroborating line from a runner diag log.
type DiagHit struct {
	File string
	Line string
}

// diagTailBytes bounds how much of each log is read (the refusal is logged
// near the end, right before exit).
const diagTailBytes = 256 << 10

// ScanDiag looks through <runnerDir>/_diag/Runner_*.log for a deprecation
// marker. Returns the first hit. A missing _diag directory is not an error
// (dispatched runners keep theirs in the guest), just no hit.
func ScanDiag(runnerDir string) (DiagHit, bool, error) {
	diagDir := filepath.Join(runnerDir, "_diag")
	entries, err := os.ReadDir(diagDir)
	if errors.Is(err, os.ErrNotExist) {
		return DiagHit{}, false, nil
	}
	if err != nil {
		return DiagHit{}, false, fmt.Errorf("reading %s: %w", diagDir, err)
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, "Runner_") && strings.HasSuffix(n, ".log") {
			names = append(names, n)
		}
	}
	// Newest first: the names carry a timestamp.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names {
		p := filepath.Join(diagDir, n)
		hit, ok, err := scanTail(p)
		if err != nil {
			return DiagHit{}, false, err
		}
		if ok {
			return DiagHit{File: p, Line: hit}, true, nil
		}
	}
	return DiagHit{}, false, nil
}

func scanTail(path string) (hit string, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	st, err := f.Stat()
	if err != nil {
		return "", false, err
	}
	if st.Size() > diagTailBytes {
		if _, err := f.Seek(st.Size()-diagTailBytes, io.SeekStart); err != nil {
			return "", false, err
		}
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if MatchesDeprecation(line) {
			return strings.TrimSpace(line), true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", false, err
	}
	return "", false, nil
}

// MatchesDeprecation reports whether a log line carries a refusal marker.
func MatchesDeprecation(line string) bool {
	l := strings.ToLower(line)
	for _, m := range deprecationMarkers {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}
