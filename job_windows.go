//go:build windows

package main

import (
	"log"
	"syscall"
	"unsafe"
)

// Windows job-object plumbing. Assigning ourselves to a job whose processes are
// killed when the job closes means anything we spawn - cloudflared in
// particular - dies with us however we go: Ctrl+C, a crash, or Task Manager.
// Without this an orphaned tunnel would keep a public address alive pointing at
// a port that something else could later claim.

const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000

	// Permitting breakaway is what lets the Restart button work. Everything we
	// spawn is meant to die with us, which is the whole point of the job - but
	// the replacement copy of ourselves has to survive the exit that follows it
	// by milliseconds, and without this the job would take it down too. A child
	// only escapes by asking, with CREATE_BREAKAWAY_FROM_JOB, so cloudflared and
	// everything else stay bound to our lifetime as before.
	jobObjectLimitBreakawayOK = 0x0800
)

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobObjectExtendedLimitInfo struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// jobHandle is kept for the life of the process: closing it would trigger the
// very kill we are arming.
var jobHandle syscall.Handle

func adoptChildProcesses() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	createJob := kernel32.NewProc("CreateJobObjectW")
	setJobInfo := kernel32.NewProc("SetInformationJobObject")
	assignJob := kernel32.NewProc("AssignProcessToJobObject")

	handle, _, err := createJob.Call(0, 0)
	if handle == 0 {
		log.Printf("note: could not create a job object (%v); a tunnel may outlive a hard kill", err)
		return
	}

	info := jobObjectExtendedLimitInfo{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose | jobObjectLimitBreakawayOK
	ok, _, err := setJobInfo.Call(handle, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if ok == 0 {
		log.Printf("note: could not configure the job object (%v)", err)
		syscall.CloseHandle(syscall.Handle(handle))
		return
	}

	current, _, _ := kernel32.NewProc("GetCurrentProcess").Call()
	if ok, _, err := assignJob.Call(handle, current); ok == 0 {
		log.Printf("note: could not join the job object (%v)", err)
		syscall.CloseHandle(syscall.Handle(handle))
		return
	}

	jobHandle = syscall.Handle(handle)
}
