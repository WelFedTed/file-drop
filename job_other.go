//go:build !windows

package main

// adoptChildProcesses is a Windows-only safeguard; elsewhere the shell cleans up.
func adoptChildProcesses() {}
