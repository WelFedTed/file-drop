package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Diagnostics, switched on with -log.
//
// The question this answers is "why did it stop?", and the awkward cases are
// the ones where the program cannot say so itself. Started from Explorer, every
// error File Drop reports goes into a console window that closes the instant
// the program ends, so an ordinary "that port is in use" looks exactly like a
// crash. Killed by antivirus, it gets no chance to write anything at all.
//
// So three things work together, all writing to one file:
//
//   - This process logs what it is doing as it does it: every step of start-up,
//     everything about the machine that could matter, every error, and - when
//     it means to stop - that it is about to, and why. A Go panic or runtime
//     crash is sent to the file as well as the screen.
//
//   - A watcher: a second copy of the program, with no window, whose only job
//     is to wait for this one to end and record how. Windows hands it the exit
//     code whatever happened, and an exit code from a process that never said
//     it was leaving is the fingerprint of being killed from outside.
//
//   - A survey, run once by the first watcher while the program gets on with
//     starting: which security products are installed and enforcing what, and
//     what Windows' own logs say about this executable - antivirus detections,
//     blocked launches, Application Error crash reports.
//
// None of it runs without -log. With it, start-up is not delayed: the slow
// parts belong to the watcher.

const (
	logFileName = "file-drop.log"

	// Kept small enough to attach to an email or paste into an issue. A file
	// grown past this is moved aside at the next start, not truncated mid-run.
	logRotateBytes = 8 << 20

	// Every copy started from the first - the watcher, the console-less copy
	// of -start-hidden, the replacement after a restart or an update - writes
	// to the same file, which is found through these rather than worked out
	// again by each.
	logPathEnv     = "FILE_DROP_LOG_PATH"
	logSurveyedEnv = "FILE_DROP_LOG_SURVEYED"

	heartbeatEvery = 5 * time.Minute

	// Set on the copy -start-hidden starts with no console (detach_windows.go),
	// so a spawn that somehow keeps one cannot turn into a program that starts
	// itself for ever - and so the log can say which copy is which.
	detachedEnv = "FILE_DROP_DETACHED"
)

var (
	diagOn   bool
	diagRole = "main"
	diagFile *os.File
	diagPath string
	diagMu   sync.Mutex
	diagT0   = time.Now()
)

// logRequested reads -log straight out of the command line. The flag is also
// declared normally, but this runs before flag.Parse, because a mistyped flag
// is itself one of the ways the program stops before it has said anything.
func logRequested(args []string) bool {
	on := false
	for _, a := range args {
		if a == "--" {
			break
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || name != "log" {
			continue
		}
		if !hasValue {
			on = true
			continue
		}
		b, err := strconv.ParseBool(value)
		on = err == nil && b
	}
	return on
}

// watchTarget reports whether this process is a watcher, and if so the
// argument naming what it watches. It is never typed by anyone; startWatcher
// adds it.
func watchTarget(args []string) (string, bool) {
	for i, a := range args {
		if a == "-log-watch" && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// startDiagnostics is the first thing main does. It returns the watcher's
// target when this process is a watcher, which then does nothing else.
func startDiagnostics() (watch string, isWatcher bool) {
	watch, isWatcher = watchTarget(os.Args[1:])
	if !isWatcher && !logRequested(os.Args[1:]) {
		return "", false
	}
	diagOn = true

	switch {
	case isWatcher:
		diagRole = "watcher"
	case os.Getenv(detachedEnv) == "1":
		diagRole = "hidden"
	}

	if err := openLog(); err != nil {
		// Nowhere to write is worth saying where it can be seen, and is no
		// reason to refuse to run.
		fmt.Fprintf(os.Stderr, "\n  -log: could not open a log file anywhere (%v); carrying on without one.\n", err)
		diagOn = false
		return watch, isWatcher
	}

	// A crash prints every goroutine rather than only the one that failed:
	// with a deadlock or a stuck tray thread, the culprit is usually another.
	debug.SetTraceback("all")
	captureCrashOutput(diagFile)

	if isWatcher {
		return watch, true
	}

	// The log package is what every error in the program already goes
	// through, so pointing it at the file as well catches all of them.
	log.SetOutput(io.MultiWriter(os.Stderr, diagLine{}))

	if os.Getenv(logPathEnv) == "" {
		fmt.Fprintf(os.Stderr, "\n  Writing a diagnostic log to %s\n", diagPath)
	}
	os.Setenv(logPathEnv, diagPath)

	tracef("==== %s %s started, pid %d ====", filepath.Base(exePath()), version, os.Getpid())
	reportUnfinishedRuns()
	writeHeader()

	// The first copy has the survey done for the whole run; copies it starts
	// later only need watching.
	survey := os.Getenv(logSurveyedEnv) == ""
	if err := startWatcher(survey); err != nil {
		tracef("could not start the watcher (%v): if this process is killed, nothing will record how", err)
	} else if survey {
		os.Setenv(logSurveyedEnv, "1")
	}

	go heartbeat()
	return "", false
}

// openLog finds somewhere writable, preferring beside the executable because
// that is where anyone would look. A program run from a folder it cannot write
// to - Program Files, a protected Desktop - falls back to the user's own.
func openLog() error {
	var candidates []string
	if p := os.Getenv(logPathEnv); p != "" {
		candidates = append(candidates, p)
	}
	candidates = append(candidates, filepath.Join(filepath.Dir(exePath()), logFileName))
	if d, err := os.UserCacheDir(); err == nil {
		candidates = append(candidates, filepath.Join(d, "FileDrop", logFileName))
	}
	candidates = append(candidates, filepath.Join(os.TempDir(), logFileName))

	var firstErr error
	for _, p := range candidates {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if fi, err := os.Stat(p); err == nil && fi.Size() > logRotateBytes && os.Getenv(logPathEnv) == "" {
			os.Remove(p + ".old")
			os.Rename(p, p+".old")
		}
		// Appending, which on Windows makes every write land at the end
		// whatever else has the file open: several copies of this program
		// share it, and none may write over another's lines.
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		diagFile, diagPath = f, p
		return nil
	}
	return firstErr
}

// diagLine is the file's half of the log package's output: the same message,
// stamped with the full date, the process and which copy it is.
type diagLine struct{}

func (diagLine) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	// The log package's own time prefix is redundant beside ours.
	if len(msg) > 9 && msg[2] == ':' && msg[5] == ':' && msg[8] == ' ' {
		msg = msg[9:]
	}
	writeLine(msg)
	return len(p), nil
}

func writeLine(msg string) {
	if diagFile == nil {
		return
	}
	stamp := time.Now().Format("2006-01-02 15:04:05.000")
	var b strings.Builder
	for i, line := range strings.Split(msg, "\n") {
		if i == 0 {
			fmt.Fprintf(&b, "%s [%d %s] %s\r\n", stamp, os.Getpid(), diagRole, line)
		} else {
			fmt.Fprintf(&b, "%s [%d %s]     %s\r\n", stamp, os.Getpid(), diagRole, line)
		}
	}
	diagMu.Lock()
	diagFile.WriteString(b.String())
	diagMu.Unlock()
}

// tracef is a line that only exists with -log: a step taken, a fact found.
// Anything worth saying without -log goes through the log package instead.
func tracef(format string, args ...any) {
	if diagOn {
		writeLine(fmt.Sprintf(format, args...))
	}
}

// step marks the start of something that could be where it goes wrong. When
// the program dies without a word, the last step in the log is where it was.
func step(format string, args ...any) {
	if diagOn {
		writeLine("step: " + fmt.Sprintf(format, args...))
	}
}

// noteExit is written immediately before the program ends itself, however it
// does it. The watcher looks for it: an exit with no such line before it was
// not this program's decision.
func noteExit(code int, why string) {
	if diagOn {
		writeLine(fmt.Sprintf("exit-intent pid=%d code=%d: %s", os.Getpid(), code, why))
	}
}

// fatalf is log.Fatalf, with the intent to exit recorded first.
func fatalf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	noteExit(1, msg)
	log.Fatal(msg)
}

// exitNow is os.Exit, with the intent recorded first.
func exitNow(code int, why string) {
	noteExit(code, why)
	os.Exit(code)
}

func exePath() string {
	exe, err := os.Executable()
	if err != nil {
		return os.Args[0]
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		return r
	}
	return exe
}

func writeHeader() {
	exe := exePath()
	wd, _ := os.Getwd()
	build := "standard build"
	if legacyBuild() {
		build = "Windows 7 and 8 build"
	}
	tracef("version %s (%s), %s, %s/%s", version, build, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	tracef("command line: %q", os.Args)
	tracef("executable: %s", exe)
	tracef("working folder: %s", wd)
	tracef("log file: %s", diagPath)
	if fi, err := os.Stat(exe); err == nil {
		tracef("executable size %d bytes, modified %s", fi.Size(), fi.ModTime().Format(time.RFC3339))
	}
	if sum, err := fileSHA256(exe); err == nil {
		// Compare with checksums.txt on the release page: a different value
		// means a damaged or altered download, not the build that was published.
		tracef("executable sha256: %s", sum)
	}
	platformHeader(exe)
	logNetwork()
}

// logSettings records what the program is about to run with. The access code
// for internet uploads is a password in all but name, so it is not copied into
// a file meant to be sent to someone.
func logSettings(cfg Settings) {
	if !diagOn {
		return
	}
	if cfg.Token != "" {
		cfg.Token = "(set, not shown)"
	}
	from := configPath + " (not written yet - these are the defaults and the command line)"
	if configFound.Load() {
		from = configPath
	}
	tracef("settings from %s: %+v", from, cfg)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// logNetwork lists every adapter, because "no active network adapter with an
// IPv4 address" is one of the ways start-up ends, and the list is what shows
// why - an adapter that is down, or only has an IPv6 or 169.254 address.
func logNetwork() {
	ifaces, err := net.Interfaces()
	if err != nil {
		tracef("network: could not list adapters: %v", err)
		return
	}
	for _, iface := range ifaces {
		var addrs []string
		if as, err := iface.Addrs(); err == nil {
			for _, a := range as {
				addrs = append(addrs, a.String())
			}
		}
		state := "down"
		if iface.Flags&net.FlagUp != 0 {
			state = "up"
		}
		tracef("network: %q %s %v", iface.Name, state, addrs)
	}
}

// heartbeat shows how long the program lived and whether it was growing. A
// crash after hours with memory climbing all the while is a leak; one at a
// steady size is something else.
func heartbeat() {
	for range time.Tick(heartbeatEvery) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		tracef("alive for %s: %d goroutines, %s in use, %s from the system",
			time.Since(diagT0).Round(time.Second), runtime.NumGoroutine(),
			humanSize(int64(m.HeapAlloc)), humanSize(int64(m.Sys)))
	}
}

// reportUnfinishedRuns reads back the end of the log for copies that started
// and then vanished: no exit-intent, and no watcher report either. That last
// part matters - the watcher records even a hard kill, so a run with nothing
// from either was taken down together with its watcher, which is what security
// software does when it removes every copy of a file it has decided against.
func reportUnfinishedRuns() {
	tail, err := readTail(diagPath, 2<<20)
	if err != nil {
		return
	}
	started := map[int]string{}
	var order []int
	ended := map[int]bool{}

	sc := bufio.NewScanner(bytes.NewReader(tail))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "==== "); i >= 0 && strings.Contains(line, " started, pid ") {
			if pid := lineInt(line, " started, pid "); pid > 0 && pid != os.Getpid() {
				if _, seen := started[pid]; !seen {
					order = append(order, pid)
				}
				started[pid] = strings.TrimSpace(line[:23])
			}
		}
		if pid := lineInt(line, "exit-intent pid="); pid > 0 {
			ended[pid] = true
		}
		if pid := lineInt(line, "watch-report pid="); pid > 0 {
			ended[pid] = true
		}
	}

	for _, pid := range order {
		if ended[pid] || processAlive(pid) {
			continue
		}
		tracef("EARLIER RUN UNACCOUNTED FOR: pid %d, started %s, left no record of ending - neither its own nor"+
			" its watcher's. Both copies of the program went at once. That is almost always security software"+
			" removing every running copy of a file it has flagged (look for its quarantine or history list), or"+
			" the computer losing power or being reset.", pid, started[pid])
	}
}

func lineInt(line, after string) int {
	i := strings.Index(line, after)
	if i < 0 {
		return 0
	}
	rest := line[i+len(after):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(rest[:end])
	return n
}

func readTail(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := fi.Size() - max
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

// intentFor finds what a process said, if anything, before it ended.
func intentFor(pid int) (string, bool) {
	tail, err := readTail(diagPath, 4<<20)
	if err != nil {
		return "", false
	}
	key := fmt.Sprintf("exit-intent pid=%d ", pid)
	found := ""
	ok := false
	for _, line := range strings.Split(string(tail), "\n") {
		if i := strings.Index(line, key); i >= 0 {
			found = strings.TrimSpace(line[i+len(key):])
			// "code=1: cannot listen..." - the code is reported separately.
			if _, rest, cut := strings.Cut(found, ": "); cut && strings.HasPrefix(found, "code=") {
				found = rest
			}
			ok = true
		}
	}
	return found, ok
}
