//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	consoleKernel32 = syscall.NewLazyDLL("kernel32.dll")

	procSetConsoleOutputCP         = consoleKernel32.NewProc("SetConsoleOutputCP")
	procGetConsoleScreenBufferInfo = consoleKernel32.NewProc("GetConsoleScreenBufferInfo")
	procSetConsoleTextAttribute    = consoleKernel32.NewProc("SetConsoleTextAttribute")
)

// useUTF8Console switches the console to code page 65001 so the QR code drawn
// with block characters at start-up renders instead of turning into mojibake.
func useUTF8Console() {
	procSetConsoleOutputCP.Call(65001)
}

// stdinIsConsole reports whether there is someone at a keyboard to ask: false
// for the copy started without a console, and when input is redirected.
func stdinIsConsole() bool {
	var mode uint32
	return syscall.GetConsoleMode(syscall.Stdin, &mode) == nil
}

// consoleClosesWithUs reports whether the console window goes away the moment
// this program ends - true when it was opened for us, by a double-click or a
// shortcut, rather than being a Command Prompt that carries on afterwards.
func consoleClosesWithUs() bool {
	var pids [4]uint32
	n, _, _ := procGetConsoleProcs.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

// consoleScreenBufferInfo is CONSOLE_SCREEN_BUFFER_INFO.
type consoleScreenBufferInfo struct {
	size, cursor [2]int16
	attributes   uint16
	window       [4]int16
	maxSize      [2]int16
}

// inRed runs print with the console's text in bright red, putting the colour
// back afterwards. Where the output is not a console it simply prints.
func inRed(print func()) {
	var info consoleScreenBufferInfo
	ok, _, _ := procGetConsoleScreenBufferInfo.Call(uintptr(syscall.Stdout), uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		print()
		return
	}
	const foregroundRed, foregroundIntensity = 0x0004, 0x0008
	background := info.attributes & 0x00F0
	procSetConsoleTextAttribute.Call(uintptr(syscall.Stdout), uintptr(background|foregroundRed|foregroundIntensity))
	print()
	procSetConsoleTextAttribute.Call(uintptr(syscall.Stdout), uintptr(info.attributes))
}
