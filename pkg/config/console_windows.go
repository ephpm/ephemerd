//go:build windows

package config

import (
	"os"

	"golang.org/x/sys/windows"
)

func init() {
	var mode uint32
	h := windows.Handle(os.Stderr.Fd())
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return // not a console (piped/redirected), skip
	}
	_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}
