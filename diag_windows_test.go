//go:build windows

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// The dependency probe under -log is only as good as its list, and the list is
// only right if it names every Windows function the source looks up. This
// keeps the two from drifting apart.
func TestDependencyListCoversTheSource(t *testing.T) {
	known := map[string]bool{}
	for _, fns := range dependencies {
		for _, fn := range fns {
			known[fn] = true
		}
	}
	files, _ := filepath.Glob("*.go")
	re := regexp.MustCompile(`NewProc\("([^"]+)"\)`)
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllSubmatch(src, -1) {
			if !known[string(m[1])] {
				t.Errorf("%s looks up %s, which the -log dependency probe in diag_windows.go does not list", f, m[1])
			}
		}
	}
}

func TestLogRequested(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"-log"}, true},
		{[]string{"--log"}, true},
		{[]string{"-port", "9000", "--log"}, true},
		{[]string{"-log=true"}, true},
		{[]string{"-log=false"}, false},
		{[]string{"-log", "-log=false"}, false},
		{[]string{"-logs"}, false},
		{[]string{"-log-watch", "12:34"}, false},
		{[]string{"--", "-log"}, false},
	}
	for _, c := range cases {
		if got := logRequested(c.args); got != c.want {
			t.Errorf("logRequested(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestExplainExitNamesTheCause(t *testing.T) {
	if lines := explainExit(1, "cannot listen on port 8080", true); len(lines) < 2 || lines[0] != "It said it was going to: cannot listen on port 8080" {
		t.Errorf("an intended exit should quote its reason, got %q", lines)
	}
	if lines := explainExit(1, "", false); len(lines) < 2 || !regexp.MustCompile(`TerminateProcess`).MatchString(lines[1]) {
		t.Errorf("an unannounced exit code 1 should point at TerminateProcess, got %q", lines)
	}
	if lines := explainExit(0xFFFFFFFF, "", false); !regexp.MustCompile(`TerminateProcess`).MatchString(lines[1]) {
		t.Errorf("an unannounced exit code -1 is a forced kill, not a Windows error, got %q", lines)
	}
	if lines := explainExit(0xC0000005, "", false); !regexp.MustCompile(`ACCESS_VIOLATION`).MatchString(lines[1]) {
		t.Errorf("0xC0000005 should be named, got %q", lines)
	}
}
