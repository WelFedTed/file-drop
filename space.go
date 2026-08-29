package main

import "fmt"

// The free-space guard. Windows behaves badly on a full system disk, and the
// program most likely to fill one is the one taking whatever a phone sends. The
// check happens twice: the upload page asks before it starts, so a batch that
// cannot fit is refused before a byte crosses the network, and handleUpload
// asks again for itself, because the page is not the only thing that can post.

// uploadHeadroom is what a batch needs beyond the size of its files: the
// multipart envelope around them, the folder entries, the checksum manifest,
// and enough slack that a batch which just fits does not leave the volume at
// literally zero.
const uploadHeadroom = 8 << 20

// roomFor reports whether a batch of want bytes can be taken without leaving
// the drop volume below its reserve. The second result is the sentence to show
// whoever is trying to send it.
//
// It errs towards letting the upload run: a reserve of zero, an unknown size,
// or a volume that cannot be measured all come back true. The point is to catch
// the case that is certain to fail, not to police the disk.
func roomFor(root string, minFreeMB, want int64) (bool, string) {
	if want <= 0 {
		return true, ""
	}
	free, ok := freeSpace(root)
	if !ok {
		return true, ""
	}
	// No real volume comes near this, but the conversion below has to be safe
	// whatever the API says.
	if free > 1<<62 {
		return true, ""
	}

	available := int64(free)
	reserve := minFreeMB * 1024 * 1024
	need := want + uploadHeadroom

	if available-reserve >= need {
		return true, ""
	}
	// The messages quote the size of the batch itself rather than what it needs
	// with the headroom added, because the sender picked the one and has never
	// heard of the other.
	if available < need {
		return false, fmt.Sprintf(
			"There is not enough room on that computer: it has %s free and this batch is %s.",
			humanSize(available), humanSize(want))
	}
	// It would fit, but only by eating into what the machine was told to keep.
	return false, fmt.Sprintf(
		"That computer is set to keep %s of its disk free, and this batch of %s would take it below that - %s is free now. Send it in smaller batches, or ask for some room to be made.",
		humanSize(reserve), humanSize(want), humanSize(available))
}

// lowOnSpace reports whether the drop volume is already at or under its
// reserve, which /host says out loud: an upload refused for want of space is
// much less puzzling if the page has been saying so beforehand.
func lowOnSpace(root string, minFreeMB int64) bool {
	if minFreeMB <= 0 {
		return false
	}
	free, ok := freeSpace(root)
	if !ok {
		return false
	}
	return free <= uint64(minFreeMB)*1024*1024
}
