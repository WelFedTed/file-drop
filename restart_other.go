//go:build !windows

package main

import "errors"

// Restarting relies on the Windows job-object arrangement that keeps cloudflared
// tied to this process's lifetime, so the button is offered only where that
// exists. Elsewhere, stop and start the program yourself.
const restartSupported = false

func relaunch(args []string) error {
	return errors.New("restarting from the page is only supported on Windows")
}
