//go:build !go1.23 && windows

package main

import (
	"os"
	"syscall"
)

// captureCrashOutput, for the toolchain the Windows 7 and 8 build uses, which
// predates debug.SetCrashOutput. The runtime writes a crash to whatever the
// standard error handle is at that moment - it asks Windows afresh each time -
// so pointing that handle at the log puts the trace there.
//
// The cost is that a crash is then not also shown on screen. Everything else
// is: the log package keeps writing to the console it started with, because
// os.Stderr is a copy of the handle taken at start-up and does not move.
func captureCrashOutput(f *os.File) {
	setStdHandle := syscall.NewLazyDLL("kernel32.dll").NewProc("SetStdHandle")
	stderr := int32(syscall.STD_ERROR_HANDLE) // negative, so not a constant uintptr
	setStdHandle.Call(uintptr(stderr), f.Fd())
}
