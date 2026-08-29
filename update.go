package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Updating in place, from the releases published on GitHub.
//
// Every release carries two assets: the executable, always under the same name
// so that neither this code nor anyone's shortcut has to know the version, and
// a checksums file listing its SHA-256. The download is only ever written to
// disk after it has been hashed and matched against that list, and a release
// without the list is refused rather than trusted - a build that cannot be
// checked is not one to replace a working program with.
const (
	releaseAsset  = "file-drop.exe"
	checksumAsset = "checksums.txt"

	// Long enough for a slow line, short enough that nothing hangs on it: the
	// check runs in the background at start-up and nothing waits for it.
	updateCheckTimeout = 20 * time.Second
	updateFetchTimeout = 10 * time.Minute
)

// updateState is what /host and the update panel are told. It is written by the
// background check at start-up and by the download, and read by requests, so it
// travels behind a lock.
type updateState struct {
	Checked   bool   `json:"checked"`
	Available bool   `json:"available"`
	Ready     bool   `json:"ready"`
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	URL       string `json:"url"`
	Notes     string `json:"notes"`
	Error     string `json:"error,omitempty"`
}

var (
	updateMu  sync.RWMutex
	updateNow = updateState{Current: version}
)

func currentUpdateState() updateState {
	updateMu.RLock()
	defer updateMu.RUnlock()
	return updateNow
}

func setUpdateState(s updateState) {
	updateMu.Lock()
	updateNow = s
	updateMu.Unlock()
}

// release is the slice of GitHub's release JSON this needs.
type release struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

func (r release) asset(name string) (string, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a.URL, true
		}
	}
	return "", false
}

func updateHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

func fetch(url string, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// GitHub turns away requests without one.
	req.Header.Set("User-Agent", "file-drop/"+version)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := updateHTTPClient(timeout).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	// Generous, but bounded: an answer far larger than any release we publish
	// is a sign of something other than what was asked for.
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

func latestRelease() (release, error) {
	var r release
	body, err := fetch("https://api.github.com/repos/"+updateRepo+"/releases/latest", updateCheckTimeout)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return r, errors.New("could not make sense of what GitHub returned")
	}
	if r.TagName == "" {
		return r, errors.New("GitHub reported no released version")
	}
	return r, nil
}

// checkForUpdate runs in the background at start-up. Being unable to reach
// GitHub is not worth interrupting anyone over: the note is kept for the panel
// and the server carries on regardless.
func checkForUpdate() {
	state := updateState{Checked: true, Current: version}

	r, err := latestRelease()
	if err != nil {
		state.Error = err.Error()
		setUpdateState(state)
		log.Printf("could not check for updates: %v", err)
		return
	}

	state.Latest = strings.TrimPrefix(r.TagName, "v")
	state.URL = r.HTMLURL
	state.Notes = firstLines(r.Body, 12)

	newer, err := newerVersion(version, r.TagName)
	if err != nil {
		state.Error = err.Error()
		setUpdateState(state)
		return
	}
	state.Available = newer
	setUpdateState(state)

	if newer {
		log.Printf("version %s is available; this is %s", state.Latest, version)
	}
}

// downloadUpdate fetches the new executable, checks it against the checksums
// published beside it, and puts it in place. The running program is not
// replaced so much as moved aside: Windows will not let a running image be
// overwritten, but it will let it be renamed.
func downloadUpdate() error {
	r, err := latestRelease()
	if err != nil {
		return err
	}
	newer, err := newerVersion(version, r.TagName)
	if err != nil {
		return err
	}
	if !newer {
		return errors.New("this is already the newest version")
	}

	binURL, ok := r.asset(releaseAsset)
	if !ok {
		return fmt.Errorf("release %s has no %s to download", r.TagName, releaseAsset)
	}
	sumURL, ok := r.asset(checksumAsset)
	if !ok {
		return fmt.Errorf("release %s publishes no %s, so the download cannot be checked", r.TagName, checksumAsset)
	}

	sums, err := fetch(sumURL, updateCheckTimeout)
	if err != nil {
		return fmt.Errorf("could not fetch %s: %v", checksumAsset, err)
	}
	want, err := checksumFor(string(sums), releaseAsset)
	if err != nil {
		return err
	}

	payload, err := fetch(binURL, updateFetchTimeout)
	if err != nil {
		return fmt.Errorf("could not download the update: %v", err)
	}

	got := sha256.Sum256(payload)
	if hex.EncodeToString(got[:]) != want {
		// Nothing has been written yet, so there is nothing to undo.
		return errors.New("the download did not match its published checksum and was thrown away")
	}

	if err := replaceExecutable(payload); err != nil {
		return err
	}
	log.Printf("updated to %s; restart to run it", strings.TrimPrefix(r.TagName, "v"))
	return nil
}

// checksumFor pulls one entry out of a "<sha256>  <name>" listing.
func checksumFor(list, name string) (string, error) {
	for _, line := range strings.Split(list, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		// The second field may carry the "*" that some tools write for a
		// binary-mode hash.
		if strings.EqualFold(strings.TrimPrefix(fields[1], "*"), name) {
			sum := strings.ToLower(fields[0])
			if len(sum) != 64 {
				return "", fmt.Errorf("the checksum listed for %s is not a SHA-256", name)
			}
			if _, err := hex.DecodeString(sum); err != nil {
				return "", fmt.Errorf("the checksum listed for %s is not readable", name)
			}
			return sum, nil
		}
	}
	return "", fmt.Errorf("%s lists no checksum for %s", checksumAsset, name)
}

// replaceExecutable puts the new build where the running one is, keeping the
// same file name so shortcuts, firewall rules and the settings file beside it
// all still refer to the right thing.
func replaceExecutable(payload []byte) error {
	exe, err := os.Executable()
	if err != nil {
		return errors.New("could not work out this program's own path")
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}

	staged := exe + ".new"
	if err := os.WriteFile(staged, payload, 0o755); err != nil {
		return fmt.Errorf("could not write the update next to the program: %v", err)
	}

	previous := exe + ".old"
	os.Remove(previous)
	if err := os.Rename(exe, previous); err != nil {
		os.Remove(staged)
		return fmt.Errorf("could not move the running program aside: %v", err)
	}
	if err := os.Rename(staged, exe); err != nil {
		// Put the working program back rather than leaving a gap where it was.
		os.Rename(previous, exe)
		os.Remove(staged)
		return fmt.Errorf("could not put the update in place: %v", err)
	}
	return nil
}

// tidyPreviousUpdate clears the copy left behind by the last update. It cannot
// be deleted at the time, because it is the program doing the deleting.
//
// After an update the copy is usually still running when its replacement
// starts - it spawns us and exits milliseconds later - and Windows will not
// delete a file that is a running image. So this keeps trying quietly for a
// short while rather than giving up on the first refusal. Failing entirely
// costs nothing: the next ordinary start clears it, when nothing holds it.
func tidyPreviousUpdate() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	previous := exe + ".old"
	if os.Remove(previous) == nil {
		return
	}
	go func() {
		for i := 0; i < 10; i++ {
			time.Sleep(time.Second)
			if _, err := os.Stat(previous); err != nil {
				return
			}
			if os.Remove(previous) == nil {
				return
			}
		}
	}()
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	if len(lines) > n {
		lines = append(lines[:n], "…")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
