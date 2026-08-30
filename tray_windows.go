//go:build windows

package main

import (
	"fmt"
	"log"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"filedrop/internal/icon"
)

// The notification-area icon.
//
// A server whose whole interface is a web page still has to live somewhere on
// the desktop, and a console window is a poor place: minimising it hides the
// program in the taskbar next to whatever else is open, and closing it by
// mistake takes the server with it. The icon beside the clock gives the program
// a home - a way back to the QR page, a way into the drop folder, and a Quit
// that shuts the tunnel down properly - and it is what makes hiding the console
// window a reasonable thing to offer at all.
//
// Everything here is the Windows API by hand, because a tray icon is a window
// class, a message loop and a handful of structs, and pulling in a dependency
// for that would cost more than it saves.

const traySupported = true

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procRegisterClass      = user32.NewProc("RegisterClassW")
	procCreateWindowEx     = user32.NewProc("CreateWindowExW")
	procDefWindowProc      = user32.NewProc("DefWindowProcW")
	procDestroyWindow      = user32.NewProc("DestroyWindow")
	procGetMessage         = user32.NewProc("GetMessageW")
	procTranslateMessage   = user32.NewProc("TranslateMessage")
	procDispatchMessage    = user32.NewProc("DispatchMessageW")
	procPostQuitMessage    = user32.NewProc("PostQuitMessage")
	procCreatePopupMenu    = user32.NewProc("CreatePopupMenu")
	procAppendMenu         = user32.NewProc("AppendMenuW")
	procDestroyMenu        = user32.NewProc("DestroyMenu")
	procTrackPopupMenu     = user32.NewProc("TrackPopupMenu")
	procSetMenuDefaultItem = user32.NewProc("SetMenuDefaultItem")
	procGetCursorPos       = user32.NewProc("GetCursorPos")
	procSetForegroundWnd   = user32.NewProc("SetForegroundWindow")
	procPostMessage        = user32.NewProc("PostMessageW")
	procCreateIcon         = user32.NewProc("CreateIconFromResourceEx")
	procGetSystemMetrics   = user32.NewProc("GetSystemMetrics")
	procShowWindow         = user32.NewProc("ShowWindow")
	procIsWindowVisible    = user32.NewProc("IsWindowVisible")
	procRegisterWinMessage = user32.NewProc("RegisterWindowMessageW")
	procGetClassName       = user32.NewProc("GetClassNameW")

	procShellNotifyIcon = shell32.NewProc("Shell_NotifyIconW")

	procGetModuleHandle  = kernel32.NewProc("GetModuleHandleW")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcs  = kernel32.NewProc("GetConsoleProcessList")
)

const (
	wmDestroy   = 0x0002
	wmClose     = 0x0010
	wmCommand   = 0x0111
	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205
	wmApp       = 0x8000

	// The message the icon sends us when it is clicked.
	wmTrayCallback = wmApp + 1

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10

	niifInfo = 0x01

	mfString    = 0x0000
	mfSeparator = 0x0800

	tpmLeftAlign   = 0x0000
	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	swHide = 0
	swShow = 5

	smCXSmIcon = 49

	// Menu command ids.
	cmdOpenHost    = 1
	cmdOpenFolder  = 2
	cmdToggleShell = 3
	cmdQuit        = 4
)

type wndClass struct {
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   syscall.Handle
	Icon       syscall.Handle
	Cursor     syscall.Handle
	Background syscall.Handle
	MenuName   *uint16
	ClassName  *uint16
}

type point struct{ X, Y int32 }

type winMsg struct {
	HWnd    syscall.Handle
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

// notifyIconData is NOTIFYICONDATAW as of Vista, which is the size Windows
// expects to be told: 976 bytes on 64-bit, checked against unsafe.Sizeof rather
// than written in.
type notifyIconData struct {
	CbSize           uint32
	HWnd             syscall.Handle
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            syscall.Handle
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     syscall.Handle
}

var tray struct {
	mu sync.Mutex

	running bool
	hwnd    syscall.Handle
	icon    syscall.Handle

	hostURL string
	quit    func()

	// The message the shell broadcasts when it restarts, at which point every
	// tray icon on the desktop has been forgotten and has to be added again.
	taskbarCreated uint32
}

// startTray puts the icon beside the clock and keeps a message loop running for
// as long as the program does. It returns once the icon is up, so the caller
// can tell whether hiding the console window would strand anybody.
func startTray(hostURL string, quit func()) error {
	tray.mu.Lock()
	tray.hostURL = hostURL
	tray.quit = quit
	tray.mu.Unlock()

	ready := make(chan error, 1)
	go trayLoop(ready)
	return <-ready
}

// trayLoop owns the window and the icon. A window's messages only arrive on the
// thread that created it, so this goroutine keeps one to itself for the life of
// the process.
func trayLoop(ready chan<- error) {
	runtime.LockOSThread()

	instance, _, _ := procGetModuleHandle.Call(0)
	className := syscall.StringToUTF16Ptr("FileDropTray")

	class := wndClass{
		WndProc:   syscall.NewCallback(trayWndProc),
		Instance:  syscall.Handle(instance),
		ClassName: className,
	}
	if atom, _, err := procRegisterClass.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		ready <- fmt.Errorf("could not register the tray window class: %v", err)
		return
	}

	// Never shown: it exists to receive the icon's messages and to own the menu.
	hwnd, _, err := procCreateWindowEx.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("File Drop"))),
		0, 0, 0, 0, 0, 0, 0, instance, 0,
	)
	if hwnd == 0 {
		ready <- fmt.Errorf("could not create the tray window: %v", err)
		return
	}

	taskbar, _, _ := procRegisterWinMessage.Call(
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("TaskbarCreated"))))

	tray.mu.Lock()
	tray.hwnd = syscall.Handle(hwnd)
	tray.icon = loadTrayIcon()
	tray.taskbarCreated = uint32(taskbar)
	hostURL := tray.hostURL
	tray.mu.Unlock()

	if err := addTrayIcon(hostURL); err != nil {
		procDestroyWindow.Call(hwnd)
		ready <- err
		return
	}

	tray.mu.Lock()
	tray.running = true
	tray.mu.Unlock()
	ready <- nil

	var m winMsg
	for {
		got, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		// 0 is WM_QUIT and -1 is an error; either way there is nothing left to
		// pump, and the icon is removed on the way out.
		if int32(got) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	removeTrayIcon()
}

// loadTrayIcon builds the icon at the size the shell is asking for, which is
// what makes it crisp on a high-DPI display instead of a scaled 16-pixel
// picture.
func loadTrayIcon() syscall.Handle {
	size, _, _ := procGetSystemMetrics.Call(smCXSmIcon)
	if size < 8 || size > 256 {
		size = 16
	}
	bits := icon.Image(int(size))
	// 0x00030000 is the icon format version, which has not changed since
	// Windows 3.0 and is still what this call wants to be told.
	h, _, _ := procCreateIcon.Call(
		uintptr(unsafe.Pointer(&bits[0])), uintptr(len(bits)),
		1, 0x00030000, size, size, 0)
	return syscall.Handle(h)
}

func (n *notifyIconData) init() {
	n.CbSize = uint32(unsafe.Sizeof(*n))
	tray.mu.Lock()
	n.HWnd = tray.hwnd
	n.HIcon = tray.icon
	tray.mu.Unlock()
	n.UID = 1
}

func addTrayIcon(hostURL string) error {
	var data notifyIconData
	data.init()
	data.UFlags = nifMessage | nifIcon | nifTip
	data.UCallbackMessage = wmTrayCallback
	copyUTF16(data.SzTip[:], "File Drop — "+hostURL)

	if ok, _, err := procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&data))); ok == 0 {
		return fmt.Errorf("Windows would not add the tray icon: %v", err)
	}
	return nil
}

func removeTrayIcon() {
	var data notifyIconData
	data.init()
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&data)))

	tray.mu.Lock()
	tray.running = false
	tray.mu.Unlock()
}

// notifyArrival is the tray's half of the arrival announcement: the balloon
// beside the clock, for whoever is not looking at the /host page. It does
// nothing when there is no icon to hang it off.
func notifyArrival(summary, folder string) {
	tray.mu.Lock()
	running := tray.running
	tray.mu.Unlock()
	if !running {
		return
	}

	var data notifyIconData
	data.init()
	data.UFlags = nifInfo
	data.DwInfoFlags = niifInfo
	copyUTF16(data.SzInfoTitle[:], "Files received")
	copyUTF16(data.SzInfo[:], folder+"\n"+summary)

	procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&data)))
}

func trayWndProc(hwnd syscall.Handle, message uint32, wparam, lparam uintptr) uintptr {
	tray.mu.Lock()
	taskbarCreated := tray.taskbarCreated
	hostURL := tray.hostURL
	tray.mu.Unlock()

	switch {
	case message == wmTrayCallback:
		switch uint32(lparam) {
		case wmLButtonUp:
			// The page is what the program is for, so a plain click opens it.
			openBrowser(hostURL)
		case wmRButtonUp:
			showTrayMenu(hwnd)
		}
		return 0

	case message == wmCommand:
		trayCommand(uint32(wparam & 0xFFFF))
		return 0

	case message == wmClose:
		procDestroyWindow.Call(uintptr(hwnd))
		return 0

	case message == wmDestroy:
		procPostQuitMessage.Call(0)
		return 0

	case taskbarCreated != 0 && message == taskbarCreated:
		// Explorer restarted and took every tray icon with it.
		if err := addTrayIcon(hostURL); err != nil {
			log.Printf("note: %v", err)
		}
		return 0
	}

	ret, _, _ := procDefWindowProc.Call(uintptr(hwnd), uintptr(message), wparam, lparam)
	return ret
}

func showTrayMenu(hwnd syscall.Handle) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	add := func(id uintptr, label string) {
		procAppendMenu.Call(menu, mfString, id,
			uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(label))))
	}
	separator := func() { procAppendMenu.Call(menu, mfSeparator, 0, 0) }

	add(cmdOpenHost, "Open the QR page")
	add(cmdOpenFolder, "Open the drop folder")
	if consoleHideable() {
		separator()
		if consoleVisible() {
			add(cmdToggleShell, "Hide the console window")
		} else {
			add(cmdToggleShell, "Show the console window")
		}
	}
	separator()
	add(cmdQuit, "Quit File Drop")
	procSetMenuDefaultItem.Call(menu, cmdOpenHost, 0)

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

	// Without this the menu stays up after a click elsewhere, which is a
	// documented quirk of tray menus rather than anything we are doing wrong.
	procSetForegroundWnd.Call(uintptr(hwnd))

	cmd, _, _ := procTrackPopupMenu.Call(menu,
		tpmLeftAlign|tpmRightButton|tpmReturnCmd,
		uintptr(pt.X), uintptr(pt.Y), 0, uintptr(hwnd), 0)
	if cmd != 0 {
		trayCommand(uint32(cmd))
	}
}

func trayCommand(id uint32) {
	tray.mu.Lock()
	hostURL := tray.hostURL
	quit := tray.quit
	tray.mu.Unlock()

	switch id {
	case cmdOpenHost:
		openBrowser(hostURL)
	case cmdOpenFolder:
		openFolder(currentSettings().Dir)
	case cmdToggleShell:
		toggleConsole()
	case cmdQuit:
		log.Printf("quitting from the tray icon")
		removeTrayIcon()
		if quit != nil {
			// Runs on its own goroutine: the shutdown it starts ends in
			// os.Exit, and holding the message loop up until then would leave
			// the menu on screen throughout.
			go quit()
		}
	}
}

// consoleHideable reports whether there is a console window this program may
// show and hide, which takes three things being true at once.
//
// It has to be ours alone: a server started from a shell that is still attached
// shares that shell's window, and hiding it would take the operator's terminal
// away over something they asked of one program inside it.
//
// And it has to be a real console window rather than the pseudo-console
// Windows 11 hands out when Windows Terminal is the default terminal. That one
// is invisible already; the window on screen belongs to the terminal, which may
// be showing other tabs, and is not ours to hide. Offering a menu item that
// silently did nothing would be worse than not offering it.
func consoleHideable() bool {
	hwnd := consoleWindow()
	if hwnd == 0 {
		return false
	}
	if windowClass(hwnd) != "ConsoleWindowClass" {
		return false
	}
	var pids [8]uint32
	n, _, _ := procGetConsoleProcs.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

func windowClass(hwnd syscall.Handle) string {
	var buf [64]uint16
	n, _, _ := procGetClassName.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

func consoleWindow() syscall.Handle {
	h, _, _ := procGetConsoleWindow.Call()
	return syscall.Handle(h)
}

func consoleVisible() bool {
	hwnd := consoleWindow()
	if hwnd == 0 {
		return false
	}
	visible, _, _ := procIsWindowVisible.Call(uintptr(hwnd))
	return visible != 0
}

func toggleConsole() {
	if !consoleHideable() {
		return
	}
	hwnd := consoleWindow()
	if consoleVisible() {
		procShowWindow.Call(uintptr(hwnd), swHide)
		return
	}
	procShowWindow.Call(uintptr(hwnd), swShow)
	procSetForegroundWnd.Call(uintptr(hwnd))
}

// copyUTF16 fills a fixed-width UTF-16 field, truncating rather than
// overflowing: these are all display strings, and a clipped tooltip is better
// than a corrupted struct.
func copyUTF16(dst []uint16, s string) {
	encoded, err := syscall.UTF16FromString(s)
	if err != nil {
		return
	}
	if len(encoded) > len(dst) {
		encoded = encoded[:len(dst)]
		encoded[len(encoded)-1] = 0
	}
	copy(dst, encoded)
}
