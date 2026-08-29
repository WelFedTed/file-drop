package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Settings is everything the server can be told to do differently. Values come
// from three places, each overriding the one before: the built-in defaults, the
// TOML file, and the command line. The cog on /host reads and writes whatever
// the running server ended up with.
//
// The file is optional. When it is missing - a first run, or someone deleted it
// to start over - the defaults below are used, and nothing is written until the
// operator saves from the settings panel.
type Settings struct {
	Port         int    `json:"port"`
	Dir          string `json:"dir"`
	Host         string `json:"host"`
	MaxMB        int64  `json:"max_mb"`
	Recent       int    `json:"recent"`
	Open         bool   `json:"open"`
	OpenHost     bool   `json:"open_host"`
	CheckUpdates bool   `json:"check_updates"`
	LanOnly      bool   `json:"lan_only"`
	InternetOnly bool   `json:"internet_only"`
	Public       string `json:"public"`
	PublicPort   int    `json:"public_port"`
	Token        string `json:"token"`
}

// The defaults, shared with the flag declarations so there is one source of
// truth for what "unconfigured" means.
const (
	defaultPort     = 8080
	defaultDir      = `C:\file-drop-server`
	defaultMaxMB    = 0
	defaultRecent   = 10
	defaultOpen     = true
	defaultOpenHost = true
	defaultChecks   = true

	settingsFile = "file-drop-server.toml"
	qrCodeFile   = "qr-code.png"
)

func defaultSettings() Settings {
	return Settings{
		Port:         defaultPort,
		Dir:          defaultDir,
		MaxMB:        defaultMaxMB,
		Recent:       defaultRecent,
		Open:         defaultOpen,
		OpenHost:     defaultOpenHost,
		CheckUpdates: defaultChecks,
	}
}

var (
	// settingsMu guards the live copy, which the settings panel can replace
	// while uploads are in flight.
	settingsMu sync.RWMutex
	live       Settings

	// startupSettings is what the server actually booted with, so the panel can
	// go on saying "restart to apply" after the file has been written.
	startupSettings Settings

	configPath string
	// configFound is set from a request goroutine when the panel first writes
	// the file, and read by the banner, so it carries its own guard.
	configFound atomic.Bool
)

// currentSettings hands back a snapshot. Settings holds only value types, so
// the copy can be read afterwards without holding the lock.
func currentSettings() Settings {
	settingsMu.RLock()
	defer settingsMu.RUnlock()
	return live
}

func setSettings(s Settings) {
	settingsMu.Lock()
	live = s
	settingsMu.Unlock()
}

// loadSettings works out what the server should start with. A missing file is
// not an error; a malformed one is, because quietly ignoring a file someone has
// just edited would be worse than refusing to start.
func loadSettings() error {
	path := strings.TrimSpace(*flagConfig)
	if path == "" {
		path = defaultConfigPath()
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	configPath = path

	s := defaultSettings()
	switch data, err := os.ReadFile(path); {
	case err == nil:
		values, err := parseTOML(string(data))
		if err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		if err := s.applyTOML(values); err != nil {
			return fmt.Errorf("%s: %v", path, err)
		}
		configFound.Store(true)
	case errors.Is(err, os.ErrNotExist):
		// First run, or the file was deleted on purpose: defaults it is.
	default:
		return fmt.Errorf("could not read %s: %v", path, err)
	}

	s.applyFlags()
	if err := s.normalise(); err != nil {
		return err
	}

	startupSettings = s
	setSettings(s)
	return nil
}

// defaultConfigPath puts the file beside the executable, so the whole thing
// stays portable: copy the .exe and its .toml to another machine and the
// settings travel with it.
func defaultConfigPath() string {
	exe, err := os.Executable()
	if err != nil {
		return settingsFile
	}
	return filepath.Join(filepath.Dir(exe), settingsFile)
}

// applyFlags lets the command line win over the file, but only for the flags
// actually typed - flag.Visit reports those, where iterating over all of them
// would overwrite every saved value with a default nobody asked for.
func (s *Settings) applyFlags() {
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "port":
			s.Port = *flagPort
		case "dir":
			s.Dir = *flagDir
		case "host":
			s.Host = *flagHost
		case "max":
			s.MaxMB = *flagMaxMB
		case "recent":
			s.Recent = *flagRecent
		case "open":
			s.Open = *flagOpen
		case "open-host":
			s.OpenHost = *flagOpenHost
		case "check-updates":
			s.CheckUpdates = *flagCheckUpdates
		case "lan-only":
			s.LanOnly = *flagLanOnly
		case "internet-only":
			s.InternetOnly = *flagInternetOnly
		case "public":
			s.Public = *flagPublic
		case "public-port":
			s.PublicPort = *flagPublicPort
		case "token":
			s.Token = *flagToken
		}
	})
}

// normalise tidies the values and rejects the ones that cannot work, so a bad
// setting is refused where it is entered rather than where it would have broken
// something.
func (s *Settings) normalise() error {
	s.Dir = strings.TrimSpace(s.Dir)
	s.Host = strings.TrimSpace(s.Host)
	s.Public = strings.TrimSpace(s.Public)
	s.Token = strings.TrimSpace(s.Token)

	if s.Dir == "" {
		s.Dir = defaultDir
	}
	abs, err := filepath.Abs(s.Dir)
	if err != nil {
		return fmt.Errorf("drop folder %q is not a usable path", s.Dir)
	}
	s.Dir = filepath.Clean(abs)

	if s.Port < 1 || s.Port > 65535 {
		return fmt.Errorf("port %d is outside 1-65535", s.Port)
	}
	if s.PublicPort < 0 || s.PublicPort > 65535 {
		return fmt.Errorf("internet port %d is outside 0-65535", s.PublicPort)
	}
	if s.PublicPort != 0 && s.PublicPort == s.Port {
		return errors.New("the internet port has to differ from the main port")
	}
	if s.MaxMB < 0 {
		return errors.New("the largest batch cannot be negative - use 0 for no limit")
	}
	// The page asks for this list every three seconds and each entry means
	// walking a batch folder, so the ceiling is there to stop a stray number
	// making the operator's own screen the slowest thing on the machine.
	if s.Recent < 1 || s.Recent > 500 {
		return fmt.Errorf("recent drops has to be between 1 and 500, not %d", s.Recent)
	}
	if strings.ContainsAny(s.Host, " \t/\\:") {
		return fmt.Errorf("%q is not an address - give a name or an IP, with no port", s.Host)
	}
	if s.Public != "" {
		u, err := url.Parse(s.Public)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("%q is not a full address - it should look like https://drop.example.com/", s.Public)
		}
	}
	if strings.ContainsAny(s.Token, " \t\r\n") {
		return errors.New("the access code cannot contain spaces - it travels inside a URL")
	}
	// Each one turns off the route the other keeps, so together they would ask
	// for a server nobody can reach.
	if s.LanOnly && s.InternetOnly {
		return errors.New("pick either local network only or internet only, not both")
	}
	return nil
}

// restartNeeded names the settings that have changed but cannot take effect
// until the server is started again: the listeners, the QR codes and the tunnel
// are all built once, at start-up.
func restartNeeded(was, now Settings) []string {
	var out []string
	add := func(changed bool, label string) {
		if changed {
			out = append(out, label)
		}
	}
	add(was.Port != now.Port, "port")
	add(was.Host != now.Host, "QR code address")
	add(was.LanOnly != now.LanOnly, "internet link")
	add(was.InternetOnly != now.InternetOnly, "local network link")
	add(was.Public != now.Public, "tunnel address")
	add(was.PublicPort != now.PublicPort, "internet port")
	add(was.Token != now.Token, "access code")
	return out
}

// save writes the file out whole, through a temporary file, so an interrupted
// write cannot leave half a settings file behind.
func (s Settings) save(path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, s.toTOML(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// toTOML renders the settings as a commented file, so what lands on disk is
// worth opening in an editor rather than a bare dump of key = value.
func (s Settings) toTOML() []byte {
	var b strings.Builder
	b.WriteString("# File Drop settings.\n")
	b.WriteString("#\n")
	b.WriteString("# Written by the cog on the /host page, and safe to edit by hand.\n")
	b.WriteString("# Delete this file to go back to the defaults. Command-line flags\n")
	b.WriteString("# still win over anything here, for the run they are passed on.\n\n")

	entry := func(comment, key, value string) {
		b.WriteString("# " + comment + "\n")
		b.WriteString(key + " = " + value + "\n\n")
	}

	entry("TCP port to listen on.",
		"port", strconv.Itoa(s.Port))
	entry("Root folder that receives the uploaded batches.",
		"dir", tomlString(s.Dir))
	entry("Address to encode in the QR code.\n# Empty auto-detects this machine's LAN address.",
		"host", tomlString(s.Host))
	entry("Largest single upload batch, in MB. 0 removes the limit.",
		"max_mb", strconv.FormatInt(s.MaxMB, 10))
	entry("How many of the newest drop folders /host lists.",
		"recent", strconv.Itoa(s.Recent))
	entry("Open each finished batch in Windows Explorer.",
		"open", strconv.FormatBool(s.Open))
	entry("Open the QR code page in a browser when the program starts.",
		"open_host", strconv.FormatBool(s.OpenHost))
	entry("Ask GitHub whether a newer release exists, once at start-up.",
		"check_updates", strconv.FormatBool(s.CheckUpdates))
	entry("Stay on the local network: publish no internet link at all.",
		"lan_only", strconv.FormatBool(s.LanOnly))
	entry("The other way round: serve the internet route only, and refuse\n# uploads from this local network. The two cannot both be true.",
		"internet_only", strconv.FormatBool(s.InternetOnly))
	entry("Public HTTPS address to advertise, if you run your own tunnel.\n# Empty starts a Cloudflare quick tunnel instead.",
		"public", tomlString(s.Public))
	entry("Local port the internet listener uses. 0 means port + 1.",
		"public_port", strconv.Itoa(s.PublicPort))
	entry("Access code for internet uploads.\n# Empty makes a fresh random one every start.",
		"token", tomlString(s.Token))

	return []byte(b.String())
}

// applyTOML lays the parsed file over the defaults. Unknown keys are ignored
// rather than rejected, so a file written by a later version still starts an
// earlier one.
func (s *Settings) applyTOML(values map[string]any) error {
	for key, value := range values {
		var err error
		switch key {
		case "port":
			err = assignInt(&s.Port, key, value)
		case "dir":
			err = assignString(&s.Dir, key, value)
		case "host":
			err = assignString(&s.Host, key, value)
		case "max_mb":
			err = assignInt64(&s.MaxMB, key, value)
		case "recent":
			err = assignInt(&s.Recent, key, value)
		case "open":
			err = assignBool(&s.Open, key, value)
		case "open_host":
			err = assignBool(&s.OpenHost, key, value)
		case "check_updates":
			err = assignBool(&s.CheckUpdates, key, value)
		case "lan_only":
			err = assignBool(&s.LanOnly, key, value)
		case "wifi_only":
			// What lan_only was called before. Honoured on the way in, because
			// dropping it would quietly start publishing an internet link for
			// someone whose saved answer was that they wanted none. A file
			// carrying both keys is decided by the current one. Saving from the
			// panel rewrites the file under the new name.
			if _, current := values["lan_only"]; !current {
				err = assignBool(&s.LanOnly, key, value)
				log.Printf("note: %s still says wifi_only; that setting is now called lan_only", configPath)
			}
		case "internet_only":
			err = assignBool(&s.InternetOnly, key, value)
		case "public":
			err = assignString(&s.Public, key, value)
		case "public_port":
			err = assignInt(&s.PublicPort, key, value)
		case "token":
			err = assignString(&s.Token, key, value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func assignString(dst *string, key string, value any) error {
	v, ok := value.(string)
	if !ok {
		return fmt.Errorf("%s should be quoted text", key)
	}
	*dst = v
	return nil
}

func assignBool(dst *bool, key string, value any) error {
	v, ok := value.(bool)
	if !ok {
		return fmt.Errorf("%s should be true or false", key)
	}
	*dst = v
	return nil
}

func assignInt64(dst *int64, key string, value any) error {
	v, ok := value.(int64)
	if !ok {
		return fmt.Errorf("%s should be a whole number", key)
	}
	*dst = v
	return nil
}

func assignInt(dst *int, key string, value any) error {
	var v int64
	if err := assignInt64(&v, key, value); err != nil {
		return err
	}
	*dst = int(v)
	return nil
}

// parseTOML reads the slice of TOML these settings need: one flat table of
// key = value pairs, where a value is quoted text, a whole number or a boolean.
// That is the whole shape of the file, and it keeps the program dependency-free
// for the sake of nine lines of configuration.
func parseTOML(src string) (map[string]any, error) {
	out := map[string]any{}
	for n, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			// Nothing here lives in a table. Skip a header rather than failing,
			// so a hand-edited file that grew one still starts the server.
			continue
		}
		key, rest, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key = value", n+1)
		}
		value, err := parseTOMLValue(strings.TrimSpace(rest))
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", n+1, err)
		}
		out[strings.TrimSpace(key)] = value
	}
	return out, nil
}

func parseTOMLValue(s string) (any, error) {
	if s == "" {
		return nil, errors.New("nothing after the =")
	}
	switch s[0] {
	case '"':
		return parseTOMLBasicString(s)
	case '\'':
		// A literal string: no escapes, so it ends at the next quote.
		if i := strings.IndexByte(s[1:], '\''); i >= 0 {
			return s[1 : 1+i], nil
		}
		return nil, errors.New("a quote is missing from the end of that value")
	}

	// Anything unquoted may be followed by a trailing comment.
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(s, "_", ""), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%q is not quoted text, a whole number or true/false", s)
	}
	return n, nil
}

// parseTOMLBasicString reads a "quoted" value. Bytes are copied straight
// through apart from the escapes, so UTF-8 in a folder name survives.
func parseTOMLBasicString(s string) (string, error) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			return b.String(), nil
		case '\\':
			i++
			if i >= len(s) {
				return "", errors.New("that value ends in a stray backslash")
			}
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				return "", fmt.Errorf("\\%c is not an escape TOML knows", s[i])
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", errors.New("a quote is missing from the end of that value")
}

// tomlString quotes a value for the file. Windows paths are full of
// backslashes, which is exactly what a basic string has to escape.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
