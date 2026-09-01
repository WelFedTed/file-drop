//go:build windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// cloudflaredInstallSupported gates the install button on /host: a Windows-only
// affair either way.
const cloudflaredInstallSupported = true

// cloudflaredMSI is Cloudflare's own installer, and the route the Windows 7 and
// 8 build takes. winget does not exist on those versions of Windows, so the
// button that offers to install the tunnel client would otherwise be a button
// that always fails. The URL is Cloudflare's "latest" redirect, so it does not
// have to be revised every time they publish.
const cloudflaredMSI = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-windows-amd64.msi"

// installCloudflared puts the tunnel client on this machine, elevating for the
// install the same way the firewall button does: the server keeps running
// unprivileged and the consent prompt covers the one command.
func installCloudflared() error {
	if legacyBuild() {
		return installCloudflaredFromMSI()
	}
	return installCloudflaredWithWinget()
}

func installCloudflaredWithWinget() error {
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

// installCloudflaredFromMSI downloads Cloudflare's installer and hands it to
// msiexec behind the administrator prompt.
//
// Running an installer fetched over the network deserves more care than a
// package manager needs, so this does three things before elevating: it takes
// the file from Cloudflare's own release URL over HTTPS and nowhere else, it
// refuses anything that is not actually an MSI, and it reads the Authenticode
// signature and refuses anything not signed by Cloudflare. The consent prompt
// then names the publisher as well, which is the last word: whoever is at the
// machine sees who signed it before agreeing to run it.
func installCloudflaredFromMSI() error {
	path, err := downloadMSI(cloudflaredMSI)
	if err != nil {
		return err
	}
	// Kept until msiexec has finished with it, and no longer.
	defer os.Remove(path)

	if err := checkSignedByCloudflare(path); err != nil {
		return err
	}

	script := fmt.Sprintf(`
try {
  $p = Start-Process -FilePath 'msiexec.exe' -ArgumentList '/i', %s, '/qn', '/norestart' -Verb RunAs -Wait -PassThru -ErrorAction Stop
  exit $p.ExitCode
} catch {
  exit 5
}
`, psQuote(path))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	_, runErr := runPowerShell(ctx, script)

	// As with winget: the installer writes the PATH to the registry, which this
	// process cannot see in its own inherited copy.
	refreshPathFromRegistry()

	// And as with winget, the question is whether the client is there now.
	if _, err := cloudflaredPath(); err == nil {
		return nil
	}

	if runErr != nil {
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			switch exit.ExitCode() {
			case 5, 1602:
				// 5 is our own marker for a dismissed prompt; 1602 is msiexec's
				// for an install the user stopped.
				return errors.New("the install was cancelled, so nothing was changed")
			case 3010:
				return errors.New("cloudflared installed, but Windows wants a restart before it can be used")
			}
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("the install did not finish in ten minutes and was given up on")
		}
		return errors.New("the installer did not complete: " + runErr.Error())
	}
	return errors.New("cloudflared installed but still cannot be found on the PATH - a sign-out or reboot may be needed")
}

// downloadMSI fetches the installer to a temporary file and satisfies itself
// that what came back is one.
func downloadMSI(url string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("could not reach Cloudflare to download the installer: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Cloudflare answered %s when asked for the installer", resp.Status)
	}

	// Comfortably more than the installer has ever been, and a limit all the
	// same: this is written to disk before anything has vouched for it.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return "", fmt.Errorf("the download was interrupted: %v", err)
	}
	// Every MSI is an OLE compound file and starts with the same eight bytes.
	if len(payload) < 8 || !bytes.Equal(payload[:8], []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}) {
		return "", errors.New("what came back from that address was not an installer, so it was thrown away")
	}

	file, err := os.CreateTemp("", "cloudflared-*.msi")
	if err != nil {
		return "", fmt.Errorf("could not make room for the download: %v", err)
	}
	name := file.Name()
	if _, err := file.Write(payload); err != nil {
		file.Close()
		os.Remove(name)
		return "", fmt.Errorf("could not save the download: %v", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("could not save the download: %v", err)
	}
	return name, nil
}

// checkSignedByCloudflare refuses an installer that does not carry Cloudflare's
// signature.
//
// It judges on who signed the file rather than on whether Windows can validate
// the chain, because these are old machines: one that has not seen an update in
// years may well lack the roots to check a current certificate, and refusing
// Cloudflare's own installer over that would help nobody. An unsigned file, or
// one signed by somebody else, is refused outright - and Windows makes its own
// judgement at the elevation prompt either way.
func checkSignedByCloudflare(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	out, err := runPowerShell(ctx, fmt.Sprintf(
		`$s = Get-AuthenticodeSignature -FilePath %s; "$($s.Status)|$($s.SignerCertificate.Subject)"`,
		psQuote(path)))
	if err != nil {
		return errors.New("could not check who signed the installer, so it was not run")
	}

	report := strings.TrimSpace(string(out))
	status, subject, _ := strings.Cut(report, "|")
	if !strings.Contains(strings.ToLower(subject), "cloudflare") {
		if strings.EqualFold(strings.TrimSpace(status), "NotSigned") {
			return errors.New("that installer carries no signature at all, so it was thrown away")
		}
		return errors.New("that installer is not signed by Cloudflare, so it was thrown away")
	}
	if !strings.EqualFold(strings.TrimSpace(status), "Valid") {
		// Signed by them, but this machine cannot confirm the chain. Say so and
		// let the elevation prompt put the same question to whoever is there.
		log.Printf("note: the installer is signed by Cloudflare, but Windows reports its signature as %q - "+
			"check the publisher named in the administrator prompt", status)
	}
	return nil
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
