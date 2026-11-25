package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Loopback only, through InternetOnly, so the tests never ask the firewall
// for anything.
func loopbackSettings() Settings {
	s := defaultSettings()
	s.InternetOnly = true
	return s
}

func TestFirstFreePortSkipsTakenOnes(t *testing.T) {
	s := loopbackSettings()
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	taken := held.Addr().(*net.TCPAddr).Port
	if taken >= 65534 {
		t.Skip("the system handed out a port at the very top of the range")
	}

	// The next port up is the internet listener's, so that is skipped too.
	s.PublicPort = taken + 1
	ln, port := firstFreePort(s, taken)
	if ln == nil {
		t.Fatal("no free port found")
	}
	defer ln.Close()
	if port == taken || port == taken+1 {
		t.Fatalf("picked port %d, which is taken or set aside (%d, %d)", port, taken, taken+1)
	}
	if port < taken {
		t.Fatalf("picked port %d, below where the search started (%d)", port, taken)
	}
}

func TestSavedPortLeavesTheRestOfTheFileAlone(t *testing.T) {
	dir := t.TempDir()
	oldPath, oldFile := configPath, fileSettings
	defer func() { configPath, fileSettings = oldPath, oldFile }()

	configPath = filepath.Join(dir, settingsFile)
	fileSettings = defaultSettings()
	fileSettings.Dir = `D:\drops`
	if err := savePort(8083); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	values, err := parseTOML(string(data))
	if err != nil {
		t.Fatal(err)
	}
	got := defaultSettings()
	if err := got.applyTOML(values); err != nil {
		t.Fatal(err)
	}
	if got.Port != 8083 || got.Dir != `D:\drops` {
		t.Fatalf("file has port %d and folder %q, want 8083 and D:\\drops", got.Port, got.Dir)
	}
}

func TestChildArgsCarryTheCorrectedPort(t *testing.T) {
	oldArgs, oldOverride := os.Args, portOverride
	defer func() { os.Args, portOverride = oldArgs, oldOverride }()

	os.Args = []string{"file-drop.exe", "-port", "9000", "-lan-only"}
	portOverride = 0
	if got := strings.Join(childArgs("-open-host=false"), " "); got != "-port 9000 -lan-only -open-host=false" {
		t.Fatalf("without an override: %q", got)
	}
	// The flag package takes the last -port, so appending is enough.
	portOverride = 9001
	if got := strings.Join(childArgs(), " "); got != "-port 9000 -lan-only -port 9001" {
		t.Fatalf("with an override: %q", got)
	}
}

func TestSecondStartFindsItselfRunning(t *testing.T) {
	oldPath, oldHolders := configPath, holdersOf
	defer func() { configPath, holdersOf = oldPath, oldHolders }()
	configPath = filepath.Join(t.TempDir(), settingsFile)
	holdersOf = func(int) []string { return nil }

	answer := func(config string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{"internet": false, "app": statusApp, "config": config})
		}))
	}
	portOf := func(s *httptest.Server) int { return s.Listener.Addr().(*net.TCPAddr).Port }

	same := answer(strings.ToUpper(configPath))
	defer same.Close()
	if !sameCopyRunning(portOf(same)) {
		t.Error("a File Drop with the same settings file was not recognised")
	}

	other := answer(filepath.Join(t.TempDir(), settingsFile))
	defer other.Close()
	if sameCopyRunning(portOf(other)) {
		t.Error("a File Drop with different settings was taken for this one")
	}

	stranger := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer stranger.Close()
	if sameCopyRunning(portOf(stranger)) {
		t.Error("some other web server was taken for File Drop")
	}
}

func TestSearchSkipsAPortOnlyLoopbackHolds(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.Addr().(*net.TCPAddr).Port
	if !loopbackAnswers(port) {
		t.Fatalf("port %d is listening on loopback but was not noticed", port)
	}
}
