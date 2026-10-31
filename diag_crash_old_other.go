//go:build !go1.23 && !windows

package main

import "os"

func captureCrashOutput(f *os.File) {}
