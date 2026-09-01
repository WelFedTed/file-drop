//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// cloudflaredInstallSupported gates the install button on /host. winget is the
// only route offered, so this is a Windows-only affair.
const cloudflaredInstallSupported = true

// installCloudflared fetches the tunnel client with winget, elevating for the
// install the same way the firewall button does: the server keeps running
// unprivileged and the consent prompt covers the one command.
func installCloudflared() error {
	if _, err := exec.LookPath("winget"); err != nil {
		return errors.New("winget is not available on this machine - install cloudflared yourself from cloudflare.com")
	}

	args := "install --id Cloudflare.cloudflared --exact --source winget " +
		"--accept-source-agreements --accept-package-agreements --disable-interactivity"

	script := fmt.Sprintf(`
try {
  $p = Start-Process -FilePath 'winget.exe' -ArgumentList %s -Verb RunAs -Wait -PassThru -ErrorAction Stop
  exit $p.ExitCode
} catch {
  exit 5
}
`, psQuote(args))

	// Downloading and installing can take a while on a slow line, and the
	// consent prompt sits in front of it waiting to be answered.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	_, runErr := runPowerShell(ctx, script)

	// An installer that puts itself on the PATH changes the registry, not this
	// process: our copy was inherited at start-up and will never see the new
	// entry. Nor would the replacement a restart spawns, since it inherits ours
	// in turn - so the freshly installed client would still look missing after
	// restarting. Re-reading the registry here fixes both at once.
	refreshPathFromRegistry()

	// What matters is whether the client is there now, not what winget's exit
	// code said. It reports a failure when the package is already present and
	// has no upgrade, which is not a failure of the thing being asked for.
	if _, err := cloudflaredPath(); err == nil {
		return nil
	}

	if runErr != nil {
		var exit *exec.ExitError
		if errors.As(runErr, &exit) && exit.ExitCode() == 5 {
			return errors.New("the administrator prompt was dismissed, so nothing was installed")
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("the install did not finish in ten minutes and was given up on")
		}
		return errors.New("winget could not install cloudflared: " + runErr.Error())
	}
	return errors.New("cloudflared installed but still cannot be found on the PATH - a sign-out or reboot may be needed")
}

// refreshExecPathOnce re-reads the PATH from the registry at most once for the
// life of the process, for the case where something was installed before this
// program started but after the PATH it inherited was handed to it.
var refreshExecPathOnce = onceFunc(refreshPathFromRegistry)

// refreshPathFromRegistry rebuilds PATH from where Windows actually keeps it,
// rather than from what this process was handed when it started.
func refreshPathFromRegistry() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := runPowerShell(ctx,
		`[Environment]::GetEnvironmentVariable('Path','Machine') + ';' + [Environment]::GetEnvironmentVariable('Path','User')`)
	if err != nil {
		return
	}
	if path := strings.TrimSpace(string(out)); path != "" {
		os.Setenv("PATH", path)
	}
}
