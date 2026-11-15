// Package iso9660 writes a small ISO 9660 image with Joliet names and, when
// asked, an El Torito boot catalog for UEFI. It writes from files given as
// readers, in one pass, to any io.Writer: no host tool, no mount.
//
// The primary tree has ISO 9660 level 2 names (upper case, d-characters, a
// ";1" version); the Joliet tree keeps each name as given, which is what
// Windows reads.
package iso9660

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
)

const sector = 2048

// File is one file in the image.
type File struct {
	// Path is slash-separated from the root; its directories are made.
	Path string
	Size int64
	Open func() (io.ReadCloser, error)
}

// Image is what Write writes.
type Image struct {
	// VolumeID is the volume's label, up to 16 characters.
	VolumeID string
	Files    []File
	// EFIBoot, when set, is the Path of one of Files: a FAT image the UEFI
	// firmware boots from, as El Torito's no-emulation entry for EFI.
	EFIBoot string
	// Time stamps every record; the zero time is the Unix epoch.
	Time time.Time
}

// maxSize bounds a file: one extent of a directory record.
const maxSize = 1<<32 - sector

type node struct {
	name     string // as given
	dir      bool
	children []*node
	file     *File
	parent   *node
	// Layout.
	number          int    // in the path table, from 1
	primary, joliet uint32 // a directory's record extents
	primarySize     uint32
	jolietSize      uint32
	data            uint32 // a file's extent
}

// Write writes im to w.
func Write(w io.Writer, im Image) error {
	if len(im.VolumeID) > 16 {
		return fmt.Errorf("iso9660: volume ID %q is over 16 characters", im.VolumeID)
	}
	root := &node{dir: true}
	var boot *node
	for i := range im.Files {
		f := &im.Files[i]
		if f.Size < 0 || f.Size > maxSize {
			return fmt.Errorf("iso9660: %s is %d bytes", f.Path, f.Size)
		}
		n, err := root.add(f)
		if err != nil {
			return err
		}
		if im.EFIBoot != "" && strings.EqualFold(strings.Trim(f.Path, "/"), strings.Trim(im.EFIBoot, "/")) {
			boot = n
		}
	}
	if im.EFIBoot != "" && boot == nil {
		return fmt.Errorf("iso9660: the boot image %s is not among the files", im.EFIBoot)
	}
	stamp := im.Time
	if stamp.IsZero() {
		stamp = time.Unix(0, 0)
	}
	stamp = stamp.UTC()

	// Directories in path-table order: by depth, then parent, then name.
	dirs := []*node{root}
	for i := 0; i < len(dirs); i++ {
		d := dirs[i]
		d.number = i + 1
		slices.SortFunc(d.children, func(a, b *node) int { return strings.Compare(primaryName(a), primaryName(b)) })
		for _, c := range d.children {
			if c.dir {
				dirs = append(dirs, c)
			}
		}
	}

	// Layout: 16 system sectors, the descriptors, the boot catalog, the
	// path tables, the directories, then the files' data.
	next := uint32(16)
	next++ // the primary volume descriptor
	if boot != nil {
		next++ // the boot record
	}
	next += 2 // the Joliet descriptor, the terminator
	catalogAt := uint32(0)
	if boot != nil {
		catalogAt = next
		next++
	}
	lPrimary, mPrimary, lJoliet, mJoliet := pathTable(dirs, false, true), pathTable(dirs, false, false), pathTable(dirs, true, true), pathTable(dirs, true, false)
	tables := [4]uint32{}
	for i, t := range [][]byte{lPrimary, mPrimary, lJoliet, mJoliet} {
		tables[i] = next
		next += sectors(int64(len(t)))
	}
	for _, d := range dirs {
		d.primarySize = dirSize(d, false)
		d.primary = next
		next += d.primarySize / sector
	}
	for _, d := range dirs {
		d.jolietSize = dirSize(d, true)
		d.joliet = next
		next += d.jolietSize / sector
	}
	var files []*node
	walk(root, func(n *node) {
		if !n.dir {
			files = append(files, n)
		}
	})
	for _, f := range files {
		f.data = next
		next += sectors(f.file.Size)
	}
	total := next
	// Path tables were laid out before the directories had extents.
	lPrimary, mPrimary, lJoliet, mJoliet = pathTable(dirs, false, true), pathTable(dirs, false, false), pathTable(dirs, true, true), pathTable(dirs, true, false)

	out := &sectorWriter{w: w}
	if err := out.pad(16 * sector); err != nil {
		return err
	}
	pvd := descriptor(1, im.VolumeID, false, total, uint32(len(lPrimary)), tables[0], tables[1], record(root, ".", false, stamp), stamp)
	if err := out.write(pvd); err != nil {
		return err
	}
	if boot != nil {
		br := make([]byte, sector)
		br[0] = 0
		copy(br[1:], "CD001")
		br[6] = 1
		copy(br[7:], "EL TORITO SPECIFICATION")
		binary.LittleEndian.PutUint32(br[71:], catalogAt)
		if err := out.write(br); err != nil {
			return err
		}
	}
	svd := descriptor(2, im.VolumeID, true, total, uint32(len(lJoliet)), tables[2], tables[3], record(root, ".", true, stamp), stamp)
	if err := out.write(svd); err != nil {
		return err
	}
	term := make([]byte, sector)
	term[0] = 255
	copy(term[1:], "CD001")
	term[6] = 1
	if err := out.write(term); err != nil {
		return err
	}
	if boot != nil {
		if err := out.write(catalog(boot)); err != nil {
			return err
		}
	}
	for _, t := range [][]byte{lPrimary, mPrimary, lJoliet, mJoliet} {
		if err := out.write(t); err != nil {
			return err
		}
	}
	for _, joliet := range []bool{false, true} {
		for _, d := range dirs {
			if err := out.write(dirRecords(d, joliet, stamp)); err != nil {
				return err
			}
		}
	}
	for _, f := range files {
		if err := copyFile(out, f.file); err != nil {
			return err
		}
	}
	if out.n != int64(total)*sector {
		return fmt.Errorf("iso9660: wrote %d bytes of a %d-sector image", out.n, total)
	}
	return nil
}

func (d *node) add(f *File) (*node, error) {
	parts := strings.Split(strings.Trim(path.Clean("/"+f.Path), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return nil, fmt.Errorf("iso9660: a file with no name (%q)", f.Path)
	}
	if len(parts) > 8 {
		return nil, fmt.Errorf("iso9660: %s is deeper than 8 levels", f.Path)
	}
	cur := d
	for i, p := range parts {
		if len(utf16.Encode([]rune(p))) > 64 {
			return nil, fmt.Errorf("iso9660: %q is over 64 characters", p)
		}
		var found *node
		for _, c := range cur.children {
			if strings.EqualFold(c.name, p) || primaryName(c) == primaryName(&node{name: p, dir: c.dir}) {
				found = c
			}
		}
		last := i == len(parts)-1
		if found != nil {
			if last || !found.dir {
				return nil, fmt.Errorf("iso9660: %s names a file twice, or a file and a directory", f.Path)
			}
			cur = found
			continue
		}
		n := &node{name: p, dir: !last, parent: cur}
		if last {
			n.file = f
		}
		cur.children = append(cur.children, n)
		cur = n
	}
	return cur, nil
}

func walk(n *node, fn func(*node)) {
	fn(n)
	for _, c := range n.children {
		walk(c, fn)
	}
}

// primaryName is n's ISO 9660 level 2 identifier: d-characters, a file's
// with a version.
func primaryName(n *node) string {
	name := strings.ToUpper(n.name)
	clean := func(s string, limit int) string {
		b := []byte{}
		for _, r := range s {
			if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
				b = append(b, byte(r))
			} else {
				b = append(b, '_')
			}
		}
		if len(b) > limit {
			b = b[:limit]
		}
		return string(b)
	}
	if n.dir {
		return clean(name, 31)
	}
	base, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		base, ext = name[:i], name[i+1:]
	}
	ext = clean(ext, 8)
	base = clean(base, 30-len(ext)-1)
	return base + "." + ext + ";1"
}

func identifier(n *node, joliet bool) []byte {
	if !joliet {
		return []byte(primaryName(n))
	}
	return ucs2(n.name)
}

func ucs2(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.BigEndian.PutUint16(b[2*i:], c)
	}
	return b
}

func sectors(size int64) uint32 { return uint32((size + sector - 1) / sector) }

// record is one directory record for n, under the identifier given for "."
// and "..", or n's own.
func record(n *node, special string, joliet bool, stamp time.Time) []byte {
	var id []byte
	switch special {
	case ".":
		id = []byte{0}
	case "..":
		id = []byte{1}
	default:
		id = identifier(n, joliet)
	}
	size := 33 + len(id)
	if size%2 != 0 {
		size++
	}
	r := make([]byte, size)
	r[0] = byte(size)
	var extent, length uint32
	flags := byte(0)
	if n.dir {
		extent, length, flags = n.primary, n.primarySize, 2
		if joliet {
			extent, length = n.joliet, n.jolietSize
		}
	} else {
		extent, length = n.data, uint32(n.file.Size)
	}
	both32(r[2:], extent)
	both32(r[10:], length)
	recordTime(r[18:], stamp)
	r[25] = flags
	both16(r[28:], 1)
	r[32] = byte(len(id))
	copy(r[33:], id)
	return r
}

func dirRecords(d *node, joliet bool, stamp time.Time) []byte {
	parent := d.parent
	if parent == nil {
		parent = d
	}
	var buf []byte
	used := 0
	add := func(r []byte) {
		if used+len(r) > sector {
			buf = append(buf, make([]byte, sector-used)...)
			used = 0
		}
		buf = append(buf, r...)
		used += len(r)
	}
	add(record(d, ".", joliet, stamp))
	add(record(parent, "..", joliet, stamp))
	children := d.children
	if joliet {
		children = slices.Clone(children)
		slices.SortFunc(children, func(a, b *node) int { return bytes.Compare(ucs2(a.name), ucs2(b.name)) })
	}
	for _, c := range children {
		add(record(c, "", joliet, stamp))
	}
	return buf
}

// dirSize is the sectors d's records fill, in bytes; it lays them out as
// dirRecords does, with the extents' values irrelevant to their sizes.
func dirSize(d *node, joliet bool) uint32 {
	n := len(dirRecords(d, joliet, time.Time{}))
	return sectors(int64(n)) * sector
}

func pathTable(dirs []*node, joliet, little bool) []byte {
	var buf []byte
	for _, d := range dirs {
		id := []byte{0}
		if d.parent != nil {
			id = identifier(d, joliet)
		}
		e := make([]byte, 8+len(id)+len(id)%2)
		e[0] = byte(len(id))
		extent := d.primary
		if joliet {
			extent = d.joliet
		}
		parent := 1
		if d.parent != nil {
			parent = d.parent.number
		}
		if little {
			binary.LittleEndian.PutUint32(e[2:], extent)
			binary.LittleEndian.PutUint16(e[6:], uint16(parent))
		} else {
			binary.BigEndian.PutUint32(e[2:], extent)
			binary.BigEndian.PutUint16(e[6:], uint16(parent))
		}
		copy(e[8:], id)
		buf = append(buf, e...)
	}
	return buf
}

func descriptor(typ byte, volume string, joliet bool, total, tableSize, lTable, mTable uint32, root []byte, stamp time.Time) []byte {
	d := make([]byte, sector)
	d[0] = typ
	copy(d[1:], "CD001")
	d[6] = 1
	text := func(at, n int, s string) {
		if joliet {
			b := ucs2(s)
			if len(b) > n {
				b = b[:n]
			}
			copy(d[at:], b)
			for i := at + len(b); i+1 < at+n; i += 2 {
				d[i], d[i+1] = 0, ' '
			}
			return
		}
		copy(d[at:at+n], bytes.Repeat([]byte{' '}, n))
		copy(d[at:at+n], strings.ToUpper(s))
	}
	text(8, 32, "")
	text(40, 32, volume)
	both32(d[80:], total)
	if joliet {
		copy(d[88:], "%/E") // UCS-2 level 3
	}
	both16(d[120:], 1)
	both16(d[124:], 1)
	both16(d[128:], sector)
	both32(d[132:], tableSize)
	binary.LittleEndian.PutUint32(d[140:], lTable)
	binary.BigEndian.PutUint32(d[148:], mTable)
	copy(d[156:], root)
	for _, f := range [][2]int{{190, 128}, {318, 128}, {446, 128}, {574, 128}, {702, 37}, {739, 37}, {776, 37}} {
		text(f[0], f[1], "")
	}
	if !joliet {
		text(574, 128, "DISCO-VM")
	}
	descriptorTime(d[813:], stamp)
	descriptorTime(d[830:], stamp)
	copy(d[847:], "0000000000000000")
	copy(d[864:], "0000000000000000")
	d[881] = 1
	return d
}

// catalog is the El Torito boot catalog: a validation entry for x86 and a
// default entry, then a final section for EFI, both booting the same
// no-emulation image, which a UEFI firmware loads whole.
func catalog(boot *node) []byte {
	c := make([]byte, sector)
	c[0] = 1 // validation entry, platform 0 (x86)
	copy(c[4:], "DISCO-VM")
	c[30], c[31] = 0x55, 0xAA
	var sum uint16
	for i := 0; i < 32; i += 2 {
		sum += binary.LittleEndian.Uint16(c[i:])
	}
	binary.LittleEndian.PutUint16(c[28:], -sum)
	count := uint16(min((boot.file.Size+511)/512, 0xFFFF))
	entry := func(e []byte) {
		e[0] = 0x88 // bootable, no emulation
		binary.LittleEndian.PutUint16(e[6:], count)
		binary.LittleEndian.PutUint32(e[8:], boot.data)
	}
	entry(c[32:64])
	c[64] = 0x91 // final section header
	c[65] = 0xEF // EFI
	binary.LittleEndian.PutUint16(c[66:], 1)
	entry(c[96:128])
	return c
}

func copyFile(out *sectorWriter, f *File) error {
	r, err := f.Open()
	if err != nil {
		return fmt.Errorf("iso9660: %s: %w", f.Path, err)
	}
	defer r.Close()
	n, err := io.Copy(out, io.LimitReader(r, f.Size))
	if err != nil {
		return fmt.Errorf("iso9660: %s: %w", f.Path, err)
	}
	if n != f.Size {
		return fmt.Errorf("iso9660: %s gave %d of its %d bytes", f.Path, n, f.Size)
	}
	return out.pad(int64(sectors(f.Size))*sector - n)
}

type sectorWriter struct {
	w io.Writer
	n int64
}

func (s *sectorWriter) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	s.n += int64(n)
	return n, err
}

// write writes b, padded to whole sectors.
func (s *sectorWriter) write(b []byte) error {
	if _, err := s.Write(b); err != nil {
		return err
	}
	if rem := len(b) % sector; rem != 0 {
		return s.pad(int64(sector - rem))
	}
	return nil
}

func (s *sectorWriter) pad(n int64) error {
	if n < 0 {
		return errors.New("iso9660: negative padding")
	}
	_, err := io.CopyN(s, zeros{}, n)
	return err
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func both16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
}

func both32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
}

func recordTime(b []byte, t time.Time) {
	b[0] = byte(t.Year() - 1900)
	b[1], b[2] = byte(t.Month()), byte(t.Day())
	b[3], b[4], b[5] = byte(t.Hour()), byte(t.Minute()), byte(t.Second())
	b[6] = 0 // UTC
}

func descriptorTime(b []byte, t time.Time) {
	copy(b, fmt.Sprintf("%04d%02d%02d%02d%02d%02d00", t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second()))
	b[16] = 0
}
