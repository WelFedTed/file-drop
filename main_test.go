package main

// Tests for the handful of things here that are easy to get wrong and hard to
// notice: what a file name is allowed to become, what the settings file can
// survive a round trip through, who is allowed to post, and what a batch leaves
// on disk.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A name too long for Windows is cut down, but never through the middle of a
// character: the file would land under a name the manifest no longer matches.
func TestLongNamesStayReadable(t *testing.T) {
	for _, r := range []string{"日", "é", "a"} {
		got := safeSegment(strings.Repeat(r, 300) + ".jpg")
		switch {
		case !utf8.ValidString(got):
			t.Errorf("%q produced invalid UTF-8", r)
		case len(got) > 180:
			t.Errorf("%q produced %d bytes", r, len(got))
		case !strings.HasSuffix(got, ".jpg"):
			t.Errorf("%q lost its extension: %q", r, got)
		}
	}
	// A cut landing on a dot must not leave a name Windows would rewrite.
	got := safeSegment(strings.Repeat("a", 175) + "." + strings.Repeat("a", 40) + ".txt")
	if strings.HasSuffix(strings.TrimSuffix(got, ".txt"), ".") {
		t.Errorf("a trailing dot survived the cut: %q", got)
	}
}

// Whatever the settings panel can save, the next start has to be able to read.
func TestSettingsSurviveTheFile(t *testing.T) {
	s := defaultSettings()
	s.Dir = "C:\\drop\u0001café"
	s.Token = "ab\u0007cd"
	s.Host = "日本"

	values, err := parseTOML(string(s.toTOML()))
	if err != nil {
		t.Fatalf("what toTOML wrote, parseTOML would not read: %v", err)
	}
	back := defaultSettings()
	if err := back.applyTOML(values); err != nil {
		t.Fatal(err)
	}
	if back.Dir != s.Dir || back.Token != s.Token || back.Host != s.Host {
		t.Fatalf("came back as %q / %q / %q", back.Dir, back.Token, back.Host)
	}
}

// The escapes that are not ours are still refused, so a hand-edited file with a
// typo in it says so rather than starting on a value nobody wrote.
func TestNonsenseEscapesRefused(t *testing.T) {
	for _, bad := range []string{
		`"a\q"`, `"a\u12"`, `"a\uZZZZ"`, `"a\UFFFFFFFF"`, `"a\`,
	} {
		if _, err := parseTOMLValue(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}
