package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Getting the main port.
//
// Something else already listening on the port is the commonest reason File
// Drop cannot start, and started from Explorer the error used to flash past in
// a console window that closed straight away - which looks exactly like a
// crash. So:
//
//   - On the default port, File Drop just moves along: 8081, 8082 and so on
//     until one is free, and saves that one so the address stays put from then on.
//   - On a port someone chose, moving silently would break whatever they chose
//     it for, so it says so, loudly, and asks for another.

// restartingEnv marks the copy a restart starts, which may ask for its port a
// moment before the copy it replaces has finished letting go of it.
const restartingEnv = "FILE_DROP_RESTARTING"

// portOverride is a port the command line has to be corrected to when this
// program starts another copy of itself: set when -port named a port that was
// taken and a different one was used instead, which no settings file can carry
// because the command line would overrule it.
var portOverride int

// stdinLines is the one reader of the keyboard, so nothing typed ahead is lost
// between one question and the next.
var stdinLines = bufio.NewReader(os.Stdin)

func listenAddr(s Settings, port int) string {
	// Internet only binds loopback: the operator's own screen still works, but
	// nothing on the local network can reach the upload page or /upload, and
	// the tunnel remains the sole way in.
	if s.InternetOnly {
		return fmt.Sprintf("127.0.0.1:%d", port)
	}
	return fmt.Sprintf(":%d", port)
}

func portTyped() bool {
	typed := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			typed = true
		}
	})
	return typed
}

// claimPort binds the main port, or another if that one is taken, and returns
// the listener. cfg.Port ends up as the port actually in use.
func claimPort(cfg *Settings) net.Listener {
	patient := os.Getenv(restartingEnv) == "1" || os.Getenv(detachedEnv) == "1"
	os.Unsetenv(restartingEnv)

	want := cfg.Port
	ln, err := listenPatiently(*cfg, want, patient)
	if err == nil {
		return ln
	}
	typed := portTyped()
	tracef("port %d could not be had: %v", want, err)

	var port int
	if want == defaultPort && !typed {
		explainPort(want)
		ln, port = firstFreePort(*cfg, want+1)
		if ln == nil {
			fatalf("cannot listen on port %d, or on any port above it: %v", want, err)
		}
		why := "another program is using it"
		if portRefused(err) {
			why = "Windows will not allow it"
		}
		if held := portHolders(want); len(held) > 0 {
			why = "it is being used by " + strings.Join(held, ", ")
		}
		log.Printf("port %d is not free (%s) - using port %d instead", want, why, port)
	} else {
		ln, port = askForPort(*cfg, want, err)
	}
	usePort(cfg, port, typed)
	return ln
}

// listenPatiently is net.Listen, except that a copy that has just been started
// by another one - a restart, or the hand-over to the copy without a console -
// gives the port a few seconds to come free before deciding it is taken.
func listenPatiently(s Settings, port int, patient bool) (net.Listener, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		ln, err := net.Listen("tcp", listenAddr(s, port))
		if err == nil || !patient || time.Now().After(deadline) {
			return ln, err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// firstFreePort binds the first port from `from` upwards that can be had,
// skipping the one the internet listener has been told to use.
func firstFreePort(s Settings, from int) (net.Listener, int) {
	for p := from; p <= 65535; p++ {
		if p == s.PublicPort {
			continue
		}
		if ln, err := net.Listen("tcp", listenAddr(s, p)); err == nil {
			return ln, p
		}
	}
	return nil, 0
}

// nextFreePort is the port firstFreePort would pick, without keeping it.
func nextFreePort(s Settings, from int) int {
	ln, p := firstFreePort(s, from)
	if ln != nil {
		ln.Close()
	}
	return p
}

// askForPort puts up the error and asks, at the console, for another port.
// Where there is nobody to ask it stops, as it always did.
func askForPort(s Settings, want int, err error) (net.Listener, int) {
	held := portHolders(want)
	reserved := portReservations(want)
	for _, h := range held {
		tracef("port %d is held by %s", want, h)
	}
	for _, r := range reserved {
		tracef("port %d is inside a range Windows has reserved (%s)", want, r)
	}
	portBanner(want, err, held, reserved)

	if !stdinIsConsole() {
		fatalf("cannot listen on port %d: %v - choose another with -port", want, err)
	}

	for asked := false; !asked; {
		fmt.Print("  Do you want to use a different port? [Y/n] ")
		answer, ok := readAnswer()
		if !ok {
			fatalf("cannot listen on port %d: %v", want, err)
		}
		switch strings.ToLower(answer) {
		case "", "y", "yes":
			asked = true
		case "n", "no", "q", "quit":
			fmt.Printf("\n  File Drop was not started.\n")
			exitNow(1, fmt.Sprintf("port %d is not free, and using another port was declined", want))
		}
	}

	for {
		suggest := nextFreePort(s, want+1)
		if suggest != 0 {
			fmt.Printf("  Port number (press Enter for %d, the next free one): ", suggest)
		} else {
			fmt.Print("  Port number: ")
		}
		answer, ok := readAnswer()
		if !ok {
			fatalf("cannot listen on port %d: %v", want, err)
		}
		if strings.EqualFold(answer, "n") || strings.EqualFold(answer, "q") {
			fmt.Printf("\n  File Drop was not started.\n")
			exitNow(1, fmt.Sprintf("port %d is not free, and using another port was declined", want))
		}

		port := suggest
		if answer != "" {
			n, convErr := strconv.Atoi(answer)
			switch {
			case convErr != nil || n < 1 || n > 65535:
				fmt.Printf("  %q is not a port number - type a number from 1 to 65535.\n\n", answer)
				continue
			case n == s.PublicPort:
				fmt.Printf("  Port %d is the one set aside for the internet link - choose another.\n\n", n)
				continue
			}
			port = n
		}
		if port == 0 {
			continue
		}

		ln, lnErr := net.Listen("tcp", listenAddr(s, port))
		if lnErr != nil {
			why := "another program is using it"
			if portRefused(lnErr) {
				why = "Windows will not allow it"
			}
			fmt.Printf("  Port %d is not free either (%s) - try another.\n\n", port, why)
			continue
		}
		fmt.Println()
		tracef("port %d chosen at the console instead of %d", port, want)
		return ln, port
	}
}

// portBanner is the error, made impossible to miss.
func portBanner(want int, err error, held, reserved []string) {
	title := fmt.Sprintf("ERROR: PORT %d IS ALREADY IN USE", want)
	if portRefused(err) && len(held) == 0 {
		title = fmt.Sprintf("ERROR: WINDOWS WILL NOT LET FILE DROP USE PORT %d", want)
	}
	const width = 62
	bar := "  " + strings.Repeat("#", width)
	blank := "  ##" + strings.Repeat(" ", width-4) + "##"
	pad := width - 7 - len(title)
	if pad < 0 {
		pad = 0
	}
	line := "  ##   " + title + strings.Repeat(" ", pad) + "##"

	fmt.Print("\a")
	inRed(func() {
		fmt.Printf("\n%s\n%s\n%s\n%s\n%s\n", bar, blank, line, blank, bar)
	})

	fmt.Printf("\n  File Drop cannot start on port %d.\n", want)
	switch {
	case len(held) > 0:
		fmt.Printf("  It is being used by:\n")
		for _, h := range held {
			fmt.Printf("      %s\n", h)
		}
	case len(reserved) > 0:
		fmt.Printf("  Windows has reserved it (%s), usually for Hyper-V, WSL or Docker.\n", strings.Join(reserved, ", "))
	case portRefused(err):
		fmt.Printf("  Windows refused it - it may be reserved, or blocked by security software.\n")
	default:
		fmt.Printf("  %v\n", err)
	}
	fmt.Println()
}

// readAnswer reads one line from the keyboard. ok is false when there is no
// more input to read.
func readAnswer() (string, bool) {
	line, err := stdinLines.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return strings.TrimSpace(line), true
}

// usePort records that a different port is in use, and keeps it: saved to the
// settings file, so the next start goes straight to it, or - when -port on the
// command line would overrule the file - carried on the command line of any
// copy this one starts.
func usePort(cfg *Settings, port int, typed bool) {
	was := cfg.Port
	cfg.Port = port
	settingsMu.Lock()
	live.Port = port
	startupSettings.Port = port
	settingsMu.Unlock()

	if typed {
		portOverride = port
		log.Printf("note: -port %d on the command line is not free, so port %d is being used this time;"+
			" change -port in your shortcut or command to keep using it", was, port)
		return
	}
	if err := savePort(port); err != nil {
		portOverride = port
		log.Printf("note: could not save port %d to %s (%v); it will be looked for again next time", port, configPath, err)
		return
	}
	log.Printf("saved port %d to %s, so File Drop will use it from now on", port, configPath)
}

// savePort writes the settings file with only the port changed: what the file
// said before, not whatever the command line overrode it with this time.
func savePort(port int) error {
	s := fileSettings
	s.Port = port
	if err := s.save(configPath); err != nil {
		return err
	}
	fileSettings = s
	configFound.Store(true)
	return nil
}

// holdWindowOpen keeps a console window that would close the moment this
// program ends open until Enter is pressed, so an error can be read rather
// than flash past.
func holdWindowOpen() {
	if !stdinIsConsole() || !consoleClosesWithUs() {
		return
	}
	fmt.Print("\n  Press Enter to close this window.")
	readAnswer()
}

// childArgs is the command line for another copy of this program: this one's,
// corrected to the port actually in use where the command line named another.
func childArgs(extra ...string) []string {
	args := append([]string{}, os.Args[1:]...)
	if portOverride != 0 {
		args = append(args, "-port", strconv.Itoa(portOverride))
	}
	return append(args, extra...)
}
