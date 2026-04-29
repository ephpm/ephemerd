package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

const launchdPlist = "/Library/LaunchDaemons/dev.ephpm.ephemerd.plist"

func serviceAction(action string) error {
	switch action {
	case "start":
		out, err := exec.Command("launchctl", "load", "-w", launchdPlist).CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl load: %s", out)
		}
	case "stop":
		out, err := exec.Command("launchctl", "unload", launchdPlist).CombinedOutput()
		if err != nil {
			return fmt.Errorf("launchctl unload: %s", out)
		}
	default:
		return fmt.Errorf("unsupported action: %s", action)
	}
	fmt.Printf("ephemerd %sed\n", action)
	return nil
}

const logFile = "/var/log/ephemerd.log"

func serviceLogs(lines int, follow bool) error {
	f, err := os.Open(logFile)
	if err != nil {
		return fmt.Errorf("opening %s: %w", logFile, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "warning: closing log file: %v\n", cerr)
		}
	}()

	// Read last N lines by scanning backward from end of file.
	if err := printLastLines(f, lines); err != nil {
		return err
	}

	if !follow {
		return nil
	}

	// Follow: poll for new data.
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			fmt.Print(line)
		}
		if err != nil {
			if err != io.EOF {
				return fmt.Errorf("reading log: %w", err)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// printLastLines prints the last n lines from the file.
func printLastLines(f *os.File, n int) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat log file: %w", err)
	}
	size := info.Size()
	if size == 0 {
		return nil
	}

	// Read backward to find n newlines.
	buf := make([]byte, 0, 8192)
	found := 0
	offset := size
	for offset > 0 && found <= n {
		readSize := int64(8192)
		if readSize > offset {
			readSize = offset
		}
		offset -= readSize
		chunk := make([]byte, readSize)
		if _, err := f.ReadAt(chunk, offset); err != nil {
			return fmt.Errorf("reading log: %w", err)
		}
		buf = append(chunk, buf...)
		for _, b := range chunk {
			if b == '\n' {
				found++
			}
		}
	}

	// Trim to last n lines.
	lineCount := 0
	start := len(buf)
	for i := len(buf) - 1; i >= 0; i-- {
		if buf[i] == '\n' {
			lineCount++
			if lineCount == n+1 {
				start = i + 1
				break
			}
		}
	}
	if lineCount <= n {
		start = 0
	}

	os.Stdout.Write(buf[start:])

	// Seek to end for follow mode.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seeking to end: %w", err)
	}
	return nil
}
