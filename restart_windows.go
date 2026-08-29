//go:build windows

package main

import (
	"log"
	"os"
	"os/exec"
	"syscall"
)

// createBreakawayFromJob starts a child outside this process's job object. The
// job exists so that anything we spawn dies with us - cloudflared above all -
// and the replacement started by a restart is the one thing that must not.
// See job_windows.go, which has to allow breakaway for this to be permitted.
const createBreakawayFromJob = 0x01000000

const restartSupported = true

// relaunch hands Windows a fresh copy of this program and returns as soon as it
// has started. The console handles are inherited, so the new banner appears in
// the same terminal the old one was printed to.
func relaunch(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	start := func(flags uint32) (*exec.Cmd, error) {
		cmd := exec.Command(exe, args...)
		cmd.Dir, _ = os.Getwd()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
		return cmd, cmd.Start()
	}

	cmd, err := start(createBreakawayFromJob)
	if err != nil {
		// Breakaway is refused when something above us runs this program inside
		// a job of its own that does not permit it - a task runner, or a shell
		// that groups its children. Falling back to an ordinary start is worth
		// trying: it only fails if that outer job also kills on close.
		log.Printf("could not start outside the job object (%v); trying without", err)
		cmd, err = start(0)
		if err != nil {
			return err
		}
	}
	log.Printf("replacement started, pid %d", cmd.Process.Pid)
	return nil
}
