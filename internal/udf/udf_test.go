package udf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

// image is a UDF image under construction, sector by sector: a volume
// descriptor sequence at 32, the anchor at 256, and the partition from 300.
type image struct {
	b []byte
}

const partStart = 300

func newImage() *image { return &image{b: make([]byte, 400*sectorSize)} }

func (im *image) sector(n int) []byte { return im.b[n*sectorSize : (n+1)*sectorSize] }
func (im *image) block(n int) []byte  { return im.sector(partStart + n) }

// tag stamps a descriptor tag and its checksum on d.
func tag(d []byte, id uint16) {
	binary.LittleEndian.PutUint16(d, id)
	d[4] = 0
	var sum byte
	for i, c := range d[:16] {
		if i != 4 {
			sum += c
		}
	}
	d[4] = sum
}

func putLongAD(b []byte, length, block uint32) {
	binary.LittleEndian.PutUint32(b, length)
	binary.LittleEndian.PutUint32(b[4:], block)
}

// volume writes the anchor, the volume descriptors, and the file set
// descriptor, whose root directory's entry is at block 1.
func (im *image) volume() {
	a := im.sector(256)
	binary.LittleEndian.PutUint32(a[16:], 3*sectorSize)
	binary.LittleEndian.PutUint32(a[20:], 32)
	tag(a, tagAnchor)

	pd := im.sector(32)
	binary.LittleEndian.PutUint16(pd[22:], 7)
	binary.LittleEndian.PutUint32(pd[188:], partStart)
	tag(pd, tagPartition)

	lv := im.sector(33)
	binary.LittleEndian.PutUint32(lv[212:], sectorSize)
	putLongAD(lv[248:], sectorSize, 0)
	binary.LittleEndian.PutUint32(lv[268:], 1)
	lv[440], lv[441] = 1, 6
	binary.LittleEndian.PutUint16(lv[444:], 7)
	tag(lv, tagLogicalVolume)

	tag(im.sector(34), tagTerminating)

	fsd := im.block(0)
	putLongAD(fsd[400:], sectorSize, 1)
	tag(fsd, tagFileSet)
}

// entry writes a file entry at block n: a directory or a file of size, its
// allocation descriptors of kind adType (0 short, 1 long, 3 inline).
func (im *image) entry(n int, extended, dir bool, size int, adType uint16, ads []byte) {
	e := im.block(n)
	id, adStart := uint16(tagFileEntry), 176
	if extended {
		id, adStart = tagExtendedFileEnt, 216
	}
	if dir {
		e[16+11] = 4
	} else {
		e[16+11] = 5
	}
	binary.LittleEndian.PutUint16(e[16+18:], adType)
	binary.LittleEndian.PutUint64(e[56:], uint64(size))
	binary.LittleEndian.PutUint32(e[adStart-4:], uint32(len(ads)))
	copy(e[adStart:], ads)
	tag(e, id)
}

func shortAD(kind, length, block uint32) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b, kind<<30|length)
	binary.LittleEndian.PutUint32(b[4:], block)
	return b
}

func longAD16(kind, length, block uint32) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint32(b, kind<<30|length)
	binary.LittleEndian.PutUint32(b[4:], block)
	return b
}

// fid is a file identifier descriptor, padded to four bytes.
func fid(name string, wide bool, chars byte, icb uint32) []byte {
	var n []byte
	switch {
	case name == "":
	case wide:
		n = []byte{16}
		for _, u := range utf16.Encode([]rune(name)) {
			n = binary.BigEndian.AppendUint16(n, u)
		}
	default:
		n = append([]byte{8}, name...)
	}
	b := make([]byte, 38+len(n))
	binary.LittleEndian.PutUint16(b, tagFileIdentifier)
	b[18], b[19] = chars, byte(len(n))
	putLongAD(b[20:], sectorSize, icb)
	copy(b[38:], n)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

const (
	fidDir    = 1 << 1
	fidParent = 1 << 3
	fidGone   = 1 << 2
)

var (
	wimData  = strings.Repeat("W", 100)
	inline   = "inline data"
	bigFirst = bytes.Repeat([]byte("abcdefgh"), 375) // 3000 bytes
	bigLast  = bytes.Repeat([]byte("Z"), 500)
)

// sample is an image with every form the reader handles:
//
//	/a.txt       inline, in an extended file entry
//	/Sources/    a directory
//	/Sources/install.wim   short allocation descriptors
//	/big.bin     long ones: 3000 bytes, 1000 unrecorded (zeros), then 500
//	             more through an allocation extent descriptor
//	/ünï.txt     a UTF-16 name, in an extended file entry
//	/gone.txt    deleted
func sample() *image {
	im := newImage()
	im.volume()

	var root []byte
	root = append(root, fid("", false, fidParent, 1)...)
	root = append(root, fid("Sources", false, fidDir, 3)...)
	root = append(root, fid("a.txt", false, 0, 5)...)
	root = append(root, fid("big.bin", false, 0, 6)...)
	root = append(root, fid("ünï.txt", true, 0, 13)...)
	root = append(root, fid("gone.txt", false, fidGone, 5)...)
	copy(im.block(2), root)
	im.entry(1, false, true, len(root), 0, shortAD(0, uint32(len(root)), 2))

	src := append(fid("", false, fidParent, 1), fid("install.wim", false, 0, 8)...)
	copy(im.block(4), src)
	im.entry(3, true, true, len(src), 0, shortAD(0, uint32(len(src)), 4))
	copy(im.block(9), wimData)
	im.entry(8, false, false, len(wimData), 0, shortAD(0, uint32(len(wimData)), 9))

	im.entry(5, true, false, len(inline), 3, []byte(inline))

	copy(im.b[(partStart+10)*sectorSize:], bigFirst)
	copy(im.block(12), bigLast)
	aed := im.block(7)
	ads := longAD16(0, uint32(len(bigLast)), 12)
	binary.LittleEndian.PutUint32(aed[20:], uint32(len(ads)))
	copy(aed[24:], ads)
	tag(aed, tagAllocExtent)
	var big []byte
	big = append(big, longAD16(0, uint32(len(bigFirst)), 10)...)
	big = append(big, longAD16(1, 1000, 0)...)
	big = append(big, longAD16(3, sectorSize, 7)...)
	im.entry(6, false, false, len(bigFirst)+1000+len(bigLast), 1, big)

	im.entry(13, true, false, 2, 3, []byte("hi"))
	return im
}

func readAll(t *testing.T, f *FS, name string) []byte {
	t.Helper()
	fl, err := f.Open(name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	b, err := io.ReadAll(fl.Reader())
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

func TestRead(t *testing.T) {
	f, err := Open(bytes.NewReader(sample().b))
	if err != nil {
		t.Fatal(err)
	}
	names, err := f.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Sources", "a.txt", "big.bin", "ünï.txt"}; !slices.Equal(names, want) {
		t.Errorf("the root lists %q, want %q", names, want)
	}
	if got := string(readAll(t, f, "SOURCES/Install.WIM")); got != wimData {
		t.Errorf("install.wim reads %q", got)
	}
	if got := string(readAll(t, f, "a.txt")); got != inline {
		t.Errorf("a.txt reads %q", got)
	}
	if got := string(readAll(t, f, "/ÜNÏ.txt")); got != "hi" {
		t.Errorf("ünï.txt reads %q", got)
	}
	want := slices.Concat(bigFirst, make([]byte, 1000), bigLast)
	if got := readAll(t, f, "big.bin"); !bytes.Equal(got, want) {
		t.Errorf("big.bin reads %d bytes unlike the %d written", len(got), len(want))
	}
	// A read across all three extents, from inside the first.
	fl, _ := f.Open("big.bin")
	p := make([]byte, 2000)
	if n, err := fl.ReadAt(p, 2500); n != 2000 || err != nil || !bytes.Equal(p, want[2500:4500]) {
		t.Errorf("ReadAt across extents: %d, %v", n, err)
	}
	if n, err := fl.ReadAt(p, int64(len(want))-10); n != 10 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at the end: %d, %v", n, err)
	}
	if fl.Dir || fl.Size != int64(len(want)) {
		t.Errorf("big.bin is %+v", fl)
	}
	if dir, _ := f.Open("sources"); !dir.Dir {
		t.Error("sources is not a directory")
	}
	for _, name := range []string{"gone.txt", "nope", "a.txt/b", "sources/nope"} {
		if _, err := f.Open(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("open %s: %v, want not exist", name, err)
		}
	}
	if _, err := f.ReadDir("a.txt"); err == nil {
		t.Error("a file listed as a directory")
	}
}

func TestRefuses(t *testing.T) {
	for name, spoil := range map[string]func(im *image){
		"no anchor": func(im *image) { clear(im.sector(256)) },
		"a bad anchor checksum": func(im *image) {
			im.sector(256)[4]++
		},
		"two partition maps": func(im *image) {
			lv := im.sector(33)
			binary.LittleEndian.PutUint32(lv[268:], 2)
			tag(lv, tagLogicalVolume)
		},
		"512-byte blocks": func(im *image) {
			lv := im.sector(33)
			binary.LittleEndian.PutUint32(lv[212:], 512)
			tag(lv, tagLogicalVolume)
		},
		"a map of another partition": func(im *image) {
			lv := im.sector(33)
			binary.LittleEndian.PutUint16(lv[444:], 8)
			tag(lv, tagLogicalVolume)
		},
		"no file set": func(im *image) { clear(im.block(0)) },
	} {
		im := sample()
		spoil(im)
		if _, err := Open(bytes.NewReader(im.b)); err == nil {
			t.Errorf("%s: opened", name)
		}
	}

	for name, spoil := range map[string]func(im *image){
		"extents short of the size": func(im *image) {
			im.entry(8, false, false, 5000, 0, shortAD(0, 100, 9))
		},
		"no entry where a FID points": func(im *image) { clear(im.block(8)) },
		"a continuation to no descriptor": func(im *image) {
			clear(im.block(7))
		},
	} {
		im := sample()
		spoil(im)
		f, err := Open(bytes.NewReader(im.b))
		if err != nil {
			t.Fatal(err)
		}
		_, err1 := f.Open("sources/install.wim")
		_, err2 := f.Open("big.bin")
		if err1 == nil && err2 == nil {
			t.Errorf("%s: read", name)
		}
	}

	// A name a path cannot hold is refused, not walked into.
	for _, bad := range []string{".", "..", "a/b", "a\\b", "a\x00b"} {
		im := sample()
		src := append(fid("", false, fidParent, 1), fid(bad, false, fidDir, 3)...)
		copy(im.block(4), src)
		im.entry(3, true, true, len(src), 0, shortAD(0, uint32(len(src)), 4))
		f, err := Open(bytes.NewReader(im.b))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.ReadDir("sources"); err == nil {
			t.Errorf("a directory holding %q listed", bad)
		}
	}
}
