//go:build !windows

package main

// A console window is a Windows idea, and so is starting without one.

const detachSupported = false

func startDetached() (bool, error) { return false, nil }
