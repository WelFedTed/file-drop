//go:build go1.23

package main

import (
	"os"
	"runtime/debug"
)

// captureCrashOutput sends an unrecovered panic or a fatal runtime error to the
// log as well as the screen. This is the trace that says which line failed,
// and without -log it is printed to a console window that is gone a moment
// later.
func captureCrashOutput(f *os.File) {
	debug.SetCrashOutput(f, debug.CrashOptions{})
}
