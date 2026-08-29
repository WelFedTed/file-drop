//go:build !windows

package main

import (
	"log"
	"os/exec"
	"runtime"
)

// openFolder only does anything on Windows, which is where this server runs.
func openFolder(dir string) {}

// openBrowser shows the operator's own screen at start-up. Unlike the Explorer
// window, this one is worth having anywhere the program will build.
func openBrowser(url string) {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	cmd := exec.Command(opener, url)
	if err := cmd.Start(); err != nil {
		log.Printf("could not open %s in a browser: %v", url, err)
		return
	}
	go cmd.Wait()
}
