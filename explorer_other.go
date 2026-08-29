//go:build !windows

package main

// openFolder only does anything on Windows, which is where this server runs.
func openFolder(dir string) {}
