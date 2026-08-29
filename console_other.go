//go:build !windows

package main

// useUTF8Console is a no-op outside Windows, where consoles are UTF-8 already.
func useUTF8Console() {}
