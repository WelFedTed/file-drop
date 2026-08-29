//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// The firewall check behind the button at the top of the settings panel.
//
// "Is this reachable from the network?" cannot be answered from inside this
// process - only a second machine could really tell us - so what is checked is
// the thing that goes wrong in practice: Windows Defender Firewall having no
// inbound allow rule for this program, because the prompt on first run was
// dismissed or never appeared.
//
// The work is done by PowerShell rather than by parsing netsh, whose output is
// translated into the user's language and would need re-parsing per locale. The
// Get-NetFirewall* cmdlets hand back objects, and the script below reduces them
// to one line of JSON.

// firewallSupported lets the panel decide whether to show the row at all,
// without paying for a check that shells out to PowerShell.
const firewallSupported = true

// firewallRuleName is what the rule this adds is called. It is also the name
// the README's manual netsh command uses, so a rule added by hand that way is
// the same rule rather than a second one beside it.
const firewallRuleName = "File Drop Server"

// firewallReport is what the panel is told. Profiles is a comma-separated list
// rather than a JSON array because Windows PowerShell 5.1 collapses a
// single-element array to a scalar on the way through ConvertTo-Json, which
// would leave the field's type depending on how many networks are connected.
type firewallReport struct {
	Supported  bool   `json:"supported"`
	Allowed    bool   `json:"allowed"`
	How        string `json:"how"`
	Profiles   string `json:"profiles"`
	FirewallOn bool   `json:"firewall_on"`
	Error      string `json:"error,omitempty"`
}

// psQuote wraps a value for a PowerShell single-quoted string, where the only
// escape that exists is a doubled quote.
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func runPowerShell(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	return cmd.Output()
}

// firewallStatus reports whether an enabled inbound allow rule covers this
// program, or the port it is on, for the networks currently connected.
func firewallStatus(port int) firewallReport {
	exe, err := os.Executable()
	if err != nil {
		return firewallReport{Supported: true, Error: "could not work out this program's own path"}
	}

	// A rule scoped to Domain does nothing on a Private network, so the profiles
	// currently in use are needed to judge the rules that were found. A port
	// rule written as a range ("8000-9000") is not recognised, and the check
	// then says blocked when it is not: the cost of that is one redundant rule.
	script := fmt.Sprintf(`
$ErrorActionPreference = 'SilentlyContinue'
$port = %d
$exe  = %s

$cats = @()
foreach ($p in @(Get-NetConnectionProfile)) {
  $c = [string]$p.NetworkCategory
  if ($c -eq 'DomainAuthenticated') { $c = 'Domain' }
  if ($c) { $cats += $c }
}
$cats = @($cats | Sort-Object -Unique)
if ($cats.Count -eq 0) { $cats = @('Private') }

$fwOn = $false
foreach ($p in @(Get-NetFirewallProfile)) {
  if (($cats -contains [string]$p.Name) -and $p.Enabled) { $fwOn = $true }
}

function Covers($rule) {
  $prof = [string]$rule.Profile
  if ($prof -eq 'Any') { return $true }
  foreach ($c in $cats) { if ($prof -like ('*' + $c + '*')) { return $true } }
  return $false
}

function Opens($rule) {
  return ([string]$rule.Direction -eq 'Inbound') -and
         ([string]$rule.Action -eq 'Allow') -and
         ([string]$rule.Enabled -eq 'True') -and
         (Covers $rule)
}

$allowed = $false
$how = ''

foreach ($r in @(Get-NetFirewallApplicationFilter -Program $exe | Get-NetFirewallRule)) {
  if (Opens $r) { $allowed = $true; $how = 'program' }
}

if (-not $allowed) {
  $filters = @(Get-NetFirewallPortFilter | Where-Object {
    ([string]$_.Protocol -eq 'TCP') -and
    ((@($_.LocalPort) -contains [string]$port) -or (@($_.LocalPort) -contains 'Any'))
  })
  foreach ($f in $filters) {
    foreach ($r in @($f | Get-NetFirewallRule)) {
      if (Opens $r) { $allowed = $true; $how = 'port' }
    }
  }
}

if (-not $fwOn) { $allowed = $true; $how = 'firewall-off' }

[pscustomobject]@{
  supported   = $true
  allowed     = $allowed
  how         = $how
  profiles    = [string]::Join(',', $cats)
  firewall_on = $fwOn
} | ConvertTo-Json -Compress
`, port, psQuote(exe))

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	out, err := runPowerShell(ctx, script)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return firewallReport{Supported: true, Error: "Windows took too long to answer"}
		}
		return firewallReport{Supported: true, Error: "could not ask Windows about the firewall: " + err.Error()}
	}

	var report firewallReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &report); err != nil {
		return firewallReport{Supported: true, Error: "could not make sense of what Windows reported"}
	}
	report.Supported = true
	return report
}

// firewallAllow adds the inbound rule, which needs administrator rights this
// process does not have and should not be started with. Start-Process -Verb
// RunAs raises the consent prompt instead, so the elevation is the operator's
// own deliberate click on their own desktop, and lasts only for the one netsh
// command rather than for the life of the server.
//
// The rule is written against this program rather than against the port, so it
// keeps working when the port setting changes, and grants nothing else the
// right to listen. Public networks are left out on purpose: this is a tool for
// a network you trust, and widening it to Public is a decision to be taken
// deliberately in Windows, not as a side effect of clicking Fix.
func firewallAllow(port int) error {
	exe, err := os.Executable()
	if err != nil {
		return errors.New("could not work out this program's own path")
	}

	args := fmt.Sprintf(`advfirewall firewall add rule name="%s" dir=in action=allow program="%s" enable=yes profile=private,domain`,
		firewallRuleName, exe)

	script := fmt.Sprintf(`
try {
  $p = Start-Process -FilePath 'netsh.exe' -ArgumentList %s -Verb RunAs -Wait -PassThru -WindowStyle Hidden -ErrorAction Stop
  exit $p.ExitCode
} catch {
  exit 5
}
`, psQuote(args))

	// Long enough for someone to notice the consent prompt and answer it.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	if _, err := runPowerShell(ctx, script); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 5 {
			return errors.New("the administrator prompt was dismissed, so nothing was changed")
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return errors.New("the administrator prompt went unanswered, so nothing was changed")
		}
		return errors.New("Windows would not add the rule: " + err.Error())
	}
	return nil
}
