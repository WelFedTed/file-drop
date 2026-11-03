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

const detachedProcess = 0x00000008

const detachSupported = true

// canDetach reports whether startDetached would hand over: not in the copy
// without a console, and not when there was no console to get away from.
func canDetach() bool {
	return os.Getenv(detachedEnv) != "1" && consoleWindow() != 0
}

// startDetached launches the console-less copy. The bool reports whether this
// process has handed over and should now exit.
func startDetached() (bool, error) {
	if !canDetach() {
		return false, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return false, err
	}
	cmd := exec.Command(exe, childArgs()...)
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
	tracef("the copy without a console is pid %d", cmd.Process.Pid)
	// Nothing waits on it: this process is about to end, and the new one is
	// deliberately no longer a child of anything.
	go cmd.Wait()
	return true, nil
}
