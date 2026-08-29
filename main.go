// file-drop-server serves a mobile-friendly upload page on your local network.
//
// Point a phone's camera at the QR code printed on start-up (or shown at /host),
// pick some files, hit upload, and every batch lands in its own timestamped
// folder under the drop root, e.g. C:\file-drop-server\2026-08-29_16-54-33\
package main

import (
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
	"path/filepath"
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

var (
	flagPort  = flag.Int("port", 8080, "TCP port to listen on")
	flagDir   = flag.String("dir", `C:\file-drop-server`, "root folder that receives the uploaded batches")
	flagHost  = flag.String("host", "", "address to encode in the QR code (auto-detected LAN IP when empty)")
	flagMaxMB = flag.Int64("max", 10240, "maximum size of a single upload batch in MB (0 for no limit)")
	flagOpen  = flag.Bool("open", true, "open each finished batch in Windows Explorer")
)

// batchMu serialises batch-folder creation so two phones uploading in the same
// second cannot land in the same directory.
var batchMu sync.Mutex

func main() {
	useUTF8Console()
	flag.Parse()
	log.SetFlags(log.Ltime)

	root, err := filepath.Abs(*flagDir)
	if err != nil {
		log.Fatalf("drop folder %q is not a usable path: %v", *flagDir, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		log.Fatalf("cannot create drop folder %s: %v", root, err)
	}

	host := *flagHost
	if host == "" {
		host, err = lanIP()
		if err != nil {
			log.Fatalf("could not work out this machine's LAN address (%v) - pass one with -host", err)
		}
	}
	publicURL := fmt.Sprintf("http://%s:%d/", host, *flagPort)

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
	qrPath := filepath.Join(root, "qr-code.png")
	if err := os.WriteFile(qrPath, qrPNG, 0o644); err != nil {
		log.Printf("warning: could not save %s: %v", qrPath, err)
	}

	hostTmpl := template.Must(template.New("host").Parse(hostHTMLSrc))

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

	mux.HandleFunc("/host", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		hostTmpl.Execute(w, map[string]string{"URL": publicURL, "Root": root})
	})

	mux.HandleFunc("/qr.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(qrPNG)
	})

	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		handleUpload(w, r, root)
	})

	mux.HandleFunc("/batches", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"batches": recentBatches(root, 15)})
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", *flagPort),
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 20 * time.Second,
		// No read/write timeouts on purpose: a phone on a weak signal can spend a
		// long while pushing a batch of videos, and cutting it off mid-upload
		// would leave half-written files behind.
	}

	fmt.Print("\n" + qr.ToSmallString(false) + "\n")
	fmt.Printf("  File Drop is running\n\n")
	fmt.Printf("  Scan the code above, or open   %s\n", publicURL)
	fmt.Printf("  Big QR code for a screen:      http://localhost:%d/host\n", *flagPort)
	fmt.Printf("  Printable QR image:            %s\n", qrPath)
	fmt.Printf("  Uploads land in:               %s\\<date>_<time>\\\n\n", root)
	fmt.Printf("  Press Ctrl+C to stop.\n\n")

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server stopped: %v", err)
	}
}

// handleUpload streams each part of the multipart body straight to disk, so a
// batch of large videos never has to fit in memory.
func handleUpload(w http.ResponseWriter, r *http.Request, root string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if *flagMaxMB > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, *flagMaxMB*1024*1024)
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
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": tooBigMessage()})
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
				log.Printf("rejected: batch over the %d MB limit", *flagMaxMB)
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": tooBigMessage()})
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
	if *flagOpen {
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

func tooBigMessage() string {
	return fmt.Sprintf("that batch is over the %d MB limit - try sending it in a few smaller goes", *flagMaxMB)
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
