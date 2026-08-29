//go:build !windows

package main

import "errors"

// The notification area is a Windows idea, and this program is a Windows
// program; elsewhere it stays a plain console server.

const traySupported = false

func startTray(hostURL string, quit func()) error {
	return errors.New("the tray icon is only available on Windows")
}

func notifyArrival(summary, folder string) {}

func hideConsole() error {
	return errors.New("hiding the console window is only available on Windows")
}
