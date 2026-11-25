//go:build windows

package main

import (
	"log"
	"os/exec"
	"syscall"
)

// Explorer and the browser are the operator's, not ours. Everything this
// program starts is otherwise held in its job object and killed when it exits
// - see job_windows.go - and a browser that was not already running when File
// Drop opened a page in it would be started inside that job, and closed along
// with File Drop. So both are started outside it.
func startOutsideJob(name string, args ...string) (*exec.Cmd, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createBreakawayFromJob}
	if err := cmd.Start(); err == nil {
		return cmd, nil
	}
	// Refused when something above us runs this program in a job of its own
	// that does not allow it; starting inside the job is still better than not
	// starting at all.
	cmd = exec.Command(name, args...)
	return cmd, cmd.Start()
}

// openFolder pops up a Windows Explorer window on a freshly received batch.
func openFolder(dir string) {
	cmd, err := startOutsideJob("explorer.exe", dir)
	if err != nil {
		log.Printf("could not open Explorer for %s: %v", dir, err)
		return
	}
	// explorer.exe hands off to the running shell and exits straight away, and
	// it reports a non-zero status even when it worked - so reap the process
	// without treating the exit code as a failure.
	go cmd.Wait()
}

// openBrowser shows the operator's own screen at start-up, in whatever browser
// they have set as the default. url.dll is what the shell itself uses to follow
// a link, so it needs no console window and no quoting games with cmd /c start.
func openBrowser(url string) {
	cmd, err := startOutsideJob("rundll32.exe", "url.dll,FileProtocolHandler", url)
	if err != nil {
		log.Printf("could not open %s in a browser: %v", url, err)
		return
	}
	go cmd.Wait()
}
