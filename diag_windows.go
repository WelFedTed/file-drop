//go:build windows

package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	diagKernel32 = syscall.NewLazyDLL("kernel32.dll")
	diagNtdll    = syscall.NewLazyDLL("ntdll.dll")

	procRtlGetVersion        = diagNtdll.NewProc("RtlGetVersion")
	procGlobalMemoryStatusEx = diagKernel32.NewProc("GlobalMemoryStatusEx")
	procGetDriveType         = diagKernel32.NewProc("GetDriveTypeW")
	procProcessIdToSessionId = diagKernel32.NewProc("ProcessIdToSessionId")
	procGetACP               = diagKernel32.NewProc("GetACP")
	procGetModuleFileName    = diagKernel32.NewProc("GetModuleFileNameW")
	procQueryFullImageName   = diagKernel32.NewProc("QueryFullProcessImageNameW")
	procModule32First        = diagKernel32.NewProc("Module32FirstW")
	procModule32Next         = diagKernel32.NewProc("Module32NextW")
)

const (
	processQueryLimited = 0x1000
	synchronize         = 0x00100000
	stillActive         = 259
	createNoWindow      = 0x08000000
	th32csSnapModule    = 0x00000008
	th32csSnapModule32  = 0x00000010
	tokenElevationClass = 20
)

// dependencies is every DLL function this program looks up at run time, by
// DLL. Windows resolves these only when they are first called, so a missing
// one is not a start-up failure but a crash at the moment it is needed - the
// tray icon, say, a few hundred milliseconds in. Probing them all up front
// names the culprit. main_test.go keeps this list honest against the source.
var dependencies = map[string][]string{
	"kernel32.dll": {
		"AssignProcessToJobObject", "CreateJobObjectW", "GetACP", "GetConsoleProcessList", "GetConsoleScreenBufferInfo",
		"GetConsoleWindow", "GetCurrentProcess", "GetDiskFreeSpaceExW", "GetDriveTypeW",
		"GetModuleFileNameW", "GetModuleHandleW", "GlobalMemoryStatusEx", "Module32FirstW", "Module32NextW",
		"ProcessIdToSessionId", "QueryFullProcessImageNameW", "SetConsoleOutputCP",
		"SetConsoleTextAttribute", "SetInformationJobObject", "SetStdHandle",
	},
	"ntdll.dll":   {"RtlGetVersion"},
	"shell32.dll": {"Shell_NotifyIconW"},
	"user32.dll": {
		"AppendMenuW", "CreateIconFromResourceEx", "CreatePopupMenu", "CreateWindowExW",
		"DefWindowProcW", "DestroyMenu", "DestroyWindow", "DispatchMessageW", "GetClassNameW",
		"GetCursorPos", "GetMessageW", "GetSystemMetrics", "IsWindowVisible", "PostMessageW",
		"PostQuitMessage", "RegisterClassW", "RegisterWindowMessageW", "SetForegroundWindow",
		"SetMenuDefaultItem", "ShowWindow", "TrackPopupMenu", "TranslateMessage",
	},
}

func systemRoot() string {
	if r := os.Getenv("SystemRoot"); r != "" {
		return r
	}
	return `C:\Windows`
}

func system32(name string) string { return filepath.Join(systemRoot(), "System32", name) }

func platformHeader(exe string) {
	logWindowsVersion()

	self := os.Getpid()
	procs := processList()
	if me, ok := procs[uint32(self)]; ok {
		parent := procs[me.parent]
		pp := imagePath(me.parent)
		if pp == "" {
			pp = parent.name
		}
		if pp == "" {
			pp = "(no longer running)"
		}
		// explorer.exe means a double-click; cmd.exe or a terminal means typed;
		// file-drop.exe means a copy this program started itself.
		tracef("started by: pid %d %s", me.parent, pp)
	}
	base := strings.ToLower(filepath.Base(exe))
	var others []string
	for pid, p := range procs {
		if pid != uint32(self) && strings.ToLower(p.name) == base {
			others = append(others, fmt.Sprintf("pid %d (parent %d) %s", pid, p.parent, imagePath(pid)))
		}
	}
	sort.Strings(others)
	for _, o := range others {
		// Another copy already serving on the same port is the commonest reason
		// for "cannot listen" - and so for a window that opens and shuts.
		tracef("another copy is running: %s", o)
	}

	if cw := consoleWindow(); cw != 0 {
		tracef("console: window %#x, class %q", uintptr(cw), windowClass(cw))
	} else {
		tracef("console: none (started without one)")
	}
	var session uint32
	procProcessIdToSessionId.Call(uintptr(self), uintptr(unsafe.Pointer(&session)))
	acp, _, _ := procGetACP.Call()
	tracef("session %d, ANSI code page %d, running as administrator: %v", session, acp, elevated())
	tracef("user: %s\\%s, profile %s", os.Getenv("USERDOMAIN"), os.Getenv("USERNAME"), os.Getenv("USERPROFILE"))
	logMemory()
	describeLocation(exe)
	probeDependencies()

	// The executable has never been code-signed. Worth having in the log
	// because it is the first thing reputation-based protection keys on:
	// SmartScreen, Defender's cloud protection, and most third-party products
	// treat an unsigned file few other machines have seen as suspect.
	tracef("code signature: none (File Drop releases are not code-signed)")

	listModules("loaded at start-up", nil)
	go func() {
		// Security products often inject their DLLs a moment after a process
		// starts rather than at once, so look again.
		before := moduleSet()
		time.Sleep(20 * time.Second)
		listModules("loaded since start-up", before)
	}()
}

type osVersionInfoEx struct {
	Size         uint32
	Major        uint32
	Minor        uint32
	Build        uint32
	PlatformID   uint32
	CSDVersion   [128]uint16
	SPMajor      uint16
	SPMinor      uint16
	SuiteMask    uint16
	ProductType  byte
	ReservedByte byte
}

// logWindowsVersion asks the kernel rather than GetVersionEx, which reports
// whatever the executable's manifest claims to be compatible with.
func logWindowsVersion() {
	var v osVersionInfoEx
	v.Size = uint32(unsafe.Sizeof(v))
	procRtlGetVersion.Call(uintptr(unsafe.Pointer(&v)))

	const cv = `SOFTWARE\Microsoft\Windows NT\CurrentVersion`
	name := regString(syscall.HKEY_LOCAL_MACHINE, cv, "ProductName")
	display := regString(syscall.HKEY_LOCAL_MACHINE, cv, "DisplayVersion")
	if display == "" {
		display = regString(syscall.HKEY_LOCAL_MACHINE, cv, "ReleaseId")
	}
	edition := regString(syscall.HKEY_LOCAL_MACHINE, cv, "EditionID")
	ubr := regString(syscall.HKEY_LOCAL_MACHINE, cv, "UBR")
	// ProductName still says "Windows 10" on Windows 11; the build number is
	// what decides it (22000 and later is 11).
	tracef("windows: %d.%d build %d.%s - %s %s (%s)", v.Major, v.Minor, v.Build, ubr, name, display, edition)
	if v.Major < 10 && !legacyBuild() {
		tracef("PROBLEM: this is the build for Windows 10 and 11, on Windows %d.%d. Use file-drop-win7.exe here.", v.Major, v.Minor)
	}
}

func regString(root syscall.Handle, path, name string) string {
	var k syscall.Handle
	if syscall.RegOpenKeyEx(root, syscall.StringToUTF16Ptr(path), 0, syscall.KEY_READ, &k) != nil {
		return ""
	}
	defer syscall.RegCloseKey(k)
	n16 := syscall.StringToUTF16Ptr(name)
	var typ, n uint32
	if syscall.RegQueryValueEx(k, n16, nil, &typ, nil, &n) != nil || n == 0 {
		return ""
	}
	buf := make([]byte, n)
	if syscall.RegQueryValueEx(k, n16, nil, &typ, &buf[0], &n) != nil {
		return ""
	}
	switch typ {
	case syscall.REG_SZ, syscall.REG_EXPAND_SZ:
		u := make([]uint16, n/2)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(buf[i*2:])
		}
		return syscall.UTF16ToString(u)
	case syscall.REG_DWORD:
		if n >= 4 {
			return strconv.FormatUint(uint64(binary.LittleEndian.Uint32(buf)), 10)
		}
	}
	return ""
}

func elevated() bool {
	proc, err := syscall.GetCurrentProcess()
	if err != nil {
		return false
	}
	var tok syscall.Token
	if syscall.OpenProcessToken(proc, syscall.TOKEN_QUERY, &tok) != nil {
		return false
	}
	defer tok.Close()
	var elev, n uint32
	if syscall.GetTokenInformation(tok, tokenElevationClass, (*byte)(unsafe.Pointer(&elev)), 4, &n) != nil {
		return false
	}
	return elev != 0
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func logMemory() {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	if ok, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m))); ok == 0 {
		return
	}
	tracef("memory: %s of %s free (%d%% in use), page file %s of %s free",
		humanSize(int64(m.AvailPhys)), humanSize(int64(m.TotalPhys)), m.MemoryLoad,
		humanSize(int64(m.AvailPageFile)), humanSize(int64(m.TotalPageFile)))
}

// describeLocation says what is unusual about where the program is being run
// from, because several ways of being blocked depend on exactly that.
func describeLocation(exe string) {
	lower := strings.ToLower(exe)

	// Mark of the Web: the note Windows attaches to a downloaded file, and
	// what SmartScreen and Office-style protections check.
	if motw, err := os.ReadFile(exe + ":Zone.Identifier"); err == nil {
		zone := "unknown"
		for _, line := range strings.Split(string(motw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "ZoneId=") {
				zone = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "ZoneId="))
			}
		}
		tracef("executable is marked as downloaded (Zone.Identifier ZoneId=%s; 3 = internet). SmartScreen checks files like this.", zone)
	} else {
		tracef("executable carries no download mark (Zone.Identifier)")
	}

	vol := filepath.VolumeName(exe)
	if strings.HasPrefix(vol, `\\`) {
		tracef("executable is on a network share (%s): some security products refuse to run programs from these", vol)
	} else if vol != "" {
		t, _, _ := procGetDriveType.Call(uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(vol + `\`))))
		kinds := map[uintptr]string{2: "removable drive", 3: "fixed disk", 4: "network drive", 5: "CD/DVD", 6: "RAM disk"}
		kind := kinds[t]
		if kind == "" {
			kind = "unknown drive type " + strconv.Itoa(int(t))
		}
		tracef("executable is on %s %s", vol, kind)
		if t == 2 {
			tracef("note: Defender's attack surface reduction rules can block unsigned programs run from USB drives")
		}
	}

	temp := strings.ToLower(os.TempDir())
	switch {
	case strings.Contains(lower, `\temp1_`) || strings.Contains(lower, ".zip\\") ||
		strings.Contains(lower, `\rar$ex`) || strings.Contains(lower, `\7zo`):
		tracef("PROBLEM: this is being run straight out of a zip file. Windows unpacks a copy into a temporary " +
			"folder and deletes it when the zip window closes. Extract the zip first and run it from there.")
	case temp != "" && strings.HasPrefix(lower, temp+`\`):
		tracef("note: this is being run from the temporary folder, which Windows and clean-up tools empty")
	case strings.Contains(lower, `\onedrive`):
		tracef("note: executable is inside OneDrive; a file not yet fully downloaded from the cloud can fail to start")
	}
	if home := strings.ToLower(os.Getenv("USERPROFILE")); home != "" {
		for _, sub := range []string{"Desktop", "Documents", "Pictures", "Videos", "Music"} {
			if strings.HasPrefix(lower, strings.ToLower(filepath.Join(home, sub))+`\`) {
				tracef("note: executable is in %s, one of the folders Defender's Controlled folder access protects", sub)
			}
		}
	}
	if len(exe) > 240 {
		tracef("note: the executable's path is %d characters long, near Windows' 260-character limit", len(exe))
	}
}

func probeDependencies() {
	var missing []string
	total := 0
	dlls := make([]string, 0, len(dependencies))
	for d := range dependencies {
		dlls = append(dlls, d)
	}
	sort.Strings(dlls)
	for _, dll := range dlls {
		d, err := syscall.LoadDLL(dll)
		if err != nil {
			missing = append(missing, fmt.Sprintf("%s could not be loaded: %v", dll, err))
			continue
		}
		path := d.Name
		var buf [syscall.MAX_PATH]uint16
		if n, _, _ := procGetModuleFileName.Call(uintptr(d.Handle), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))); n > 0 {
			path = syscall.UTF16ToString(buf[:n])
		}
		for _, fn := range dependencies[dll] {
			total++
			if _, err := d.FindProc(fn); err != nil {
				missing = append(missing, fmt.Sprintf("%s has no %s (%s)", dll, fn, path))
			}
		}
		tracef("dependency %s: %s", dll, path)
	}
	if len(missing) == 0 {
		tracef("dependencies: all %d Windows functions this program calls are present", total)
		return
	}
	for _, m := range missing {
		tracef("PROBLEM: missing dependency: %s - the program will crash when it reaches the code that needs it", m)
	}
}

type procInfo struct {
	name   string
	parent uint32
}

func processList() map[uint32]procInfo {
	out := map[uint32]procInfo{}
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return out
	}
	defer syscall.CloseHandle(snap)
	var e syscall.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err := syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		out[e.ProcessID] = procInfo{name: syscall.UTF16ToString(e.ExeFile[:]), parent: e.ParentProcessID}
	}
	return out
}

func imagePath(pid uint32) string {
	h, err := syscall.OpenProcess(processQueryLimited, false, pid)
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(h)
	var buf [1024]uint16
	n := uint32(len(buf))
	if ok, _, _ := procQueryFullImageName.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(processQueryLimited, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if syscall.GetExitCodeProcess(h, &code) != nil {
		return false
	}
	return code == stillActive
}

type moduleEntry32 struct {
	Size         uint32
	ModuleID     uint32
	ProcessID    uint32
	GlblcntUsage uint32
	ProccntUsage uint32
	ModBaseAddr  uintptr
	ModBaseSize  uint32
	HModule      syscall.Handle
	Module       [256]uint16
	ExePath      [syscall.MAX_PATH]uint16
}

func moduleSet() map[string]bool {
	out := map[string]bool{}
	snap, err := syscall.CreateToolhelp32Snapshot(th32csSnapModule|th32csSnapModule32, uint32(os.Getpid()))
	if err != nil {
		return out
	}
	defer syscall.CloseHandle(snap)
	var e moduleEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	ok, _, _ := procModule32First.Call(uintptr(snap), uintptr(unsafe.Pointer(&e)))
	for ok != 0 {
		out[syscall.UTF16ToString(e.ExePath[:])] = true
		ok, _, _ = procModule32Next.Call(uintptr(snap), uintptr(unsafe.Pointer(&e)))
	}
	return out
}

// listModules names the DLLs inside this process that did not come with
// Windows. File Drop itself loads nothing else, so anything here was put there
// by other software - usually security software watching the program from the
// inside, whose name tends to be in the folder it came from.
func listModules(when string, before map[string]bool) {
	all := moduleSet()
	winDir := strings.ToLower(systemRoot()) + `\`
	self := strings.ToLower(exePath())
	var foreign []string
	for p := range all {
		lp := strings.ToLower(p)
		if before[p] || lp == self || strings.HasPrefix(lp, winDir) {
			continue
		}
		foreign = append(foreign, p)
	}
	sort.Strings(foreign)
	if before == nil {
		tracef("modules %s: %d, of which %d not part of Windows", when, len(all), len(foreign))
	} else if len(foreign) == 0 {
		tracef("modules %s: nothing new from outside Windows", when)
	}
	for _, p := range foreign {
		tracef("third-party module %s: %s", when, p)
	}
}

// startWatcher starts the copy that waits for this one to end. It is given a
// handle to this process rather than its number: a handle cannot come to mean
// some other process, and it still works if this one is gone before the
// watcher has finished starting.
func startWatcher(survey bool) error {
	self, err := syscall.GetCurrentProcess()
	if err != nil {
		return err
	}
	var h syscall.Handle
	if err := syscall.DuplicateHandle(self, self, self, &h, synchronize|processQueryLimited, true, 0); err != nil {
		return err
	}
	defer syscall.CloseHandle(h)

	args := []string{"-log-watch", fmt.Sprintf("%d:%d", os.Getpid(), uintptr(h))}
	if survey {
		args = append(args, "-log-survey")
	}
	start := func(flags uint32) (*exec.Cmd, error) {
		cmd := exec.Command(exePath(), args...)
		cmd.Dir, _ = os.Getwd()
		cmd.SysProcAttr = &syscall.SysProcAttr{
			CreationFlags:              detachedProcess | flags,
			AdditionalInheritedHandles: []syscall.Handle{h},
		}
		return cmd, cmd.Start()
	}
	// It must outlive this process however this process ends, so it is kept
	// out of any job this one is in.
	cmd, err := start(createBreakawayFromJob)
	if err != nil {
		cmd, err = start(0)
		if err != nil {
			return err
		}
		tracef("watcher started inside this process's job; a kill that takes the whole job takes it too")
	}
	tracef("watcher started, pid %d", cmd.Process.Pid)
	cmd.Process.Release()
	return nil
}

// runWatcher is all a watcher does.
func runWatcher(arg string) {
	pidText, handleText, _ := strings.Cut(arg, ":")
	pid, _ := strconv.Atoi(pidText)
	hv, err := strconv.ParseUint(handleText, 10, 64)
	if pid == 0 || err != nil {
		tracef("watcher given nothing usable to watch (%q)", arg)
		return
	}
	h := syscall.Handle(hv)
	tracef("watching pid %d", pid)

	var wg sync.WaitGroup
	if hasArg(os.Args[1:], "-log-survey") {
		wg.Add(1)
		go func() {
			defer wg.Done()
			survey()
		}()
	}

	syscall.WaitForSingleObject(h, syscall.INFINITE)

	var code uint32
	syscall.GetExitCodeProcess(h, &code)
	// When it started, so the search of Windows' logs covers its whole life
	// and nothing from before it.
	startedAt := time.Now().Add(-time.Minute)
	lifetime := ""
	var created, exited, kernel, user syscall.Filetime
	if syscall.GetProcessTimes(h, &created, &exited, &kernel, &user) == nil {
		startedAt = time.Unix(0, created.Nanoseconds())
		lifetime = fmt.Sprintf(" after %s", time.Unix(0, exited.Nanoseconds()).Sub(startedAt).Round(time.Millisecond))
	}
	syscall.CloseHandle(h)

	intent, said := intentFor(pid)
	tracef("watch-report pid=%d ended%s with exit code %d (0x%08X)", pid, lifetime, int32(code), code)
	for _, line := range explainExit(code, intent, said) {
		tracef("  %s", line)
	}

	if !said || code != 0 {
		// Windows Error Reporting and the security products write their
		// records a moment after the fact.
		time.Sleep(5 * time.Second)
		postMortem(startedAt.Add(-10 * time.Second))
	}
	wg.Wait()
	tracef("watcher finished")
}

// explainExit turns an exit code into what it means, as far as the code alone
// can say - and, for the ambiguous ones, what the log shows about it.
func explainExit(code uint32, intent string, said bool) []string {
	if said {
		lines := []string{"It said it was going to: " + intent}
		if code != 0 {
			lines = append(lines, "That is File Drop stopping itself on an error, and that line is the reason. Nothing killed it.")
		} else {
			lines = append(lines, "That is an ordinary, intended exit.")
		}
		return lines
	}

	lines := []string{"It did NOT record that it was going to stop - it did not end by its own choice."}
	switch code {
	case 0:
		lines = append(lines, "Exit code 0 is normally a clean exit, so this is likely a route out of the program that does"+
			" not record itself. Check the lines just above for what it was doing.")
	case 1:
		lines = append(lines, "Exit code 1 from a program that did not choose to exit is what TerminateProcess leaves behind:"+
			" Task Manager's End task, taskkill /F, and the way most antivirus and endpoint-protection products kill a program."+
			" If nobody ended it by hand, security software is the likeliest cause - see the survey and the events below.")
	case 0xFFFFFFFF:
		lines = append(lines, "Exit code -1 from a program that did not choose to exit is TerminateProcess again, with the"+
			" code PowerShell's Stop-Process, .NET programs and many security and system tools use. If nobody ended it by"+
			" hand, security software is the likeliest cause - see the survey and the events below.")
	case 2:
		lines = append(lines, "Exit code 2 is the Go runtime crashing: an unrecovered panic or a fatal error. The trace,"+
			" naming the failing line, is in this log just above (look for \"panic:\" or \"fatal error:\").")
	case 0xC000013A:
		lines = append(lines, "STATUS_CONTROL_C_EXIT: the console window was closed, or Ctrl+Break pressed, or Windows ended the"+
			" console session (sign-out, shut down).")
	case 0x40010004:
		lines = append(lines, "DBG_TERMINATE_PROCESS: killed by a debugger or a process-monitoring tool.")
	case 0xC0000005:
		lines = append(lines, "STATUS_ACCESS_VIOLATION: a memory fault. In a Go program that the runtime did not catch, this"+
			" usually comes from foreign code inside the process - see any \"third-party module\" lines above.")
	case 0xC0000409:
		lines = append(lines, "STATUS_STACK_BUFFER_OVERRUN / fail-fast: the process was ended immediately by a security check."+
			" Exploit protection and DLLs injected by security software are the usual sources.")
	case 0xC0000602:
		lines = append(lines, "STATUS_FAIL_FAST_EXCEPTION: ended immediately by a fail-fast request, commonly from exploit"+
			" protection or injected code.")
	case 0xC0000374:
		lines = append(lines, "STATUS_HEAP_CORRUPTION: memory was corrupted, almost certainly by foreign code in the process.")
	case 0xC0000135:
		lines = append(lines, "STATUS_DLL_NOT_FOUND: a DLL the program needs is missing - a broken or stripped-down Windows.")
	case 0xC0000139:
		lines = append(lines, "STATUS_ENTRYPOINT_NOT_FOUND: a Windows DLL is too old for this build. On Windows 7 or 8, use"+
			" file-drop-win7.exe.")
	case 0xC000007B:
		lines = append(lines, "STATUS_INVALID_IMAGE_FORMAT: the executable or a DLL it loads is damaged or the wrong architecture.")
	case 0xC0000142:
		lines = append(lines, "STATUS_DLL_INIT_FAILED: a DLL failed to start up - often one injected by other software, or a"+
			" desktop that has run out of resources.")
	case 0xC00000FD:
		lines = append(lines, "STATUS_STACK_OVERFLOW.")
	case 0xC0000017, 0xC000012D:
		lines = append(lines, "Out of memory or page file.")
	case 0xC0000022:
		lines = append(lines, "STATUS_ACCESS_DENIED: Windows refused the process something it needed to start or keep running.")
	case 0xC000041D:
		lines = append(lines, "STATUS_FATAL_USER_CALLBACK_EXCEPTION: a crash inside a window callback - the tray icon's code"+
			" or something hooked into it.")
	default:
		if code >= 0xC0000000 {
			lines = append(lines, "This is a Windows NTSTATUS error code: a crash or a forced termination by the system. Searching for"+
				fmt.Sprintf(" 0x%08X will name it.", code))
		} else {
			lines = append(lines, "Another program ended it with this code (TerminateProcess). Security software and system tools"+
				" choose their own codes.")
		}
	}
	return lines
}

// command runs one of Windows' own tools for its output, with no window and a
// time limit, and never from wherever PATH happens to point.
func command(timeout time.Duration, stdin string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("gave up after %s", timeout)
	}
	return string(out), err
}

// The survey, run once per start by the first watcher. Everything in it reads;
// nothing changes anything.
//
// It is fed to PowerShell on standard input, which reads a statement per line
// as if typed at a prompt and silently drops a block spread over several - so
// each statement here is one line, and they are sent a blank line apart. It
// keeps to what Windows 7's PowerShell 2.0 understands, for the build that
// runs there.
var surveyScript = strings.Join([]string{
	`$ErrorActionPreference = 'SilentlyContinue'`,
	`[Console]::OutputEncoding = [Text.Encoding]::UTF8`,
	`foreach ($c in 'AntiVirusProduct','AntiSpywareProduct','FirewallProduct') { Get-WmiObject -Namespace root\SecurityCenter2 -Class $c | ForEach-Object { "product|$c|$($_.displayName)|$([int]$_.productState)|$($_.pathToSignedProductExe)" } }`,
	`$st = Get-MpComputerStatus`,
	`if ($st) { "defender|running mode: $($st.AMRunningMode); real-time protection: $($st.RealTimeProtectionEnabled); behaviour monitoring: $($st.BehaviorMonitorEnabled); tamper protection: $($st.IsTamperProtected); signatures $($st.AntivirusSignatureVersion) from $($st.AntivirusSignatureLastUpdated)" } else { "defender|status unavailable: Defender is off, absent, or has handed over to another product" }`,
	`$p = Get-MpPreference`,
	`if ($p) { "cfa|$($p.EnableControlledFolderAccess)"; "cloud|$($p.PUAProtection)|$($p.MAPSReporting)|$($p.CloudBlockLevel)" }`,
	`if ($p) { $ids = @($p.AttackSurfaceReductionRules_Ids); $acts = @($p.AttackSurfaceReductionRules_Actions); for ($i = 0; $i -lt $ids.Count; $i++) { if ($ids[$i]) { "asr|$($ids[$i])|$($acts[$i])" } } }`,
	`if ($p) { "defender|exclusions (folders): $($p.ExclusionPath -join '; ')"; "defender|exclusions (processes): $($p.ExclusionProcess -join '; ')" }`,
	`$names = @{}; Get-MpThreat | ForEach-Object { $names[[string]$_.ThreatID] = $_.ThreatName }`,
	`Get-MpThreatDetection | Sort-Object InitialDetectionTime -Descending | Select-Object -First 50 | ForEach-Object { "threat|$($_.InitialDetectionTime)|$($names[[string]$_.ThreatID])|$($_.ActionSuccess)|$($_.Resources -join ', ')" }`,
	`$sac = (Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\CI\Policy').VerifiedAndReputablePolicyState; if ($sac -ne $null) { "sac|$sac" }`,
	`$ss = (Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Explorer').SmartScreenEnabled; if ($ss) { "defender|SmartScreen for apps and files: $ss" }`,
	`"done|"`,
}, "\n\n") + "\n\n"

// asrRules names the attack surface reduction rules, and says which of them
// can stop this program.
var asrRules = map[string]struct {
	name     string
	relevant bool
}{
	"01443614-cd74-433a-b99e-2ecdc07bfc25": {"Block executable files from running unless they meet a prevalence, age, or trusted list criterion", true},
	"b2b3f03d-6a65-4f7b-a9c7-1c7ef74a9ba4": {"Block untrusted and unsigned processes that run from USB", true},
	"c1db55ab-c21a-4637-bb3f-a12568109d35": {"Use advanced protection against ransomware", true},
	"d1e49aac-8f56-4280-b9ba-993a6d77406c": {"Block process creations originating from PSExec and WMI commands", false},
	"c0033c00-d16d-4114-a5a0-dc9b3a7d2ceb": {"Block use of copied or impersonated system tools", false},
	"9e6c4e1f-7d60-472f-ba1a-a39ef669e4b2": {"Block credential stealing from the Windows local security authority subsystem", false},
	"5beb7efe-fd9a-4556-801d-275e5ffc04cc": {"Block execution of potentially obfuscated scripts", false},
	"d4f940ab-401b-4efc-aadc-ad5f3c50688a": {"Block all Office applications from creating child processes", false},
	"56a863a9-875e-4185-98a7-b882c64b5ce5": {"Block abuse of exploited vulnerable signed drivers", false},
	"e6db77e5-3df2-4cf1-b95a-636979351e5b": {"Block persistence through WMI event subscription", false},
	"33ddedf1-c6e0-47cb-833e-de6133960387": {"Block rebooting machine in Safe Mode", false},
}

func survey() {
	exe := exePath()
	base := filepath.Base(exe)
	tracef("survey: looking at security software and Windows' records (read-only)")

	ps := system32(`WindowsPowerShell\v1.0\powershell.exe`)
	out, err := command(90*time.Second, surveyScript, ps, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "-")
	if !strings.Contains(out, "done|") {
		tracef("survey: PowerShell did not finish (%v) - the list of security products may be incomplete: %s", err, firstLines(strings.TrimSpace(out), 4))
	}
	asrActions := map[string]string{"0": "off", "1": "BLOCK", "2": "audit only", "6": "warn"}
	var threats, ours int
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		kind, rest, _ := strings.Cut(strings.TrimSpace(sc.Text()), "|")
		switch kind {
		case "product":
			f := strings.Split(rest, "|")
			if len(f) < 4 {
				continue
			}
			state, _ := strconv.Atoi(f[2])
			on, fresh := "OFF", "definitions up to date"
			if state&0x1000 != 0 {
				on = "ON"
			}
			if state&0x10 != 0 {
				fresh = "definitions OUT OF DATE"
			}
			kinds := map[string]string{"AntiVirusProduct": "antivirus", "AntiSpywareProduct": "antispyware", "FirewallProduct": "firewall"}
			exePart := ""
			if len(f) > 3 && f[3] != "" {
				exePart = " - " + f[3]
			}
			tracef("survey: %s %q is %s, %s%s", kinds[f[0]], f[1], on, fresh, exePart)
		case "defender":
			tracef("survey: defender: %s", rest)
		case "cfa":
			mode := map[string]string{"0": "off", "1": "ON - blocks unknown programs writing to protected folders", "2": "audit only"}[rest]
			if mode == "" {
				mode = rest
			}
			tracef("survey: defender: controlled folder access: %s", mode)
		case "asr":
			id, act, _ := strings.Cut(rest, "|")
			r, known := asrRules[strings.ToLower(id)]
			name := r.name
			if !known {
				name = "rule " + id
			}
			a := asrActions[act]
			if a == "" {
				a = act
			}
			if r.relevant && a == "BLOCK" {
				tracef("survey: defender: ASR rule ENFORCED THAT CAN STOP THIS PROGRAM: %s", name)
			} else {
				tracef("survey: defender: ASR rule %s: %s", a, name)
			}
		case "threat":
			threats++
			if strings.Contains(strings.ToLower(rest), strings.ToLower(base)) {
				ours++
				tracef("survey: DEFENDER DETECTED THIS PROGRAM: %s", rest)
			}
		case "cloud":
			f := strings.Split(rest, "|")
			if len(f) < 3 {
				continue
			}
			pua := map[string]string{"0": "off", "1": "ON", "2": "audit only"}[f[0]]
			maps := map[string]string{"0": "off", "1": "basic", "2": "advanced"}[f[1]]
			level := map[string]string{"0": "default", "1": "moderate", "2": "high", "4": "high+", "6": "zero tolerance"}[f[2]]
			// The cloud block level decides how readily a file nobody else has
			// is stopped. "high" and above block unknown programs outright.
			tracef("survey: defender: potentially unwanted app blocking %s; cloud protection %s; cloud block level %s", pua, maps, level)
		case "sac":
			mode := map[string]string{"0": "off", "1": "ON (enforcing)", "2": "evaluation"}[rest]
			tracef("survey: Smart App Control: %s", mode)
		}
	}
	if threats > 0 && ours == 0 {
		tracef("survey: Defender's history holds %d detection(s), none naming %s", threats, base)
	}

	events("in the last 7 days", time.Now().Add(-7*24*time.Hour), exe)
	tracef("survey: finished")
}

// postMortem reads what Windows wrote about the program around the time it
// ended. Asked straight after a death, so anything here is very likely about
// that death and not some earlier one.
func postMortem(since time.Time) {
	tracef("looking for what Windows recorded about this since %s", since.Format("15:04:05"))
	if n := events("around the time it ended", since, exePath()); n == 0 {
		tracef("Windows recorded nothing about it. A program killed by third-party security software often leaves" +
			" no trace in Windows' own logs - that product's own history or quarantine list is where to look.")
	}
}

type eventSource struct {
	channel string
	ids     []int
	what    string
}

var eventSources = []eventSource{
	{"Application", []int{1000, 1001, 1002}, "crash, error report or hang"},
	{"System", []int{26}, "error message box (missing DLL and similar)"},
	{"Microsoft-Windows-Windows Defender/Operational", []int{1116, 1117, 1118, 1119, 1121, 1122, 1123, 1124, 1125, 1126}, "Defender detection or block"},
	{"Microsoft-Windows-CodeIntegrity/Operational", []int{3033, 3034, 3063, 3076, 3077}, "code integrity / Smart App Control block"},
	{"Microsoft-Windows-AppLocker/EXE and DLL", []int{8003, 8004}, "AppLocker block"},
	{"Microsoft-Windows-Security-Mitigations/UserMode", nil, "exploit protection block"},
}

// events searches Windows' event logs for anything naming the program, and
// returns how many it found.
func events(when string, since time.Time, exe string) int {
	wevtutil := system32("wevtutil.exe")
	ms := time.Since(since).Milliseconds()
	if ms < 1 {
		ms = 1
	}
	needles := []string{strings.ToLower(filepath.Base(exe)), strings.ToLower(exe)}
	found := 0
	for _, src := range eventSources {
		var idTerms []string
		for _, id := range src.ids {
			idTerms = append(idTerms, "EventID="+strconv.Itoa(id))
		}
		cond := fmt.Sprintf("TimeCreated[timediff(@SystemTime) <= %d]", ms)
		if len(idTerms) > 0 {
			cond = "(" + strings.Join(idTerms, " or ") + ") and " + cond
		}
		query := "*[System[" + cond + "]]"
		out, err := command(30*time.Second, "", wevtutil, "qe", src.channel, "/q:"+query, "/f:text", "/rd:true", "/c:500")
		if err != nil {
			msg := strings.TrimSpace(out)
			switch {
			case strings.Contains(strings.ToLower(msg), "access is denied"):
				tracef("events: %s: needs administrator rights to read (run once as administrator to include it)", src.channel)
			case strings.Contains(strings.ToLower(msg), "could not be found"):
				// This edition of Windows has no such log.
			default:
				tracef("events: %s: could not read (%v %s)", src.channel, err, firstLines(msg, 2))
			}
			continue
		}
		blocks := splitEvents(out)
		matched := 0
		for _, b := range blocks {
			lb := strings.ToLower(b)
			hit := false
			for _, n := range needles {
				if strings.Contains(lb, n) {
					hit = true
				}
			}
			if !hit {
				continue
			}
			matched++
			found++
			if matched <= 8 {
				tracef("EVENT (%s, %s) in %s:\n%s", src.what, when, src.channel, trimEvent(b))
			}
		}
		if matched > 8 {
			tracef("events: %d more like that in %s", matched-8, src.channel)
		}
		if matched == 0 && len(blocks) > 0 && src.channel != "Application" && src.channel != "System" {
			tracef("events: %s holds %d %s record(s) %s, none naming %s", src.channel, len(blocks), src.what, when, filepath.Base(exe))
		}
	}
	return found
}

func splitEvents(out string) []string {
	var blocks []string
	var cur strings.Builder
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "Event[") && cur.Len() > 0 {
			blocks = append(blocks, cur.String())
			cur.Reset()
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	if strings.TrimSpace(cur.String()) != "" {
		blocks = append(blocks, cur.String())
	}
	return blocks
}

func trimEvent(b string) string {
	var keep []string
	for _, line := range strings.Split(b, "\n") {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			continue
		}
		keep = append(keep, "  "+strings.TrimSpace(line))
		if len(keep) >= 40 {
			keep = append(keep, "  …")
			break
		}
	}
	return strings.Join(keep, "\n")
}

// explainPort writes to the -log log why a port could not be had: who is
// holding it, or whether Windows has fenced it off.
func explainPort(port int) {
	if !diagOn {
		return
	}
	for _, h := range portHolders(port) {
		tracef("port %d is held by %s", port, h)
	}
	for _, r := range portReservations(port) {
		tracef("port %d is inside a range Windows has reserved (%s), usually for Hyper-V, WSL or Docker:"+
			" choose another port with -port", port, r)
	}
}

// portHolders names the programs listening on a port, each as "pid 1234
// C:\path\to\program.exe".
func portHolders(port int) []string {
	out, err := command(15*time.Second, "", system32("NETSTAT.EXE"), "-ano")
	if err != nil {
		return nil
	}
	var held []string
	seen := map[int]bool{}
	procs := processList()
	suffix := ":" + strconv.Itoa(port)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || !strings.EqualFold(f[0], "TCP") || !strings.HasSuffix(f[1], suffix) || !strings.EqualFold(f[3], "LISTENING") {
			continue
		}
		pid, _ := strconv.Atoi(f[4])
		if seen[pid] {
			continue
		}
		seen[pid] = true
		name := procs[uint32(pid)].name
		if p := imagePath(uint32(pid)); p != "" {
			name = p
		}
		if name == "" {
			name = "(a program this account cannot see)"
		}
		held = append(held, fmt.Sprintf("pid %d %s", pid, name))
	}
	return held
}

// portReservations lists the ranges Windows has set aside that take in a port.
// Hyper-V, WSL and Docker reserve ranges that can move at every boot, and a port
// inside one fails with "access permissions" even though nothing is listening.
func portReservations(port int) []string {
	var found []string
	for _, proto := range []string{"ipv4", "ipv6"} {
		out, err := command(15*time.Second, "", system32("netsh.exe"), "interface", proto, "show", "excludedportrange", "protocol=tcp")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			lo, e1 := strconv.Atoi(f[0])
			hi, e2 := strconv.Atoi(f[1])
			if e1 == nil && e2 == nil && port >= lo && port <= hi {
				found = append(found, fmt.Sprintf("%s %d-%d", proto, lo, hi))
			}
		}
	}
	return found
}

// portRefused reports whether a listen failed because Windows would not allow
// the port - a reserved range, or security software - rather than because
// another program already has it.
func portRefused(err error) bool {
	return errors.Is(err, syscall.Errno(10013)) // WSAEACCES
}
