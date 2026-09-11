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
