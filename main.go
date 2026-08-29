// file-drop-server serves a mobile-friendly upload page on your local network.
//
// Point a phone's camera at the QR code printed on start-up (or shown at /host),
// pick some files, hit upload, and every batch lands in its own timestamped
// folder under the drop root, e.g. C:\file-drop-server\2026-08-29_16-54-33\
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/crc32"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	_ "embed"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/host.html
var hostHTMLSrc string

// Every flag here has a twin in the settings file, and the two meet in
// Settings.applyFlags: the file supplies the values, a flag typed on the
// command line overrides its own for that run only.
var (
	flagPort         = flag.Int("port", defaultPort, "TCP port to listen on")
	flagDir          = flag.String("dir", defaultDir, "root folder that receives the uploaded batches")
	flagHost         = flag.String("host", "", "address to encode in the QR code (auto-detected LAN IP when empty)")
	flagMaxMB        = flag.Int64("max", defaultMaxMB, "maximum size of a single upload batch in MB (0 for no limit)")
	flagRecent       = flag.Int("recent", defaultRecent, "how many of the newest drop folders /host lists")
	flagOpen         = flag.Bool("open", defaultOpen, "open each finished batch in Windows Explorer")
	flagOpenHost     = flag.Bool("open-host", defaultOpenHost, "open the QR code page in a browser at start-up")
	flagCheckUpdates = flag.Bool("check-updates", defaultChecks, "ask GitHub for a newer release at start-up")
	flagVersion      = flag.Bool("version", false, "print the version and exit")

	flagLanOnly      = flag.Bool("lan-only", false, "stay on the local network: do not publish an internet link")
	flagInternetOnly = flag.Bool("internet-only", false, "serve the internet route only: refuse uploads from the local network")
	flagPublic       = flag.String("public", "", "public HTTPS address to advertise, if you run your own tunnel")
	flagPublicPort   = flag.Int("public-port", 0, "local port the internet listener uses (defaults to -port + 1)")
	flagToken        = flag.String("token", "", "access code for internet uploads (a random one is made when empty)")

	flagConfig = flag.String("config", "", "settings file to read and save (defaults to "+settingsFile+" beside the program)")
)

// Cloudflare's free tier refuses request bodies over 100 MB, so uploads that
// come in over the tunnel are held to that whatever -max says.
const cloudflareBodyLimit = 100 << 20

// maxBytesKey carries the per-route upload ceiling on the request context.
type maxBytesKey struct{}

// batchMu serialises batch-folder creation so two phones uploading in the same
// second cannot land in the same directory.
var batchMu sync.Mutex

func main() {
	useUTF8Console()
	adoptChildProcesses()
	flag.Parse()
	log.SetFlags(log.Ltime)

	if *flagVersion {
		fmt.Println(version)
		return
	}

	// The copy an update left behind, which could not be removed at the time
	// because it was the program doing the removing.
	tidyPreviousUpdate()

	if err := loadSettings(); err != nil {
		log.Fatalf("%v", err)
	}
	cfg := currentSettings()

	// Nothing waits on this: an unreachable GitHub must not delay or stop a
	// server whose whole job is on the local network.
	if cfg.CheckUpdates {
		go checkForUpdate()
	}

	root := cfg.Dir
	if err := os.MkdirAll(root, 0o755); err != nil {
		log.Fatalf("cannot create drop folder %s: %v", root, err)
	}

	host := cfg.Host
	if host == "" {
		if cfg.InternetOnly {
			// Nothing is served to the local network, so there is no LAN
			// address worth finding and no reason to refuse to start for the
			// want of one.
			host = "localhost"
		} else {
			var err error
			host, err = lanIP()
			if err != nil {
				log.Fatalf("could not work out this machine's LAN address (%v) - pass one with -host", err)
			}
		}
	}
	publicURL := fmt.Sprintf("http://%s:%d/", host, cfg.Port)

	qr, err := qrcode.New(publicURL, qrcode.Medium)
	if err != nil {
		log.Fatalf("could not build the QR code: %v", err)
	}
	qrPNG, err := qr.PNG(512)
	if err != nil {
		log.Fatalf("could not render the QR code: %v", err)
	}

	// Keep a printable copy on disk so the code can be stuck on a wall once and
	// reused indefinitely, as long as this machine keeps the same address and port.
	saveQRCode := func(dir string) string {
		if cfg.InternetOnly {
			// It would encode an address nothing on the network answers.
			return ""
		}
		path := filepath.Join(dir, qrCodeFile)
		if err := os.WriteFile(path, qrPNG, 0o644); err != nil {
			log.Printf("warning: could not save %s: %v", path, err)
		}
		return path
	}
	qrPath := saveQRCode(root)

	hostTmpl := template.Must(template.New("host").Parse(hostHTMLSrc))

	// Both servers and the tunnel are set up further down, but the restart
	// handler has to be able to take all three down, so the names exist before
	// the routes do.
	var srv, gatedSrv *http.Server
	var tunnel *exec.Cmd

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(indexHTML)
	})

	// The internet details arrive later, once the tunnel has come up, so the
	// operator's screen reads them behind a lock.
	var (
		hostMu      sync.RWMutex
		internetURL string
		internetQR  []byte
	)

	// Whether an internet link is on its way at all. Written once below, before
	// this server starts accepting, and only read afterwards. Local network only
	// leaves it false, and so does a listener that could not bind: in both cases
	// there is nothing to wait for, and the page should not offer a second code.
	tunnelPending := false

	// Set when the internet route is wanted but the client for it is absent.
	// Unlike the other reasons there is something the operator can do about it
	// without leaving the page, so /host says so and offers to install it.
	cloudflaredMissing := false

	hostSnapshot := func() map[string]string {
		hostMu.RLock()
		defer hostMu.RUnlock()
		data := map[string]string{
			"URL":     publicURL,
			"Root":    currentSettings().Dir,
			"Version": version,
			"Repo":    "https://github.com/" + updateRepo,
			// Releases are where the changelog actually lives: every one of
			// them carries its notes.
			"Changelog": "https://github.com/" + updateRepo + "/releases",
		}
		if cfg.InternetOnly {
			data["LanOff"] = "yes"
		}
		switch {
		case internetURL != "":
			data["InternetURL"] = internetURL
			data["InternetHost"] = strings.TrimPrefix(strings.SplitN(internetURL, "/?k=", 2)[0], "https://")
			data["InternetLimit"] = humanSize(cloudflareBodyLimit)
		case tunnelPending:
			data["Pending"] = "yes"
		case cloudflaredMissing:
			data["NoCloudflared"] = "yes"
			if cloudflaredInstallSupported {
				data["CanInstall"] = "yes"
			}
		}
		return data
	}

	mux.HandleFunc("/host", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		hostTmpl.Execute(w, hostSnapshot())
	})

	// Lets the operator's screen notice the tunnel finishing after page load.
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		hostMu.RLock()
		ready := internetURL != ""
		hostMu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]any{"internet": ready})
	})

	mux.HandleFunc("/qr.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(qrPNG)
	})

	mux.HandleFunc("/upload", handleUpload)

	mux.HandleFunc("/batches", func(w http.ResponseWriter, r *http.Request) {
		cfg := currentSettings()
		writeJSON(w, http.StatusOK, map[string]any{"batches": recentBatches(cfg.Dir, cfg.Recent)})
	})

	// The firewall button at the top of the settings panel. Like /settings and
	// /open it is absent from publicGate's allow-list and so is local-only: a
	// POST here raises an administrator prompt on this desktop, which only the
	// person sitting at it can answer.
	mux.HandleFunc("/firewall", func(w http.ResponseWriter, r *http.Request) {
		port := currentSettings().Port
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, firewallStatus(port))

		case http.MethodPost:
			log.Printf("asking Windows to allow this program through the firewall")
			if err := firewallAllow(port); err != nil {
				log.Printf("firewall rule not added: %v", err)
				// The status still goes back, so the panel can show where
				// things actually stand rather than only what failed.
				report := firewallStatus(port)
				report.Error = err.Error()
				writeJSON(w, http.StatusOK, report)
				return
			}
			log.Printf("firewall rule added")
			writeJSON(w, http.StatusOK, firewallStatus(port))

		default:
			http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		}
	})

	// The update badge in the corner of /host, and the button behind it. Both
	// are local-only: this replaces the program on disk.
	mux.HandleFunc("/update", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, currentUpdateState())

		case http.MethodPost:
			log.Printf("downloading the update")
			if err := downloadUpdate(); err != nil {
				log.Printf("update not applied: %v", err)
				state := currentUpdateState()
				state.Error = err.Error()
				setUpdateState(state)
				writeJSON(w, http.StatusOK, state)
				return
			}
			state := currentUpdateState()
			state.Ready = true
			state.Error = ""
			setUpdateState(state)
			writeJSON(w, http.StatusOK, state)

		default:
			http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		}
	})

	// Offered by the "Send over Internet" tile when the client is missing.
	// Local-only, like everything else that acts on this desktop.
	mux.HandleFunc("/install-cloudflared", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		log.Printf("installing cloudflared with winget")
		if err := installCloudflared(); err != nil {
			log.Printf("cloudflared not installed: %v", err)
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		// The tunnel is only ever started at boot, so the running server cannot
		// pick this up by itself however new the client is.
		log.Printf("cloudflared installed - restart to publish the internet link")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart": true})
	})

	// Restarting is how a setting that needs it actually takes effect, without
	// the operator going back to the terminal they may no longer have.
	mux.HandleFunc("/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if !restartSupported {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": false, "error": "restarting from the page is only supported on Windows"})
			return
		}

		log.Printf("restarting")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

		// Answer first and tear down afterwards: the reply travels over a
		// connection this is about to close.
		go func() {
			time.Sleep(400 * time.Millisecond)
			if tunnel != nil {
				tunnel.Process.Kill()
			}
			// Both ports have to be free before the replacement asks for them.
			srv.Close()
			if gatedSrv != nil {
				gatedSrv.Close()
			}
			time.Sleep(200 * time.Millisecond)

			if err := relaunch(restartArgs()); err != nil {
				// Nothing is listening any more, so say plainly that it needs
				// starting by hand rather than sit here looking alive.
				log.Fatalf("could not restart: %v - please start it again yourself", err)
			}
			os.Exit(0)
		}()
	})

	// The settings panel behind the cog on /host. It is deliberately absent
	// from publicGate's allow-list, so it exists on the local network only.
	mux.HandleFunc("/settings", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			current := currentSettings()
			writeJSON(w, http.StatusOK, map[string]any{
				"settings": current,
				"defaults": defaultSettings(),
				"path":     configPath,
				"saved":    configFound.Load(),
				"firewall": firewallSupported,
				"restart":  restartNeeded(startupSettings, current),
			})

		case http.MethodPost:
			var in Settings
			// A settings post is a handful of short fields; anything larger is
			// not one of ours.
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "those settings were not readable"})
				return
			}
			if err := in.normalise(); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			// Prove the folder is usable before saving a setting that would
			// send every future batch somewhere that cannot be written.
			if err := os.MkdirAll(in.Dir, 0o755); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": "cannot use " + in.Dir + ": " + err.Error()})
				return
			}
			if err := in.save(configPath); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": "could not write " + configPath + ": " + err.Error()})
				return
			}

			was := currentSettings()
			setSettings(in)
			configFound.Store(true)
			log.Printf("settings saved to %s", configPath)
			if in.Dir != was.Dir {
				log.Printf("uploads now land in %s", in.Dir)
				saveQRCode(in.Dir)
			}

			writeJSON(w, http.StatusOK, map[string]any{
				"ok":       true,
				"settings": in,
				"path":     configPath,
				"restart":  restartNeeded(startupSettings, in),
			})

		default:
			http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
		}
	})

	// Opens a batch folder for whoever is sitting at this machine. A browser
	// will not follow a file:// link from an http:// page, so the click comes
	// back here and the server opens Explorer itself.
	mux.HandleFunc("/open", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("folder")
		if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
			http.Error(w, "not a batch folder name", http.StatusBadRequest)
			return
		}
		root := currentSettings().Dir
		dir := filepath.Join(root, name)
		if !within(root, dir) {
			http.Error(w, "not a batch folder name", http.StatusBadRequest)
			return
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			http.Error(w, "no such batch", http.StatusNotFound)
			return
		}
		log.Printf("opening %s", name)
		openFolder(dir)
		w.WriteHeader(http.StatusNoContent)
	})

	// The internet route, unless asked to stay local. It listens only on
	// loopback: the tunnel is the sole way in, so public traffic always meets
	// the access gate while people on the LAN carry on using the plain address.
	//
	// Why there is no internet link, when there is none to be had. Knowing this
	// before the banner prints is the difference between saying the link is on
	// its way and admitting it is not coming.
	noInternet := ""

	// Look for cloudflared up front rather than inside the goroutine that
	// starts it. It is an instant, harmless check, and doing it here means a
	// missing client is known in time for both the banner and /host, instead of
	// being discovered a moment after both have promised a second QR code.
	// Skipped when the operator runs their own tunnel: that needs no client.
	tunnelBin := ""
	if !cfg.LanOnly && cfg.Public == "" {
		bin, err := cloudflaredPath()
		if err != nil {
			noInternet = "cloudflared is not installed"
			cloudflaredMissing = true
			log.Printf("no internet link: %v", err)
			log.Printf("local uploads are unaffected; use -lan-only to stop trying")
		}
		tunnelBin = bin
	}

	if !cfg.LanOnly && noInternet == "" {
		token := cfg.Token
		if token == "" {
			token = randomToken()
		}

		publicPort := cfg.PublicPort
		if publicPort == 0 {
			publicPort = cfg.Port + 1
		}
		gated := &http.Server{
			Handler:           publicGate(token, logRequests(mux)),
			ReadHeaderTimeout: 20 * time.Second,
		}
		// Kept so a restart can give this port back before the replacement
		// tries to take it.
		gatedSrv = gated

		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort))
		if err != nil {
			// Not fatal: the local network is the main event and must still work.
			noInternet = fmt.Sprintf("port %d is not free", publicPort)
			log.Printf("no internet link - port %d is not free (%v). Local uploads are unaffected.", publicPort, err)
		} else {
			go func() {
				if err := gated.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Printf("internet listener stopped: %v", err)
				}
			}()

			publish := func(base string) {
				url := strings.TrimRight(base, "/") + "/?k=" + token
				var png []byte
				if code, err := qrcode.New(url, qrcode.Medium); err == nil {
					png, _ = code.PNG(512)
				}
				hostMu.Lock()
				internetURL, internetQR = url, png
				hostMu.Unlock()
			}

			if cfg.Public != "" {
				publish(cfg.Public)
			} else {
				// Bringing the tunnel up takes the better part of a minute, so it
				// happens in the background: the LAN page is usable immediately and
				// the second QR code appears on /host when it is ready.
				tunnelPending = true
				go func() {
					base, cmd, err := startTunnel(tunnelBin, publicPort)
					if err != nil {
						log.Printf("no internet link: %v", err)
						log.Printf("local uploads are unaffected; use -lan-only to stop trying")
						return
					}
					tunnel = cmd
					publish(base)
					log.Printf("internet link ready: %s", base)
				}()
			}
		}
	}

	mux.HandleFunc("/qr-internet.png", func(w http.ResponseWriter, r *http.Request) {
		hostMu.RLock()
		png := internetQR
		hostMu.RUnlock()
		if png == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	})

	// Internet only binds loopback: the operator's own screen still works, but
	// nothing on the local network can reach the upload page or /upload, and the
	// tunnel remains the sole way in.
	addr := fmt.Sprintf(":%d", cfg.Port)
	if cfg.InternetOnly {
		addr = fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	}

	srv = &http.Server{
		Addr:              addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 20 * time.Second,
		// No read/write timeouts on purpose: a phone on a weak signal can spend a
		// long while pushing a batch of videos, and cutting it off mid-upload
		// would leave half-written files behind.
	}

	if !cfg.InternetOnly {
		fmt.Print("\n" + qr.ToSmallString(false) + "\n")
	} else {
		fmt.Print("\n")
	}
	fmt.Printf("  File Drop is running\n\n")
	fmt.Printf("  Version:                       %s\n", version)
	if cfg.InternetOnly {
		fmt.Printf("  Send over Local Area Network:  off (internet only)\n")
	} else {
		fmt.Printf("  Send over Local Area Network:  %s\n", publicURL)
	}
	switch {
	case cfg.LanOnly:
		fmt.Printf("  Send over Internet:            off (local network only)\n")
	case noInternet != "":
		fmt.Printf("  Send over Internet:            off (%s)\n", noInternet)
	case tunnelPending:
		fmt.Printf("  Send over Internet:            starting, appears on /host shortly\n")
	default:
		hostMu.RLock()
		fmt.Printf("  Send over Internet:            %s\n", internetURL)
		hostMu.RUnlock()
	}
	hostURL := fmt.Sprintf("http://localhost:%d/host", cfg.Port)
	if cfg.OpenHost {
		fmt.Printf("  Opening in your browser:       %s\n", hostURL)
	} else {
		fmt.Printf("  Both codes and the settings:   %s\n", hostURL)
	}
	if qrPath != "" {
		fmt.Printf("  Printable QR image:            %s\n", qrPath)
	}
	fmt.Printf("  Uploads land in:               %s\\<date>_<time>\\\n", root)
	if configFound.Load() {
		fmt.Printf("  Settings file:                 %s\n", configPath)
	} else {
		fmt.Printf("  Settings file:                 %s (defaults - not written yet)\n", configPath)
	}
	if !cfg.LanOnly && noInternet == "" {
		fmt.Printf("\n  The internet address changes every restart, and Cloudflare caps\n")
		if cfg.InternetOnly {
			fmt.Printf("  uploads at %s over that route.\n", humanSize(cloudflareBodyLimit))
		} else {
			fmt.Printf("  uploads at %s over that route. The local one is unlimited.\n", humanSize(cloudflareBodyLimit))
		}
	}
	fmt.Printf("\n  Press Ctrl+C to stop.\n\n")

	// Stop the tunnel with us, rather than leaving cloudflared running.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	go func() {
		<-stop
		if tunnel != nil {
			log.Printf("closing the tunnel")
			tunnel.Process.Kill()
		}
		srv.Close()
		os.Exit(0)
	}()

	// Bind before opening the browser rather than handing the whole job to
	// ListenAndServe: once this returns the socket is accepting, so the page
	// cannot be asked for a moment before there is anything to answer it.
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		if tunnel != nil {
			tunnel.Process.Kill()
		}
		log.Fatalf("cannot listen on port %d: %v", cfg.Port, err)
	}

	if cfg.OpenHost {
		openBrowser(hostURL)
	}

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		if tunnel != nil {
			tunnel.Process.Kill()
		}
		log.Fatalf("server stopped: %v", err)
	}

	// The server was closed on purpose: either Ctrl+C or the Restart button.
	// Both finish by calling os.Exit themselves, and both are still working
	// when Serve returns - returning from main here would kill the goroutine
	// doing that work, which is exactly how a restart loses its replacement.
	// The wait is bounded so a shutdown that never completes still ends.
	time.Sleep(30 * time.Second)
	log.Printf("shutdown did not finish; exiting anyway")
}

// handleUpload streams each part of the multipart body straight to disk, so a
// batch of large videos never has to fit in memory.
func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	// One snapshot for the whole batch: a setting changed mid-upload must not
	// land half of it in the old folder and half in the new one.
	cfg := currentSettings()
	root := cfg.Dir

	limit := cfg.MaxMB * 1024 * 1024
	if routeLimit, ok := r.Context().Value(maxBytesKey{}).(int64); ok {
		// The internet route has a ceiling of its own; take the tighter of the two.
		if limit == 0 || routeLimit < limit {
			limit = routeLimit
		}
	}
	if limit > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}

	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "that request was not a file upload"})
		return
	}

	var (
		dir      string
		saved    []savedFile
		written  int64
		verified int
		wantCRC  string
		wantPath string
		taken    = map[string]bool{}
	)

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			discardBatch(dir)
			if isTooBig(err) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": tooBigMessage(limit)})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the upload was interrupted: " + err.Error()})
			return
		}
		if part.FileName() == "" {
			// The page sends "path" and "crc" fields immediately before each
			// file, so the parts arrive in matched groups.
			switch part.FormName() {
			case "crc":
				value, _ := io.ReadAll(io.LimitReader(part, 32))
				wantCRC = strings.TrimSpace(string(value))
			case "path":
				// Carried separately because Go's multipart reader strips
				// directories out of a part's filename, as RFC 7578 requires.
				value, _ := io.ReadAll(io.LimitReader(part, 4096))
				wantPath = string(value)
			}
			part.Close()
			continue
		}

		// Only make the folder once we know there is a real file in the batch.
		if dir == "" {
			dir, err = newBatchDir(root)
			if err != nil {
				part.Close()
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not create the folder for this batch"})
				log.Printf("mkdir failed: %v", err)
				return
			}
			log.Printf("new batch -> %s", dir)
			// Reserve the manifest's name so an uploaded file cannot clobber it.
			taken[strings.ToLower(sfvName)] = true
		}

		// The page sends each file's path relative to the folder that was
		// picked, so an uploaded tree is rebuilt rather than flattened.
		rel := wantPath
		if rel == "" {
			rel = part.FileName()
		}
		name := uniqueName(safeRelPath(rel), taken)
		full := filepath.Join(dir, name)
		if !within(dir, full) {
			part.Close()
			discardBatch(dir)
			log.Printf("rejected: %q resolves outside the batch folder", part.FileName())
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "that upload contained an unusable file path"})
			return
		}
		if parent := filepath.Dir(full); parent != dir {
			if err := os.MkdirAll(parent, 0o755); err != nil {
				part.Close()
				discardBatch(dir)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not recreate the folder structure"})
				log.Printf("mkdir %s failed: %v", parent, err)
				return
			}
		}
		dst, err := os.Create(full)
		if err != nil {
			part.Close()
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not save " + name})
			log.Printf("create %s failed: %v", name, err)
			return
		}
		// Checksum the bytes on their way to disk, so verifying costs no extra
		// read and nothing has to be buffered.
		sum := crc32.NewIEEE()
		n, copyErr := io.Copy(io.MultiWriter(dst, sum), part)
		closeErr := dst.Close()
		part.Close()
		if copyErr != nil || closeErr != nil {
			discardBatch(dir)
			if isTooBig(copyErr) {
				log.Printf("rejected: batch over the %d MB limit", cfg.MaxMB)
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": tooBigMessage(limit)})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the upload was cut short - please try again"})
			log.Printf("write %s failed: %v / %v", name, copyErr, closeErr)
			return
		}

		gotCRC := fmt.Sprintf("%08x", sum.Sum32())
		switch {
		case wantCRC == "":
			// An older browser that could not hash locally; the file is still
			// protected from truncation, just not from corruption.
			log.Printf("  saved %s (%s) - no checksum sent", name, humanSize(n))
		case !strings.EqualFold(wantCRC, gotCRC):
			log.Printf("CHECKSUM MISMATCH on %s: phone said %s, received %s - discarding the batch", name, wantCRC, gotCRC)
			discardBatch(dir)
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": name + " arrived corrupted - nothing was kept, please send the batch again",
			})
			return
		default:
			verified++
			log.Printf("  saved %s (%s) crc32 %s ok", name, humanSize(n), gotCRC)
		}
		wantCRC, wantPath = "", ""

		saved = append(saved, savedFile{Name: name, CRC: gotCRC})
		written += n
	}

	if len(saved) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no files were attached"})
		return
	}

	if err := writeSFV(dir, saved); err != nil {
		// The files themselves are intact, so keep the batch and just say so.
		log.Printf("warning: could not write %s in %s: %v", sfvName, filepath.Base(dir), err)
	}

	names := make([]string, len(saved))
	for i, f := range saved {
		names[i] = f.Name
	}

	log.Printf("batch complete: %d file(s), %s, %d verified, in %s",
		len(saved), humanSize(written), verified, filepath.Base(dir))

	// Only once the batch is complete and checksum-clean, so a window never
	// opens on files that are about to be thrown away.
	if cfg.Open {
		openFolder(dir)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"folder":   filepath.Base(dir),
		"path":     dir,
		"count":    len(saved),
		"bytes":    written,
		"files":    names,
		"verified": verified,
	})
}

// randomToken makes the access code that guards the internet route.
func randomToken() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("could not generate an access code: %v", err)
	}
	return hex.EncodeToString(b)
}

// publicGate fronts the internet-facing listener. The code travels in the QR
// code, so a client never types it: the first request carries ?k=, which is
// swapped for a cookie and redirected away so the address bar stays clean.
func publicGate(token string, next http.Handler) http.Handler {
	// Everything else - the operator's screen, the batch listing, and above all
	// /open, which puts windows on this desktop - stays off the internet route.
	allowed := map[string]bool{"/": true, "/upload": true}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.URL.Path] {
			http.NotFound(w, r)
			return
		}

		fromQuery := r.URL.Query().Get("k") == token
		cookie, _ := r.Cookie("filedrop")
		fromCookie := cookie != nil && cookie.Value == token

		if !fromQuery && !fromCookie {
			log.Printf("internet request without a valid code: %s %s", r.Method, r.URL.Path)
			http.Error(w, "This link is not valid. Ask for a fresh one.", http.StatusForbidden)
			return
		}

		if fromQuery {
			http.SetCookie(w, &http.Cookie{
				Name: "filedrop", Value: token, Path: "/",
				HttpOnly: true, SameSite: http.SameSiteLaxMode,
			})
			// Drop the code from the address bar on the initial page load.
			if r.Method == http.MethodGet && r.URL.Path == "/" {
				http.Redirect(w, r, "/", http.StatusFound)
				return
			}
		}

		ctx := context.WithValue(r.Context(), maxBytesKey{}, int64(cloudflareBodyLimit))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

var tunnelURLPattern = regexp.MustCompile(`https://[a-z0-9][a-z0-9-]*\.trycloudflare\.com`)

// restartArgs repeats this run's arguments for the replacement, with the
// browser suppressed. Whoever pressed Restart is looking at the page already
// and it reloads itself when the server answers again; opening a second tab
// would be the only visible effect. The flag is appended rather than filtered,
// because a later occurrence is the one the flag package keeps.
func restartArgs() []string {
	return append(append([]string{}, os.Args[1:]...), "-open-host=false")
}

// cloudflaredPath finds the tunnel client. It is looked up before anything is
// promised about an internet link, so that a machine without it says so at
// once rather than after a placeholder has already gone up on /host.
func cloudflaredPath() (string, error) {
	if bin, err := exec.LookPath("cloudflared"); err == nil {
		return bin, nil
	}
	// Not on our PATH is not the same as not installed. An installer that adds
	// itself to the PATH writes that to the registry; this process inherited
	// its copy when it started and will never see the change - and so will
	// every process it goes on to spawn. Re-read the registry once before
	// concluding that something sitting on the disk is missing.
	refreshExecPathOnce()
	if bin, err := exec.LookPath("cloudflared"); err == nil {
		return bin, nil
	}
	return "", errors.New("cloudflared is not installed - run: winget install Cloudflare.cloudflared")
}

// startTunnel runs cloudflared against the internet listener and waits for it
// to report the public address it was given.
func startTunnel(bin string, port int) (string, *exec.Cmd, error) {
	cmd := exec.Command(bin, "tunnel", "--no-autoupdate", "--url",
		fmt.Sprintf("http://127.0.0.1:%d", port))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}

	// cloudflared announces the address on stderr, but watch both streams so a
	// change of behaviour between versions does not leave us hanging.
	found := make(chan string, 1)
	watch := func(r io.Reader) {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			if match := tunnelURLPattern.FindString(scanner.Text()); match != "" {
				select {
				case found <- match:
				default:
				}
			}
		}
	}
	go watch(stdout)
	go watch(stderr)

	select {
	case url := <-found:
		return url, cmd, nil
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		return "", nil, errors.New("cloudflared did not report a public address within a minute")
	}
}

// sfvName is the checksum manifest dropped into every batch folder.
const sfvName = "checksums.sfv"

type savedFile struct {
	Name string
	CRC  string
}

// writeSFV records the CRC-32 of every file in the batch, in the order they
// arrived, with the checksums lined up in a column:
//
//	main.go              c4bb57ac
//	photos\june\IMG.jpg  23d37aab
//
// Files inside an uploaded folder are listed by their path within the batch.
// The values are what the server read back off its own write, so the file can
// be checked later with any SFV tool to catch disk rot.
func writeSFV(dir string, files []savedFile) error {
	width := 0
	for _, f := range files {
		if n := len([]rune(f.Name)); n > width {
			width = n
		}
	}

	var b strings.Builder
	for _, f := range files {
		pad := width - len([]rune(f.Name))
		b.WriteString(f.Name)
		b.WriteString(strings.Repeat(" ", pad+1))
		b.WriteString(f.CRC)
		b.WriteString("\r\n")
	}
	return os.WriteFile(filepath.Join(dir, sfvName), []byte(b.String()), 0o644)
}

// isTooBig reports whether an upload ran into the -max limit rather than a
// genuine network or disk failure.
func isTooBig(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

func tooBigMessage(limit int64) string {
	return fmt.Sprintf("that batch is over the %s limit for this connection - try sending it in a few smaller goes",
		humanSize(limit))
}

// newBatchDir makes C:\file-drop-server\2026-08-29_16-54-33, adding a -2, -3
// suffix if that second is already spoken for. Colons are illegal in Windows
// paths, so the time is separated with hyphens.
func newBatchDir(root string) (string, error) {
	batchMu.Lock()
	defer batchMu.Unlock()

	stamp := time.Now().Format("2006-01-02_15-04-05")
	for i := 1; ; i++ {
		name := stamp
		if i > 1 {
			name = fmt.Sprintf("%s-%d", stamp, i)
		}
		dir := filepath.Join(root, name)
		err := os.Mkdir(dir, 0o755)
		if err == nil {
			return dir, nil
		}
		if !os.IsExist(err) {
			return "", err
		}
	}
}

// discardBatch throws away everything a failed upload wrote. A batch is all or
// nothing: a folder that exists on disk is always complete and checksum-clean,
// so there is never a half-batch to tell apart from the client's retry.
func discardBatch(dir string) {
	if dir == "" {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("could not clean up %s: %v", dir, err)
	}
}

var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// within reports whether path sits inside root. safeRelPath should already
// guarantee this; the check is here so a bug in it cannot become a way to write
// anywhere on the disk.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// maxDepth and maxRelLen keep a hostile or simply absurd folder tree from
// producing paths Windows cannot open.
const (
	maxDepth  = 16
	maxRelLen = 200
)

// safeRelPath turns the relative path a browser reports for a file inside an
// uploaded folder ("photos/june/IMG_01.jpg") into a path that recreates that
// structure inside the batch folder and cannot climb out of it.
func safeRelPath(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")

	var parts []string
	for _, segment := range strings.Split(name, "/") {
		segment = strings.TrimSpace(segment)
		if segment == "" || segment == "." || segment == ".." {
			continue // "..", and anything else that would escape, is dropped
		}
		if clean := safeSegment(segment); clean != "" {
			parts = append(parts, clean)
		}
	}

	if len(parts) == 0 {
		return "file"
	}
	if len(parts) > maxDepth {
		parts = parts[len(parts)-maxDepth:] // keep the end nearest the file
	}

	rel := filepath.Join(parts...)
	if len([]rune(rel)) > maxRelLen {
		return parts[len(parts)-1] // too deep to rebuild; drop it in the root
	}
	return rel
}

// safeSegment reduces one path component to something Windows will accept.
func safeSegment(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20, r == 0x7f:
			// drop control characters
		case strings.ContainsRune("<>:\"/\\|?*", r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	name = strings.TrimRight(b.String(), " .")

	if name == "" || name == "." || name == ".." {
		return ""
	}
	stem := name
	if i := strings.IndexByte(name, '.'); i > 0 {
		stem = name[:i]
	}
	if reservedNames[strings.ToLower(stem)] {
		name = "_" + name
	}
	if len(name) > 180 {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		name = name[:180-len(ext)] + ext
	}
	return name
}

// uniqueName keeps two files called IMG_0001.jpg from overwriting each other in
// the same folder. Files of the same name in *different* folders of an uploaded
// tree are already distinct, because the whole relative path is the key.
func uniqueName(rel string, taken map[string]bool) string {
	if !taken[strings.ToLower(rel)] {
		taken[strings.ToLower(rel)] = true
		return rel
	}
	dir, base := filepath.Split(rel)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 2; ; i++ {
		candidate := dir + fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if !taken[strings.ToLower(candidate)] {
			taken[strings.ToLower(candidate)] = true
			return candidate
		}
	}
}

type batch struct {
	Folder string `json:"folder"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
}

// recentBatches lists the newest drop folders for the operator's /host screen.
func recentBatches(root string, limit int) []batch {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	// Folder names are timestamps, so a reverse string sort is newest-first.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	if len(names) > limit {
		names = names[:limit]
	}

	out := make([]batch, 0, len(names))
	for _, name := range names {
		// Walk the tree: a batch can contain uploaded folders, not just files.
		b := batch{Folder: name}
		batchRoot := filepath.Join(root, name)
		filepath.WalkDir(batchRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if filepath.Dir(path) == batchRoot && strings.EqualFold(d.Name(), sfvName) {
				return nil // the manifest is not one of the client's files
			}
			if info, err := d.Info(); err == nil {
				b.Files++
				b.Bytes += info.Size()
			}
			return nil
		})
		out = append(out, b)
	}
	return out
}

// lanIP picks the address other devices on the network can reach, preferring
// ordinary home and office ranges over virtual adapters.
func lanIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	best := ""
	bestRank := 99
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			rank := 3
			switch {
			case ip[0] == 192 && ip[1] == 168:
				rank = 0
			case ip[0] == 10:
				rank = 1
			case ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31:
				rank = 2
			}
			if rank < bestRank {
				bestRank, best = rank, ip.String()
			}
		}
	}
	if best == "" {
		return "", errors.New("no active network adapter with an IPv4 address")
	}
	return best, nil
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/upload" {
			log.Printf("upload starting from %s", clientIP(r))
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
