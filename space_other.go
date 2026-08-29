//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// freeSpace is answered by a Windows API this build has no equivalent of to
// hand, so the guard simply does not run here.
func freeSpace(path string) (uint64, bool) { return 0, false }

func isDiskFull(err error) bool { return errors.Is(err, syscall.ENOSPC) }
