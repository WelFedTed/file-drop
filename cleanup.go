package main

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Removing drop folders, by hand from the page and on a timer.
//
// Both go through batchFolderName first. Only a folder this program could have
// created is ever removed: the drop root is somewhere the operator chose and may
// well hold other things, and a delete that walks outside the naming scheme is a
// delete that can take something nobody offered up. That rules out qr-code.png,
// anything dropped in by hand, and any path that tries to climb out.

// batchPattern is exactly what newBatchDir writes: a timestamp, with the -2, -3
// suffix that separates two batches arriving in the same second.
var batchPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}(-\d+)?$`)

const batchTimeLayout = "2006-01-02_15-04-05"

// batchFolderName checks that name is one of ours and returns the full path to
// it inside root.
func batchFolderName(root, name string) (string, error) {
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return "", errors.New("that is not a batch folder name")
	}
	if !batchPattern.MatchString(name) {
		return "", errors.New("that is not a batch folder name")
	}
	dir := filepath.Join(root, name)
	if !within(root, dir) {
		return "", errors.New("that is not a batch folder name")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", errors.New("there is no such batch")
	}
	return dir, nil
}

// batchTime reads the timestamp a batch folder is named after. The name is used
// rather than the folder's modification time, because the name is when the
// batch arrived and the mtime is whenever it was last touched.
func batchTime(name string) (time.Time, bool) {
	if !batchPattern.MatchString(name) {
		return time.Time{}, false
	}
	stamp := name
	// Trim the same-second suffix before parsing; it is not part of the time.
	if i := strings.LastIndex(stamp, "-"); i > len(batchTimeLayout)-1 {
		stamp = stamp[:i]
	}
	t, err := time.ParseInLocation(batchTimeLayout, stamp, time.Local)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// deleteBatch removes one drop folder and everything in it.
func deleteBatch(root, name string) error {
	dir, err := batchFolderName(root, name)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return errors.New("could not delete " + name + ": " + err.Error())
	}
	log.Printf("deleted %s", name)
	return nil
}

// sweepOldBatches removes the drop folders older than the configured age. It
// reports how many went, and stops at nothing: a folder it cannot read is left
// alone rather than treated as expired.
func sweepOldBatches(root string, days int) int {
	if days < 1 {
		return 0
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		log.Printf("could not read %s to clear old drops: %v", root, err)
		return 0
	}

	cutoff := time.Now().AddDate(0, 0, -days)
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		when, ok := batchTime(e.Name())
		if !ok || !when.Before(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			log.Printf("could not delete %s: %v", e.Name(), err)
			continue
		}
		log.Printf("deleted %s, older than %d days", e.Name(), days)
		removed++
	}
	return removed
}

// startBatchSweeper runs the sweep at start-up and then hourly, reading the
// settings each time so switching it on in the panel takes effect without a
// restart - and switching it off stops it just as quickly.
func startBatchSweeper() {
	go func() {
		for {
			cfg := currentSettings()
			if cfg.AutoDelete {
				if n := sweepOldBatches(cfg.Dir, cfg.AutoDeleteDays); n > 0 {
					log.Printf("cleared %d drop folder(s) older than %d days", n, cfg.AutoDeleteDays)
				}
			}
			time.Sleep(time.Hour)
		}
	}()
}
