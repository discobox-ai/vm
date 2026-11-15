// Package udf reads files from a UDF image (ECMA-167, OSTA UDF 1.02 to 2.60),
// the file system of Microsoft's retail Windows ISOs, whose ISO 9660 tree
// holds only a README. It reads; it never writes, and it needs nothing but an
// io.ReaderAt over the image: no mount, no host tool.
//
// It reads what a mastering tool such as oscdimg writes: one type 1
// partition, file entries and extended file entries, short, long, and inline
// allocation descriptors, and continued allocation extents.
package udf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"unicode/utf16"
)

const sectorSize = 2048

// Descriptor tag identifiers (ECMA-167 3/7.2.1, 4/7.2.1).
const (
	tagAnchor          = 2
	tagPartition       = 5
	tagLogicalVolume   = 6
	tagTerminating     = 8
	tagFileSet         = 256
	tagFileIdentifier  = 257
	tagAllocExtent     = 258
	tagFileEntry       = 261
	tagExtendedFileEnt = 266
)

// maxDescriptors bounds a walk of the volume descriptor sequence, and
// maxExtents a file's allocation, against a corrupt or hostile image.
const (
	maxDescriptors = 256
	maxExtents     = 1 << 16
	maxDirBytes    = 64 << 20
)

// FS is an open UDF image.
type FS struct {
	r         io.ReaderAt
	partStart int64 // the partition's first sector
	block     int64 // logical block size
	root      longAD
}

type longAD struct {
	length    uint32 // with the extent type in its top two bits
	block     uint32
	partition uint16
}

// Open reads the image's volume and file set descriptors.
func Open(r io.ReaderAt) (*FS, error) {
	anchor, err := readTag(r, 256*sectorSize, tagAnchor)
	if err != nil {
		return nil, fmt.Errorf("udf: anchor volume descriptor at sector 256: %w", err)
	}
	vdsLength := le32(anchor[16:])
	vdsSector := le32(anchor[20:])
	f := &FS{r: r}
	var partNumber uint16
	var mapPartition uint16
	var fsd longAD
	havePart, haveLV := false, false
	for i := range min(int(vdsLength/sectorSize), maxDescriptors) {
		buf := make([]byte, sectorSize)
		if _, err := r.ReadAt(buf, int64(vdsSector+uint32(i))*sectorSize); err != nil {
			return nil, fmt.Errorf("udf: volume descriptor sequence: %w", err)
		}
		id := le16(buf)
		switch id {
		case tagPartition:
			partNumber = le16(buf[22:])
			f.partStart = int64(le32(buf[188:]))
			havePart = true
		case tagLogicalVolume:
			f.block = int64(le32(buf[212:]))
			fsd = parseLongAD(buf[248:])
			mapCount := le32(buf[268:])
			if mapCount != 1 || buf[440] != 1 {
				return nil, errors.New("udf: only a volume with one type 1 partition map is read")
			}
			mapPartition = le16(buf[440+4:])
			haveLV = true
		}
		if id == tagTerminating {
			break
		}
	}
	if !havePart || !haveLV {
		return nil, errors.New("udf: no partition or logical volume descriptor")
	}
	if mapPartition != partNumber {
		return nil, fmt.Errorf("udf: the partition map names partition %d, and the partition descriptor %d", mapPartition, partNumber)
	}
	if f.block != sectorSize {
		return nil, fmt.Errorf("udf: a logical block of %d bytes; only %d is read", f.block, sectorSize)
	}
	set, err := readTag(r, f.offset(fsd.block), tagFileSet)
	if err != nil {
		return nil, fmt.Errorf("udf: file set descriptor: %w", err)
	}
	f.root = parseLongAD(set[400:])
	return f, nil
}

func (f *FS) offset(block uint32) int64 { return (f.partStart + int64(block)) * f.block }

// File is a file in the image. It reads at any offset, through its extents.
type File struct {
	Name  string
	Size  int64
	Dir   bool
	fs    *FS
	exts  []extent
	data  []byte // inline: the whole file
	entry longAD
}

type extent struct {
	off    int64 // within the file
	length int64
	at     int64 // within the image; -1 for one never recorded, which reads as zeros
}

// ReadAt reads from the file as an io.ReaderAt.
func (fl *File) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("udf: negative offset")
	}
	if off >= fl.Size {
		return 0, io.EOF
	}
	want := len(p)
	if rest := fl.Size - off; int64(want) > rest {
		p = p[:rest]
	}
	if fl.data != nil {
		n := copy(p, fl.data[off:])
		if n < want {
			return n, io.EOF
		}
		return n, nil
	}
	n := 0
	for _, e := range fl.exts {
		if len(p) == 0 {
			break
		}
		if off >= e.off+e.length || off+int64(len(p)) <= e.off {
			continue
		}
		in := off - e.off
		chunk := p[:min(int64(len(p)), e.length-in)]
		if e.at < 0 {
			clear(chunk)
		} else if _, err := fl.fs.r.ReadAt(chunk, e.at+in); err != nil {
			return n, err
		}
		n += len(chunk)
		p, off = p[len(chunk):], off+int64(len(chunk))
	}
	if n < want {
		return n, io.EOF
	}
	return n, nil
}

// Reader reads the file from its start.
func (fl *File) Reader() *io.SectionReader { return io.NewSectionReader(fl, 0, fl.Size) }

// Open finds name, a slash-separated path from the root, matched without
// regard to case as Windows matches it.
func (f *FS) Open(name string) (*File, error) {
	cur, err := f.entry(f.root, "/")
	if err != nil {
		return nil, err
	}
	clean := strings.Trim(path.Clean("/"+name), "/")
	if clean == "" {
		return cur, nil
	}
	for part := range strings.SplitSeq(clean, "/") {
		if !cur.Dir {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		entries, err := f.readDir(cur)
		if err != nil {
			return nil, err
		}
		var next *longAD
		for _, e := range entries {
			if strings.EqualFold(e.name, part) {
				next = &e.icb
				break
			}
		}
		if next == nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		if cur, err = f.entry(*next, part); err != nil {
			return nil, err
		}
	}
	return cur, nil
}

// ReadDir lists the directory name.
func (f *FS) ReadDir(name string) ([]string, error) {
	dir, err := f.Open(name)
	if err != nil {
		return nil, err
	}
	if !dir.Dir {
		return nil, fmt.Errorf("udf: %s is not a directory", name)
	}
	entries, err := f.readDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.name
	}
	return names, nil
}

type dirEntry struct {
	name string
	icb  longAD
}

func (f *FS) readDir(dir *File) ([]dirEntry, error) {
	if dir.Size > maxDirBytes {
		return nil, fmt.Errorf("udf: directory %s is %d bytes", dir.Name, dir.Size)
	}
	buf := make([]byte, dir.Size)
	if _, err := dir.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var entries []dirEntry
	for p := 0; p+38 <= len(buf); {
		if le16(buf[p:]) != tagFileIdentifier {
			return nil, fmt.Errorf("udf: directory %s: no file identifier descriptor at %d", dir.Name, p)
		}
		chars := buf[p+18]
		nameLen := int(buf[p+19])
		icb := parseLongAD(buf[p+20:])
		implLen := int(le16(buf[p+36:]))
		end := p + 38 + implLen + nameLen
		if end > len(buf) {
			return nil, fmt.Errorf("udf: directory %s: a file identifier runs past its end", dir.Name)
		}
		const deleted, parent = 1 << 2, 1 << 3
		if chars&(deleted|parent) == 0 && nameLen > 0 {
			entries = append(entries, dirEntry{name: dstring(buf[p+38+implLen : end]), icb: icb})
		}
		p = (end + 3) &^ 3
	}
	return entries, nil
}

// entry reads the file entry at icb.
func (f *FS) entry(icb longAD, name string) (*File, error) {
	buf := make([]byte, f.block)
	if _, err := f.r.ReadAt(buf, f.offset(icb.block)); err != nil {
		return nil, fmt.Errorf("udf: file entry of %s: %w", name, err)
	}
	var size int64
	var eaLen, adLen, adStart int
	switch le16(buf) {
	case tagFileEntry:
		size = int64(le64(buf[56:]))
		eaLen, adLen, adStart = int(le32(buf[168:])), int(le32(buf[172:])), 176
	case tagExtendedFileEnt:
		size = int64(le64(buf[56:]))
		eaLen, adLen, adStart = int(le32(buf[208:])), int(le32(buf[212:])), 216
	default:
		return nil, fmt.Errorf("udf: %s: no file entry at block %d", name, icb.block)
	}
	if size < 0 {
		return nil, fmt.Errorf("udf: %s: a negative size", name)
	}
	adStart += eaLen
	if adStart+adLen > len(buf) {
		return nil, fmt.Errorf("udf: %s: allocation descriptors run past the entry", name)
	}
	const fileTypeDir = 4
	fl := &File{Name: name, Size: size, Dir: buf[16+11] == fileTypeDir, fs: f, entry: icb}
	ads := buf[adStart : adStart+adLen]
	switch flags := le16(buf[16+18:]) & 7; flags {
	case 3: // inline
		if int64(len(ads)) < size {
			return nil, fmt.Errorf("udf: %s: inline data shorter than the file", name)
		}
		fl.data = append([]byte(nil), ads[:size]...)
		return fl, nil
	case 0, 1:
		if err := f.extents(fl, ads, flags == 1); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("udf: %s: allocation descriptor type %d is not read", name, flags)
	}
	return fl, nil
}

// extents appends ads' extents to fl, following continuations.
func (f *FS) extents(fl *File, ads []byte, long bool) error {
	adSize := 8
	if long {
		adSize = 16
	}
	var off int64
	for hops := 0; ; hops++ {
		next := int64(-1)
		for p := 0; p+adSize <= len(ads); p += adSize {
			length := le32(ads[p:])
			kind, n := length>>30, int64(length&0x3FFFFFFF)
			if n == 0 {
				break
			}
			block := le32(ads[p+4:])
			if long {
				if part := le16(ads[p+8:]); part != 0 {
					return fmt.Errorf("udf: %s: an extent in partition reference %d", fl.Name, part)
				}
			}
			if kind == 3 {
				next = f.offset(block)
				break
			}
			at := f.offset(block)
			if kind != 0 {
				at = -1
			}
			if len(fl.exts) >= maxExtents {
				return fmt.Errorf("udf: %s has more than %d extents", fl.Name, maxExtents)
			}
			fl.exts = append(fl.exts, extent{off: off, length: n, at: at})
			off += n
		}
		if next < 0 {
			break
		}
		if hops >= maxExtents {
			return fmt.Errorf("udf: %s: too many allocation extents", fl.Name)
		}
		buf := make([]byte, f.block)
		if _, err := f.r.ReadAt(buf, next); err != nil {
			return err
		}
		if le16(buf) != tagAllocExtent {
			return fmt.Errorf("udf: %s: no allocation extent descriptor where one continues", fl.Name)
		}
		l := int(le32(buf[20:]))
		if 24+l > len(buf) {
			return fmt.Errorf("udf: %s: an allocation extent runs past its block", fl.Name)
		}
		ads = buf[24 : 24+l]
	}
	if off < fl.Size {
		return fmt.Errorf("udf: %s: extents hold %d of its %d bytes", fl.Name, off, fl.Size)
	}
	return nil
}

func readTag(r io.ReaderAt, at int64, want uint16) ([]byte, error) {
	buf := make([]byte, sectorSize)
	if _, err := r.ReadAt(buf, at); err != nil {
		return nil, err
	}
	if id := le16(buf); id != want {
		return nil, fmt.Errorf("descriptor tag %d, not %d", id, want)
	}
	var sum byte
	for i, b := range buf[:16] {
		if i != 4 {
			sum += b
		}
	}
	if sum != buf[4] {
		return nil, errors.New("descriptor tag checksum does not match")
	}
	return buf, nil
}

func parseLongAD(b []byte) longAD {
	return longAD{length: le32(b), block: le32(b[4:]), partition: le16(b[8:])}
}

// dstring decodes an OSTA compressed unicode name: a compression ID of 8
// (one byte a character) or 16 (UTF-16, big-endian).
func dstring(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	switch b[0] {
	case 8:
		r := make([]rune, len(b)-1)
		for i, c := range b[1:] {
			r[i] = rune(c)
		}
		return string(r)
	case 16:
		u := make([]uint16, (len(b)-1)/2)
		for i := range u {
			u[i] = binary.BigEndian.Uint16(b[1+2*i:])
		}
		return string(utf16.Decode(u))
	}
	return ""
}

func le16(b []byte) uint16 { return binary.LittleEndian.Uint16(b) }
func le32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }
