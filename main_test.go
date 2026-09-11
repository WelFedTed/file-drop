package main

// Tests for the handful of things here that are easy to get wrong and hard to
// notice: what a file name is allowed to become, what the settings file can
// survive a round trip through, who is allowed to post, and what a batch leaves
// on disk.

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// The CRC-32 of "hello", which every batch fixture below sends.
const helloCRC = "3610a686"

func settingsFor(t *testing.T, root string) {
	t.Helper()
	s := defaultSettings()
	s.Dir, s.Open, s.Notify = root, false, false
	if err := s.normalise(); err != nil {
		t.Fatal(err)
	}
	setSettings(s)
}

// batchBody builds a multipart body in the shape the upload page sends: the
// path and checksum fields immediately before each file, and a "copy" entry
// naming a file already in the batch rather than sending its bytes a second
// time.
func batchBody(files []string, copies map[string]int) (*bytes.Buffer, string) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, path := range files {
		w.WriteField("path", path)
		w.WriteField("crc", helloCRC)
		fw, _ := w.CreateFormFile("files", filepath.Base(path))
		fw.Write([]byte("hello"))
	}
	for path, at := range copies {
		w.WriteField("path", path)
		w.WriteField("crc", helloCRC)
		w.WriteField("copy", strconv.Itoa(at))
	}
	w.Close()
	return &body, w.FormDataContentType()
}

func postBatch(body *bytes.Buffer, contentType string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "http://localhost:8080/upload", body)
	r.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	handleUpload(rec, r)
	return rec
}

// A whole batch: a loose file, and a second that is a copy of it rather than a
// second trip over the wire.
func TestUploadBatch(t *testing.T) {
	root := t.TempDir()
	settingsFor(t, root)

	body, contentType := batchBody(
		[]string{"hello.txt"},
		map[string]int{"again/hello.txt": 1},
	)
	rec := postBatch(body, contentType)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["count"].(float64) != 2 || out["verified"].(float64) != 2 || out["copied"].(float64) != 1 {
		t.Fatalf("unexpected tallies: %v", out)
	}

	dir := filepath.Join(root, out["folder"].(string))
	if got, _ := os.ReadFile(filepath.Join(dir, "again", "hello.txt")); string(got) != "hello" {
		t.Fatalf("the copy holds %q", got)
	}
	// The copy inherits the checksum the server read off its own write, so both
	// lines carry it.
	sfv, _ := os.ReadFile(filepath.Join(dir, sfvName))
	if strings.Count(string(sfv), helloCRC) != 2 {
		t.Fatalf("the manifest does not name both files:\n%s", sfv)
	}
}

// A batch is all or nothing: a folder left on disk is always a complete one.
func TestCorruptBatchLeavesNothing(t *testing.T) {
	root := t.TempDir()
	settingsFor(t, root)

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("path", "hello.txt")
	w.WriteField("crc", "deadbeef") // not what is about to arrive
	fw, _ := w.CreateFormFile("files", "hello.txt")
	fw.Write([]byte("hello"))
	w.Close()

	if rec := postBatch(&body, w.FormDataContentType()); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d", rec.Code)
	}
	if left, _ := os.ReadDir(root); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

// Batches arriving while /host polls. What is remembered about a drop folder
// must never be what it held half way through being written.
func TestArrivalsWhilePolling(t *testing.T) {
	root := t.TempDir()
	settingsFor(t, root)

	stop := make(chan struct{})
	var pollers sync.WaitGroup
	for i := 0; i < 4; i++ {
		pollers.Add(1)
		go func() {
			defer pollers.Done()
			for {
				select {
				case <-stop:
					return
				default:
					activeUploads()
					recentBatches(root, 10)
				}
			}
		}()
	}

	const batches = 12
	var uploads sync.WaitGroup
	for i := 0; i < batches; i++ {
		uploads.Add(1)
		go func(n int) {
			defer uploads.Done()
			body, contentType := batchBody(
				[]string{"one.bin"},
				map[string]int{"two.bin": 1},
			)
			if rec := postBatch(body, contentType); rec.Code != http.StatusOK {
				t.Errorf("upload %d: %d %s", n, rec.Code, rec.Body.String())
			}
		}(i)
	}
	uploads.Wait()
	close(stop)
	pollers.Wait()

	listed, total, totalBytes := recentBatches(root, 50)
	if total != batches {
		t.Fatalf("expected %d batches, got %d", batches, total)
	}
	if want := int64(batches * 2 * len("hello")); totalBytes != want {
		t.Fatalf("the drop root is reported as %d bytes, want %d", totalBytes, want)
	}
	for _, b := range listed {
		if b.Files != 2 {
			t.Fatalf("%s is remembered as %d file(s), want 2", b.Folder, b.Files)
		}
	}
}

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
