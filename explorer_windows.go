//go:build windows

package main

import (
	"log"
	"os/exec"
)

// openFolder pops up a Windows Explorer window on a freshly received batch.
func openFolder(dir string) {
	cmd := exec.Command("explorer.exe", dir)
	if err := cmd.Start(); err != nil {
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
	cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url)
	if err := cmd.Start(); err != nil {
		log.Printf("could not open %s in a browser: %v", url, err)
		return
	}
	go cmd.Wait()
}
