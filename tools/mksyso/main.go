// Command mksyso writes the Windows resources the built executable carries: the
// "File version" Explorer shows in its Details tab, and the icon shown by
// Explorer, the taskbar and Alt+Tab.
//
// Go has no way to express either, and the usual answer is a third-party
// generator. This program is that generator, kept in the tree so the project
// stays on the one dependency it already has, so the version comes from the
// same constant as everything else, and so the icon comes from the same drawing
// the tray icon does - see internal/icon.
//
// It emits a COFF object with a single .rsrc section, which the Go linker folds
// into the binary because of the _windows_amd64 suffix on the file name. Other
// platforms ignore it.
//
// Run from the repository root after changing the version:
//
//	go run ./tools/mksyso
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"filedrop/internal/icon"
)

func main() {
	log.SetFlags(0)

	var (
		flagVersion = flag.String("version", "", "version to stamp (defaults to the one in version.go)")
		flagOut     = flag.String("out", "rsrc_windows_amd64.syso", "object file to write")
		flagName    = flag.String("name", "file-drop.exe", "original file name to record")
	)
	flag.Parse()

	ver := *flagVersion
	if ver == "" {
		found, err := versionFromSource("version.go")
		if err != nil {
			log.Fatalf("%v (pass -version instead)", err)
		}
		ver = found
	}

	numbers, err := parseVersion(ver)
	if err != nil {
		log.Fatalf("%v", err)
	}

	res := []resource{{
		typ:  rtVersion,
		id:   1,
		data: versionResource(ver, numbers, *flagName),
	}}
	res = append(res, iconResources()...)

	if err := os.WriteFile(*flagOut, coffObject(res), 0o644); err != nil {
		log.Fatalf("could not write %s: %v", *flagOut, err)
	}
	fmt.Printf("wrote %s: version %s, icon at %d sizes\n", *flagOut, ver, len(iconSizes))
}

// The resource types this writes. RT_ICON holds one picture each; RT_GROUP_ICON
// is the little table that ties them together, and is what Explorer, the
// taskbar and Alt+Tab actually ask a program for.
const (
	rtIcon      = 3
	rtGroupIcon = 14
	rtVersion   = 16
)

// iconSizes are the sizes drawn into the executable. Windows picks the nearest
// one and scales from there, so the list covers the sizes the shell asks for at
// the usual display scalings rather than leaving it to resample a single
// picture into a smudge.
var iconSizes = []int{16, 20, 24, 32, 40, 48, 64, 128, 256}

// iconResources draws every size and builds the group directory over them.
func iconResources() []resource {
	var out []resource

	var group bytes.Buffer
	put16(&group, 0)                      // reserved
	put16(&group, 1)                      // type: icon rather than cursor
	put16(&group, uint16(len(iconSizes))) // how many follow

	for i, size := range iconSizes {
		data := icon.Image(size)
		if size >= 256 {
			// A quarter of a megabyte as a bitmap and a few kilobytes as a PNG,
			// which is how icons this size have been stored since Vista.
			data = iconPNG(size)
		}
		id := uint32(i + 1)
		out = append(out, resource{typ: rtIcon, id: id, data: data})

		// GRPICONDIRENTRY: 256 does not fit in a byte and is written as 0,
		// which every icon of that size does and every reader understands.
		group.WriteByte(byte(size))
		group.WriteByte(byte(size))
		group.WriteByte(0) // colours in the palette: none, this is 32-bit
		group.WriteByte(0) // reserved
		put16(&group, 1)   // planes
		put16(&group, 32)  // bits per pixel
		put32(&group, uint32(len(data)))
		put16(&group, uint16(id))
	}

	return append(out, resource{typ: rtGroupIcon, id: 1, data: group.Bytes()})
}

// iconPNG renders one size as a PNG, for the largest icon where the bitmap form
// would dwarf everything else in the resource section.
func iconPNG(size int) []byte {
	pixels := icon.Draw(size)
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			p := pixels[y*size+x]
			img.SetNRGBA(x, y, color.NRGBA{R: p[0], G: p[1], B: p[2], A: p[3]})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		log.Fatalf("could not encode the %d pixel icon: %v", size, err)
	}
	return b.Bytes()
}

func align(n, to uint32) uint32 {
	if rem := n % to; rem != 0 {
		n += to - rem
	}
	return n
}

// versionFromSource reads the version constant rather than taking it on the
// command line, so the resource cannot drift from the program by being
// regenerated with the wrong number typed in.
func versionFromSource(path string) (string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(src), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "const version") {
			continue
		}
		if i := strings.Index(line, `"`); i >= 0 {
			if j := strings.Index(line[i+1:], `"`); j >= 0 {
				return line[i+1 : i+1+j], nil
			}
		}
	}
	return "", fmt.Errorf("no version constant found in %s", path)
}

func parseVersion(s string) ([4]uint16, error) {
	var out [4]uint16
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return out, fmt.Errorf("%q is not a version number", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 0xFFFF {
			return out, fmt.Errorf("%q is not a version number", s)
		}
		out[i] = uint16(n)
	}
	return out, nil
}

// A node in the VS_VERSIONINFO tree. Every block has the same three-word header
// followed by a UTF-16 key, an optional value, and children - each part aligned
// to a 32-bit boundary.
type node struct {
	key      string
	value    []byte
	valueLen uint16 // bytes for binary values, UTF-16 characters for text
	text     bool
	children []*node
}

func (n *node) bytes() []byte {
	var b bytes.Buffer
	b.Write(make([]byte, 6)) // wLength, wValueLength and wType, filled in below
	b.Write(utf16z(n.key))
	pad4(&b)
	b.Write(n.value)
	for _, child := range n.children {
		pad4(&b)
		b.Write(child.bytes())
	}

	out := b.Bytes()
	binary.LittleEndian.PutUint16(out[0:], uint16(len(out)))
	binary.LittleEndian.PutUint16(out[2:], n.valueLen)
	if n.text {
		binary.LittleEndian.PutUint16(out[4:], 1)
	}
	return out
}

func text(key, value string) *node {
	encoded := utf16z(value)
	return &node{
		key: key, value: encoded,
		// Counted in UTF-16 characters, the terminating null included.
		valueLen: uint16(len(encoded) / 2),
		text:     true,
	}
}

func versionResource(ver string, n [4]uint16, originalName string) []byte {
	// VS_FIXEDFILEINFO: the numeric version Windows sorts and compares on,
	// as opposed to the string one it displays.
	var fixed bytes.Buffer
	put32(&fixed, 0xFEEF04BD) // signature
	put32(&fixed, 0x00010000) // struct version
	put32(&fixed, uint32(n[0])<<16|uint32(n[1]))
	put32(&fixed, uint32(n[2])<<16|uint32(n[3]))
	put32(&fixed, uint32(n[0])<<16|uint32(n[1]))
	put32(&fixed, uint32(n[2])<<16|uint32(n[3]))
	put32(&fixed, 0x3F)    // dwFileFlagsMask
	put32(&fixed, 0)       // dwFileFlags
	put32(&fixed, 0x40004) // VOS_NT_WINDOWS32
	put32(&fixed, 1)       // VFT_APP
	put32(&fixed, 0)       // subtype
	put32(&fixed, 0)       // date, high
	put32(&fixed, 0)       // date, low

	// 0409 is US English, 04B0 is codepage 1200 (UTF-16), which is what the
	// strings below are encoded in.
	strings := &node{key: "040904B0", text: true, children: []*node{
		text("CompanyName", "WelFedTed"),
		text("FileDescription", "File Drop - QR code file transfers over your network"),
		text("FileVersion", ver),
		text("InternalName", "file-drop"),
		text("LegalCopyright", "MIT licence"),
		text("OriginalFilename", originalName),
		text("ProductName", "File Drop"),
		text("ProductVersion", ver),
	}}

	translation := &node{key: "Translation", valueLen: 4}
	var tr bytes.Buffer
	put16(&tr, 0x0409)
	put16(&tr, 0x04B0)
	translation.value = tr.Bytes()

	root := &node{
		key:      "VS_VERSION_INFO",
		value:    fixed.Bytes(),
		valueLen: uint16(fixed.Len()),
		children: []*node{
			{key: "StringFileInfo", text: true, children: []*node{strings}},
			{key: "VarFileInfo", text: true, children: []*node{translation}},
		},
	}
	return root.bytes()
}

// resource is one thing to embed: what it is, which number it answers to, and
// the bytes themselves.
type resource struct {
	typ  uint32
	id   uint32
	data []byte
}

// coffObject wraps the resources in the object file the linker expects. The
// resource directory is three levels deep - type, then id, then language - and
// every data entry names its bytes by an address only the linker knows, so each
// one comes with a relocation that fixes it up.
func coffObject(resources []resource) []byte {
	const (
		dirSize    = 16 // IMAGE_RESOURCE_DIRECTORY
		entrySize  = 8  // IMAGE_RESOURCE_DIRECTORY_ENTRY
		dataSize   = 16 // IMAGE_RESOURCE_DATA_ENTRY
		subDirFlag = 0x80000000
	)

	// Windows binary-searches these directories, so entries out of order are
	// not merely untidy: they are entries it will fail to find.
	sort.SliceStable(resources, func(i, j int) bool {
		if resources[i].typ != resources[j].typ {
			return resources[i].typ < resources[j].typ
		}
		return resources[i].id < resources[j].id
	})

	var types []uint32
	perType := map[uint32]int{}
	for _, r := range resources {
		if perType[r.typ] == 0 {
			types = append(types, r.typ)
		}
		perType[r.typ]++
	}

	// Work out the whole layout first: every offset written below points
	// somewhere further down the section, so none of them can be known while
	// writing from the top.
	at := uint32(dirSize + entrySize*len(types))
	typeDirAt := map[uint32]uint32{}
	for _, t := range types {
		typeDirAt[t] = at
		at += uint32(dirSize + entrySize*perType[t])
	}
	langDirAt := make([]uint32, len(resources))
	for i := range resources {
		langDirAt[i] = at
		at += dirSize + entrySize
	}
	dataEntryAt := make([]uint32, len(resources))
	for i := range resources {
		dataEntryAt[i] = at
		at += dataSize
	}
	blobAt := make([]uint32, len(resources))
	for i, r := range resources {
		at = align(at, 8)
		blobAt[i] = at
		at += uint32(len(r.data))
	}
	sectionSize := int(align(at, 4)) // keep the relocations that follow aligned

	var sec bytes.Buffer
	pad := func(to uint32) {
		for uint32(sec.Len()) < to {
			sec.WriteByte(0)
		}
	}
	dirHeader := func(entries int) {
		put32(&sec, 0)               // characteristics
		put32(&sec, 0)               // timestamp
		put16(&sec, 0)               // major
		put16(&sec, 0)               // minor
		put16(&sec, 0)               // named entries: none, everything here has a number
		put16(&sec, uint16(entries)) // id entries
	}
	entry := func(id, offset uint32) {
		put32(&sec, id)
		put32(&sec, offset)
	}

	// The root: one entry per type of resource.
	dirHeader(len(types))
	for _, t := range types {
		entry(t, typeDirAt[t]|subDirFlag)
	}
	// A directory per type, listing the resources of that type.
	for _, t := range types {
		pad(typeDirAt[t])
		dirHeader(perType[t])
		for i, r := range resources {
			if r.typ == t {
				entry(r.id, langDirAt[i]|subDirFlag)
			}
		}
	}
	// A directory per resource, naming the one language it comes in.
	for i := range resources {
		pad(langDirAt[i])
		dirHeader(1)
		entry(0x0409, dataEntryAt[i])
	}
	// The data entries, whose first field the relocations turn into an RVA.
	for i, r := range resources {
		pad(dataEntryAt[i])
		put32(&sec, blobAt[i])
		put32(&sec, uint32(len(r.data)))
		put32(&sec, 0) // codepage
		put32(&sec, 0) // reserved
	}
	for i, r := range resources {
		pad(blobAt[i])
		sec.Write(r.data)
	}
	pad(uint32(sectionSize))

	const (
		headerSize    = 20
		sectionHdr    = 40
		relocSize     = 10
		symbolSize    = 18
		machineAMD64  = 0x8664
		relocAddr32NB = 0x0003
		scnData       = 0x40000040 // initialised data, readable
	)
	rawAt := uint32(headerSize + sectionHdr)
	relocAt := rawAt + uint32(sectionSize)
	symbolsAt := relocAt + relocSize*uint32(len(resources))

	var out bytes.Buffer
	// IMAGE_FILE_HEADER
	put16(&out, machineAMD64)
	put16(&out, 1) // one section
	put32(&out, 0) // timestamp
	put32(&out, symbolsAt)
	put32(&out, 2) // the section symbol and its auxiliary record
	put16(&out, 0) // no optional header
	put16(&out, 0) // characteristics

	// IMAGE_SECTION_HEADER
	out.WriteString(".rsrc\x00\x00\x00")
	put32(&out, 0) // virtual size
	put32(&out, 0) // virtual address
	put32(&out, uint32(sectionSize))
	put32(&out, rawAt)
	put32(&out, relocAt)
	put32(&out, 0)                      // line numbers
	put16(&out, uint16(len(resources))) // one relocation per data entry
	put16(&out, 0)
	put32(&out, scnData)

	out.Write(sec.Bytes())

	// Each data entry begins with an RVA, which needs fixing up against the
	// section's own address once the linker has placed it.
	for i := range resources {
		put32(&out, dataEntryAt[i])
		put32(&out, 0) // symbol index: the section symbol below
		put16(&out, relocAddr32NB)
	}

	// Symbol table: the section, plus the auxiliary record describing it.
	out.WriteString(".rsrc\x00\x00\x00")
	put32(&out, 0)   // value
	put16(&out, 1)   // section number
	put16(&out, 0)   // type
	out.WriteByte(3) // IMAGE_SYM_CLASS_STATIC
	out.WriteByte(1) // one auxiliary record

	put32(&out, uint32(sectionSize))
	put16(&out, uint16(len(resources))) // relocations
	put16(&out, 0)                      // line numbers
	put32(&out, 0)                      // checksum
	put16(&out, 0)                      // number
	out.WriteByte(0)
	out.Write(make([]byte, 3))

	put32(&out, 4) // string table: its own length and nothing else

	return out.Bytes()
}

func utf16z(s string) []byte {
	var b bytes.Buffer
	for _, r := range utf16.Encode([]rune(s)) {
		put16(&b, r)
	}
	put16(&b, 0)
	return b.Bytes()
}

func pad4(b *bytes.Buffer) {
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
}

func put16(b *bytes.Buffer, v uint16) { binary.Write(b, binary.LittleEndian, v) }
func put32(b *bytes.Buffer, v uint32) { binary.Write(b, binary.LittleEndian, v) }
