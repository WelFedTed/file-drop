//go:build windows

package main

import (
	"errors"
	"syscall"
	"unsafe"
)

// Free space on the drop volume. A phone can hand over more than the disk can
// hold, and the failure that follows is otherwise a truncated file and a raw
// "there is not enough space on the disk" from the middle of a copy. Knowing
// the number up front turns that into a sentence the sender can act on.

// freeSpace reports the bytes still available on the volume holding path, to
// this user - quotas included, which is why it is GetDiskFreeSpaceEx and not
// the total free space on the disk.
//
// The second result is false when the question could not be answered at all: a
// path on a disconnected drive, say. Callers treat that as "do not check"
// rather than "no room", because refusing every upload over a failed query
// would be worse than the disk-full error it is trying to pre-empt.
func freeSpace(path string) (uint64, bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, false
	}
	var free, total, totalFree uint64
	getFree := syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")
	ok, _, _ := getFree.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&free)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if ok == 0 {
		return 0, false
	}
	return free, true
}

const (
	errorHandleDiskFull = syscall.Errno(39)  // ERROR_HANDLE_DISK_FULL
	errorDiskFull       = syscall.Errno(112) // ERROR_DISK_FULL
)

// isDiskFull picks the out-of-space failure out of everything else a write can
// go wrong with, so the upload page can say which one it was.
func isDiskFull(err error) bool {
	return errors.Is(err, errorHandleDiskFull) ||
		errors.Is(err, errorDiskFull) ||
		errors.Is(err, syscall.ENOSPC)
}
