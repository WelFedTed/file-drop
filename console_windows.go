//go:build windows

package main

import "syscall"

// useUTF8Console switches the console to code page 65001 so the QR code drawn
// with block characters at start-up renders instead of turning into mojibake.
func useUTF8Console() {
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleOutputCP")
	proc.Call(65001)
}
