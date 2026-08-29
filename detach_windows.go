//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// Starting without a console window.
//
// The obvious way to do this is to hide the console after start-up, and on
// Windows 10 that works. On Windows 11, where Windows Terminal is the default
// terminal application, it does not: the window a program gets from
// GetConsoleWindow is a pseudo-console standing in for a terminal that belongs
// to another process entirely, and hiding it hides nothing anyone can see. The
// visible window is Windows Terminal's, and it may well be showing other tabs,
// so it is not ours to touch.
//
// What works everywhere is to not have a console in the first place. When the
// program is asked to start hidden it launches one more copy of itself with no
// console attached and steps aside, leaving the tray icon as the only way in -
// which is what was asked for.

const (
	detachedProcess = 0x00000008

	// Set on the copy that has no console, so a spawn that somehow keeps one
	// cannot turn into a program that starts itself for ever.
	detachedEnv = "FILE_DROP_DETACHED"
)

const detachSupported = true

// startDetached launches the console-less copy. The bool reports whether this
// process has handed over and should now exit.
func startDetached() (bool, error) {
	if os.Getenv(detachedEnv) == "1" {
		return false, nil // this is the copy without a console
	}
	if consoleWindow() == 0 {
		return false, nil // there was nothing to get away from
	}

	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Dir, _ = os.Getwd()
	cmd.Env = append(os.Environ(), detachedEnv+"=1")
	// No console, and out of the job object that would otherwise take the new
	// copy down with this one a moment from now.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createBreakawayFromJob,
	}
	if err := cmd.Start(); err != nil {
		return false, err
	}
	// Nothing waits on it: this process is about to end, and the new one is
	// deliberately no longer a child of anything.
	go cmd.Wait()
	return true, nil
}
