//go:build !windows

package main

import "errors"

// winget is the only install route offered, so the button appears on Windows
// only. Elsewhere the tile still explains what is missing.
const cloudflaredInstallSupported = false

func installCloudflared() error {
	return errors.New("installing cloudflared from the page is only supported on Windows")
}
