package vz

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// isoFile is one regular file in the root of an ISO 9660 image.
type isoFile struct {
	// Name is the file's name as Rock Ridge gives it.
	Name string
	// Mode is its POSIX permission bits, as Rock Ridge gives them.
	Mode uint32
	Data []byte
}

const isoSector = 2048

// isoEpoch is every timestamp an image records: the same files always give
// the same bytes (ADR 26-11-13-318 §2).
var isoEpoch = [7]byte{80, 1, 1, 0, 0, 0, 0} // 1980-01-01 00:00:00 GMT

// The image's layout: sectors 0 to 15 are the empty system area, then the
// primary volume descriptor, the set's terminator, the two path tables, the
// root directory, the continuation area holding the Rock Ridge extension
// reference, and the files' data, one after another.
const (
	pvdSector  = 16
	termSector = 17
	lPathTable = 18
	mPathTable = 19
	rootSector = 20
	ceSector   = 21
	dataSector = 22
)

// The Rock Ridge extension reference (IEEE P1282 as RRIP 1991A), which the
// root's first record must lead to for macOS's and BSD's cd9660 to read
// Rock Ridge at all.
const (
	rripID     = "RRIP_1991A"
	rripDesc   = "THE ROCK RIDGE INTERCHANGE PROTOCOL PROVIDES SUPPORT FOR POSIX FILE SYSTEM SEMANTICS"
	rripSource = "PLEASE CONTACT DISC PUBLISHER FOR SPECIFICATION SOURCE.  SEE PUBLISHER IDENTIFIER IN PRIMARY VOLUME DESCRIPTOR FOR CONTACT INFORMATION."
)

// writeISO writes an ISO 9660 image (level 2) whose root directory holds
// files and nothing else, with Rock Ridge names and modes, under the volume
// label volume (at most 32 of A to Z, 0 to 9 and _). It is only what the
// agent's drive needs: one directory, owned by root, each file at most 4 GiB
// less a byte, and every timestamp fixed, so that the same files always
// give the same bytes.
func writeISO(w io.Writer, volume string, files []isoFile) error {
	if volume == "" || len(volume) > 32 || strings.Trim(volume, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") != "" {
		return fmt.Errorf("iso9660: volume label %q is not 1 to 32 of A-Z, 0-9 and _", volume)
	}
	files = append([]isoFile(nil), files...)
	ids := map[string]bool{}
	for _, f := range files {
		// A directory record is at most 255 bytes, and holds the name twice.
		if f.Name == "" || f.Name == "." || f.Name == ".." || strings.ContainsAny(f.Name, "/\x00") || len(f.Name) > 100 {
			return fmt.Errorf("iso9660: %q is not a file name", f.Name)
		}
		if uint64(len(f.Data)) > 1<<32-1 {
			return fmt.Errorf("iso9660: %s is over 4 GiB", f.Name)
		}
		id := isoIdentifier(f.Name)
		if ids[id] {
			return fmt.Errorf("iso9660: %s's ISO 9660 name %s is another file's", f.Name, id)
		}
		ids[id] = true
	}
	// A directory's records are in the order of their identifiers.
	sort.Slice(files, func(i, j int) bool { return isoIdentifier(files[i].Name) < isoIdentifier(files[j].Name) })

	er := suspER()
	root := dirRecord(rootSector, isoSector, true, []byte{0},
		concat(suspSP(), suspCE(ceSector, len(er)), rripPX(0o040755, 2)))
	parent := dirRecord(rootSector, isoSector, true, []byte{1}, rripPX(0o040755, 2))
	dir := concat(root, parent)
	extents := make([]uint32, len(files))
	next := uint32(dataSector)
	for i, f := range files {
		extents[i] = next
		next += uint32((len(f.Data) + isoSector - 1) / isoSector)
		rec := dirRecord(extents[i], uint32(len(f.Data)), false, []byte(isoIdentifier(f.Name)),
			concat(rripPX(0o100000|f.Mode&0o7777, 1), rripNM(f.Name)))
		dir = append(dir, rec...)
	}
	if len(dir) > isoSector {
		return errors.New("iso9660: the root directory is over one sector")
	}
	total := next

	image := make([]byte, int(dataSector)*isoSector)
	copy(image[pvdSector*isoSector:], primaryDescriptor(volume, total, root))
	term := image[termSector*isoSector:]
	term[0] = 255
	copy(term[1:], "CD001")
	term[6] = 1
	// The path tables: the root alone, its own parent.
	pt := image[lPathTable*isoSector:]
	pt[0] = 1
	binary.LittleEndian.PutUint32(pt[2:], rootSector)
	binary.LittleEndian.PutUint16(pt[6:], 1)
	pt = image[mPathTable*isoSector:]
	pt[0] = 1
	binary.BigEndian.PutUint32(pt[2:], rootSector)
	binary.BigEndian.PutUint16(pt[6:], 1)
	copy(image[rootSector*isoSector:], dir)
	copy(image[ceSector*isoSector:], er)
	if _, err := w.Write(image); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := w.Write(f.Data); err != nil {
			return err
		}
		if pad := (isoSector - len(f.Data)%isoSector) % isoSector; pad > 0 {
			if _, err := w.Write(make([]byte, pad)); err != nil {
				return err
			}
		}
	}
	return nil
}

// isoIdentifier is a file's ISO 9660 identifier: its name in upper case,
// every character but A to Z, 0 to 9 and _ an underscore, one dot before
// the extension, and the version ";1". A reader that knows Rock Ridge shows
// the name instead.
func isoIdentifier(name string) string {
	base, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		base, ext = name[:i], name[i+1:]
	}
	clean := func(s string, n int) string {
		s = strings.Map(func(r rune) rune {
			switch {
			case r >= 'a' && r <= 'z':
				return r - 'a' + 'A'
			case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				return r
			}
			return '_'
		}, s)
		return s[:min(len(s), n)]
	}
	// Level 2 allows 30 characters of name and extension together.
	ext = clean(ext, 8)
	return clean(base, 30-len(ext)) + "." + ext + ";1"
}

// dirRecord is one directory record, with its system use area su.
func dirRecord(extent, size uint32, dir bool, id, su []byte) []byte {
	n := 33 + len(id)
	if len(id)%2 == 0 {
		n++ // pads the identifier to an even length
	}
	rec := make([]byte, n, n+len(su)+1)
	both32(rec[2:], extent)
	both32(rec[10:], size)
	copy(rec[18:25], isoEpoch[:])
	if dir {
		rec[25] = 2
	}
	both16(rec[28:], 1)
	rec[32] = byte(len(id))
	copy(rec[33:], id)
	rec = append(rec, su...)
	if len(rec)%2 == 1 {
		rec = append(rec, 0)
	}
	rec[0] = byte(len(rec))
	return rec
}

// primaryDescriptor is the volume's primary descriptor, of total sectors,
// with root as its root directory's record.
func primaryDescriptor(volume string, total uint32, root []byte) []byte {
	d := make([]byte, isoSector)
	d[0] = 1
	copy(d[1:], "CD001")
	d[6] = 1
	copy(d[8:40], pad("", 32))
	copy(d[40:72], pad(volume, 32))
	both32(d[80:], total)
	both16(d[120:], 1)
	both16(d[124:], 1)
	both16(d[128:], isoSector)
	both32(d[132:], 10)
	binary.LittleEndian.PutUint32(d[140:], lPathTable)
	binary.BigEndian.PutUint32(d[148:], mPathTable)
	// The descriptor's copy of the root's record is the bare 34 bytes.
	copy(d[156:190], root[:34])
	d[156] = 34
	copy(d[190:813], pad("", 813-190))
	date := []byte("1980010100000000\x00")
	for _, at := range []int{813, 830, 864} {
		copy(d[at:], date)
	}
	copy(d[847:], "0000000000000000\x00")
	d[881] = 1
	return d
}

// suspSP is the System Use Sharing Protocol's indicator, first in the
// root's own record.
func suspSP() []byte { return []byte{'S', 'P', 7, 1, 0xBE, 0xEF, 0} }

// suspCE continues the root's system use area at sector, for n bytes.
func suspCE(sector uint32, n int) []byte {
	e := []byte{'C', 'E', 28, 1}
	e = append(e, make([]byte, 24)...)
	both32(e[4:], sector)
	both32(e[12:], 0)
	both32(e[20:], uint32(n))
	return e
}

// suspER references the Rock Ridge extension.
func suspER() []byte {
	e := []byte{'E', 'R', byte(8 + len(rripID) + len(rripDesc) + len(rripSource)), 1,
		byte(len(rripID)), byte(len(rripDesc)), byte(len(rripSource)), 1}
	return concat(e, []byte(rripID), []byte(rripDesc), []byte(rripSource))
}

// rripPX is a file's POSIX mode and link count, owned by root.
func rripPX(mode uint32, links uint32) []byte {
	e := make([]byte, 36)
	copy(e, "PX")
	e[2], e[3] = 36, 1
	both32(e[4:], mode)
	both32(e[12:], links)
	return e
}

// rripNM is a file's name.
func rripNM(name string) []byte {
	return concat([]byte{'N', 'M', byte(5 + len(name)), 1, 0}, []byte(name))
}

func both16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
	binary.BigEndian.PutUint16(b[2:], v)
}

func both32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b, v)
	binary.BigEndian.PutUint32(b[4:], v)
}

func pad(s string, n int) []byte { return []byte(s + strings.Repeat(" ", n-len(s))) }

func concat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
