//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// Elsewhere -log keeps the log itself - every step, every error, the crash
// trace and the record of meaning to exit - but not the watcher or the survey,
// which are built on Windows' own facilities.

func platformHeader(exe string) {}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(syscall.Signal(0)) == nil
}

func startWatcher(survey bool) error { return errors.New("not available on this platform") }

func runWatcher(arg string) {}

func explainPort(port int) {}

func portHolders(port int) []string { return nil }

func portReservations(port int) []string { return nil }

func portRefused(err error) bool { return errors.Is(err, syscall.EACCES) }
