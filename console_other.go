//go:build !windows

package main

import "os"

// useUTF8Console is a no-op outside Windows, where consoles are UTF-8 already.
func useUTF8Console() {}

func stdinIsConsole() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// A terminal elsewhere does not close because the program in it ended.
func consoleClosesWithUs() bool { return false }

func inRed(print func()) {
	os.Stdout.WriteString("\x1b[1;31m")
	print()
	os.Stdout.WriteString("\x1b[0m")
}
