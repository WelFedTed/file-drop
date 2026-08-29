package main

import (
	"sync"
	"sync/atomic"
)

// What is arriving right now, as opposed to what has already landed.
//
// Uploads are streamed straight to disk, so a batch folder exists and fills up
// for as long as the phone keeps sending - which for a few videos over a weak
// signal is minutes. Without this the operator's screen would list that folder
// as a finished drop, with a file count that quietly grows, and there would be
// no moment at which anything announced that a drop had actually arrived.
//
// Registering each upload while it runs gives /host both halves: a row that
// says "receiving", and a completion it can chime for.

// liveUpload is one upload in progress. It doubles as an io.Writer so the byte
// count comes from the same copy that writes the file, rather than from a
// second pass over the data.
type liveUpload struct {
	id   int64
	from string

	// Written on every chunk and read by whoever asks /batches, so it carries
	// its own guard rather than sharing the one below.
	bytes atomic.Int64

	mu     sync.Mutex
	folder string // empty until the first file arrives and the batch is made
	file   string // the one being written at the moment
	files  int
}

// activeUpload is the same thing seen from the /host page.
type activeUpload struct {
	ID     int64  `json:"id"`
	From   string `json:"from"`
	Folder string `json:"folder"`
	File   string `json:"file"`
	Files  int    `json:"files"`
	Bytes  int64  `json:"bytes"`
}

var inflight = struct {
	mu   sync.Mutex
	next int64
	m    map[int64]*liveUpload
}{m: map[int64]*liveUpload{}}

func beginUpload(from string) *liveUpload {
	inflight.mu.Lock()
	defer inflight.mu.Unlock()
	inflight.next++
	u := &liveUpload{id: inflight.next, from: from}
	inflight.m[u.id] = u
	return u
}

// done takes the upload off the list, whether it finished, failed or was cut
// off part way. Every path out of handleUpload has to reach this, so it is
// deferred at the top rather than called at the end.
func (u *liveUpload) done() {
	inflight.mu.Lock()
	delete(inflight.m, u.id)
	inflight.mu.Unlock()
}

func (u *liveUpload) Write(p []byte) (int, error) {
	u.bytes.Add(int64(len(p)))
	return len(p), nil
}

func (u *liveUpload) setFolder(name string) {
	u.mu.Lock()
	u.folder = name
	u.mu.Unlock()
}

// startFile is called as each part begins, so the page can name what is on the
// wire rather than only counting bytes.
func (u *liveUpload) startFile(name string) {
	u.mu.Lock()
	u.file = name
	u.mu.Unlock()
}

func (u *liveUpload) finishFile() {
	u.mu.Lock()
	u.files++
	u.mu.Unlock()
}

// activeUploads lists what is arriving, oldest first. An upload that has not
// yet produced a file is included: the "receiving" row should appear when the
// phone starts sending, not a minute later when the first file completes.
func activeUploads() []activeUpload {
	inflight.mu.Lock()
	all := make([]*liveUpload, 0, len(inflight.m))
	for _, u := range inflight.m {
		all = append(all, u)
	}
	inflight.mu.Unlock()

	out := make([]activeUpload, 0, len(all))
	for _, u := range all {
		u.mu.Lock()
		out = append(out, activeUpload{
			ID:     u.id,
			From:   u.from,
			Folder: u.folder,
			File:   u.file,
			Files:  u.files,
			Bytes:  u.bytes.Load(),
		})
		u.mu.Unlock()
	}
	// The ids only ever count up, so this is arrival order.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].ID > out[j].ID; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
