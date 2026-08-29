//go:build !windows

package main

import "errors"

// The firewall check is Windows Defender Firewall specific. Elsewhere the panel
// is told it is unsupported and leaves the button off the page, rather than
// guessing at whichever of ufw, firewalld or pf might be in front of it.
type firewallReport struct {
	Supported  bool   `json:"supported"`
	Allowed    bool   `json:"allowed"`
	How        string `json:"how"`
	Profiles   string `json:"profiles"`
	FirewallOn bool   `json:"firewall_on"`
	Error      string `json:"error,omitempty"`
}

const firewallSupported = false

func firewallStatus(port int) firewallReport {
	return firewallReport{}
}

func firewallAllow(port int) error {
	return errors.New("the firewall check only works on Windows")
}
