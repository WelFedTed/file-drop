// Command mksyso writes the Windows version resource that gives the built
// executable its "File version" in Explorer's Details tab.
//
// Go has no way to express a VERSIONINFO resource, and the usual answer is a
// third-party generator. This program is that generator, kept in the tree so
// the project stays on the one dependency it already has, and so the resource
// is built from the same version constant as everything else.
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
	"log"
	"os"
	"strconv"
	"strings"
	"unicode/utf16"
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

	res := versionResource(ver, numbers, *flagName)
	if err := os.WriteFile(*flagOut, coffObject(res), 0o644); err != nil {
		log.Fatalf("could not write %s: %v", *flagOut, err)
	}
	fmt.Printf("wrote %s for version %s\n", *flagOut, ver)
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

// coffObject wraps the resource in the object file the linker expects: a
// three-level resource directory (type, then id, then language) pointing at one
// data entry, whose address has to be relocated because it is an RVA that only
// the linker knows.
func coffObject(res []byte) []byte {
	const (
		rtVersion  = 16
		dirSize    = 16 // IMAGE_RESOURCE_DIRECTORY
		entrySize  = 8  // IMAGE_RESOURCE_DIRECTORY_ENTRY
		dataSize   = 16 // IMAGE_RESOURCE_DATA_ENTRY
		subDirFlag = 0x80000000
	)

	typeDirAt := uint32(dirSize + entrySize)       // 24
	langDirAt := typeDirAt + dirSize + entrySize   // 48
	dataEntryAt := langDirAt + dirSize + entrySize // 72
	resourceAt := dataEntryAt + dataSize           // 88
	sectionSize := int(resourceAt) + len(res)      //
	for sectionSize%4 != 0 {                       // keep the relocations aligned
		sectionSize++
	}

	var sec bytes.Buffer
	writeDir := func(id, offset uint32) {
		put32(&sec, 0) // characteristics
		put32(&sec, 0) // timestamp
		put16(&sec, 0) // major
		put16(&sec, 0) // minor
		put16(&sec, 0) // named entries
		put16(&sec, 1) // id entries
		put32(&sec, id)
		put32(&sec, offset)
	}
	writeDir(rtVersion, typeDirAt|subDirFlag)
	writeDir(1, langDirAt|subDirFlag) // resource id 1
	writeDir(0x0409, dataEntryAt)     // language, pointing at the data entry

	put32(&sec, resourceAt) // patched by the relocation into an RVA
	put32(&sec, uint32(len(res)))
	put32(&sec, 0) // codepage
	put32(&sec, 0) // reserved
	sec.Write(res)
	for sec.Len() < sectionSize {
		sec.WriteByte(0)
	}

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
	symbolsAt := relocAt + relocSize

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
	put32(&out, 0) // line numbers
	put16(&out, 1) // one relocation
	put16(&out, 0)
	put32(&out, scnData)

	out.Write(sec.Bytes())

	// The data entry's first field is an RVA, so it needs fixing up against the
	// section's own address once the linker has placed it.
	put32(&out, dataEntryAt)
	put32(&out, 0) // symbol index: the section symbol below
	put16(&out, relocAddr32NB)

	// Symbol table: the section, plus the auxiliary record describing it.
	out.WriteString(".rsrc\x00\x00\x00")
	put32(&out, 0)   // value
	put16(&out, 1)   // section number
	put16(&out, 0)   // type
	out.WriteByte(3) // IMAGE_SYM_CLASS_STATIC
	out.WriteByte(1) // one auxiliary record

	put32(&out, uint32(sectionSize))
	put16(&out, 1) // relocations
	put16(&out, 0) // line numbers
	put32(&out, 0) // checksum
	put16(&out, 0) // number
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
