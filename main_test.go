package main

// Tests for the handful of things here that are easy to get wrong and hard to
// notice: what a file name is allowed to become, what the settings file can
// survive a round trip through, who is allowed to post, and what a batch leaves
// on disk.

import (
	"net/http"
	"net/http/httptest"
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

// The two sizes that get multiplied up into bytes cannot be given a value that
// overflows on the way, which would quietly turn a limit into no limit.
func TestSizesCannotOverflow(t *testing.T) {
	s := defaultSettings()
	s.MaxMB = 1 << 55
	if err := s.normalise(); err == nil {
		t.Error("an overflowing largest batch was accepted")
	}
	s = defaultSettings()
	s.MinFreeMB = 1 << 55
	if err := s.normalise(); err == nil {
		t.Error("an overflowing reserve was accepted")
	}
}

// Nothing that changes anything answers to another website.
func TestCrossSitePostsRefused(t *testing.T) {
	handler := sameOrigin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	cases := []struct {
		what    string
		method  string
		headers map[string]string
		want    int
	}{
		{"the page itself", "POST", map[string]string{"Sec-Fetch-Site": "same-origin"}, 204},
		{"another site", "POST", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"another port on this machine", "POST", map[string]string{"Sec-Fetch-Site": "same-site"}, 403},
		{"typed into the address bar", "POST", map[string]string{"Sec-Fetch-Site": "none"}, 403},
		{"an older browser, our page", "POST", map[string]string{"Origin": "http://localhost:8080"}, 204},
		{"an older browser, elsewhere", "POST", map[string]string{"Origin": "https://example.invalid"}, 403},
		{"not a browser at all", "POST", nil, 204},
		{"reading, from anywhere", "GET", map[string]string{"Sec-Fetch-Site": "cross-site"}, 204},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://localhost:8080/purge", nil)
		for k, v := range c.headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.what, w.Code, c.want)
		}
	}
}
