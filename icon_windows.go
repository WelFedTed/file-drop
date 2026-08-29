//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"math"
)

// The tray icon, drawn here rather than shipped as a file.
//
// Windows will build an icon out of raw bytes handed to CreateIconFromResource,
// which means the picture can be a few dozen lines of arithmetic instead of a
// binary blob in a repository that is otherwise all text - and it can be drawn
// at whatever size the shell asks for, rather than scaled from one that is
// nearly right. What it draws is a rounded blue tile with a white arrow coming
// down onto a line: something arriving on a machine.

// iconImage returns one icon image in the form Windows reads: a
// BITMAPINFOHEADER whose height covers both masks, the colour pixels bottom-up,
// then the AND mask, which is all zeros because the alpha channel does that job
// on anything since XP.
func iconImage(size int) []byte {
	if size < 8 {
		size = 8
	}
	pixels := drawIcon(size)

	var b bytes.Buffer
	put32 := func(v uint32) { binary.Write(&b, binary.LittleEndian, v) }
	put16 := func(v uint16) { binary.Write(&b, binary.LittleEndian, v) }

	put32(40)                      // biSize
	put32(uint32(size))            // biWidth
	put32(uint32(size * 2))        // biHeight: colour rows and mask rows
	put16(1)                       // biPlanes
	put16(32)                      // biBitCount
	put32(0)                       // biCompression: BI_RGB
	put32(uint32(size * size * 4)) // biSizeImage
	put32(0)                       // biXPelsPerMeter
	put32(0)                       // biYPelsPerMeter
	put32(0)                       // biClrUsed
	put32(0)                       // biClrImportant

	// Bottom-up, as every DIB is, and BGRA rather than RGBA.
	for y := size - 1; y >= 0; y-- {
		for x := 0; x < size; x++ {
			p := pixels[y*size+x]
			b.WriteByte(p[2])
			b.WriteByte(p[1])
			b.WriteByte(p[0])
			b.WriteByte(p[3])
		}
	}

	// The AND mask: one bit per pixel, rows padded to four bytes.
	stride := ((size + 31) / 32) * 4
	b.Write(make([]byte, stride*size))

	return b.Bytes()
}

// The tile colour, which is the accent blue the pages use.
var (
	iconBlue = [3]float64{0x25, 0x63, 0xeb}
	iconInk  = [3]float64{0xff, 0xff, 0xff}
)

// drawIcon renders the glyph with four-by-four supersampling, which is what
// keeps the rounded corners and the arrow's diagonals from looking like steps
// at the 16-pixel size the notification area actually uses.
func drawIcon(size int) [][4]byte {
	const samples = 4

	out := make([][4]byte, size*size)
	s := float64(size)

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var tile, ink float64
			for sy := 0; sy < samples; sy++ {
				for sx := 0; sx < samples; sx++ {
					px := (float64(x) + (float64(sx)+0.5)/samples) / s
					py := (float64(y) + (float64(sy)+0.5)/samples) / s
					if inTile(px, py) {
						tile++
						if inGlyph(px, py) {
							ink++
						}
					}
				}
			}
			total := float64(samples * samples)
			if tile == 0 {
				continue
			}
			alpha := tile / total
			mix := ink / tile // how much of this pixel's tile is glyph

			var c [4]byte
			for i := 0; i < 3; i++ {
				v := iconBlue[i]*(1-mix) + iconInk[i]*mix
				c[i] = byte(math.Round(v))
			}
			c[3] = byte(math.Round(alpha * 255))
			out[y*size+x] = c
		}
	}
	return out
}

// inTile is the rounded square, in coordinates that run 0 to 1 across the icon.
// A little margin keeps the corners off the very edge, where the shell likes to
// crop.
func inTile(x, y float64) bool {
	const margin = 0.03
	const radius = 0.22

	lo, hi := margin, 1-margin
	if x < lo || x > hi || y < lo || y > hi {
		return false
	}
	// Inside the straight edges, or inside the arc of the nearest corner.
	cx := math.Min(math.Max(x, lo+radius), hi-radius)
	cy := math.Min(math.Max(y, lo+radius), hi-radius)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= radius*radius
}

// inGlyph is the arrow: a stem, a head, and the line it lands on.
func inGlyph(x, y float64) bool {
	// Stem.
	if x >= 0.435 && x <= 0.565 && y >= 0.20 && y <= 0.52 {
		return true
	}
	// Head: a triangle from the stem's shoulders down to a point.
	if y >= 0.44 && y <= 0.68 {
		t := (y - 0.44) / (0.68 - 0.44) // 0 at the shoulders, 1 at the point
		half := 0.20 * (1 - t)
		if math.Abs(x-0.5) <= half {
			return true
		}
	}
	// The line it arrives on.
	if x >= 0.26 && x <= 0.74 && y >= 0.76 && y <= 0.85 {
		return true
	}
	return false
}
