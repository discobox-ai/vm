package iso9660

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
	"unicode/utf16"
)

func file(p, content string) File {
	return File{Path: p, Size: int64(len(content)), Open: func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(content)), nil
	}}
}

// An image reads back, through both its trees, as the files it was written
// from, and its El Torito catalog points at the boot image.
func TestRoundTrip(t *testing.T) {
	big := strings.Repeat("0123456789abcdef", 300) // spans sectors
	files := []File{
		file("autounattend.xml", "<unattend/>"),
		file("disco-vm/disco-vm.exe", "MZ agent"),
		file("disco-vm/specialize.cmd", "@echo off\r\n"),
		file("efi/microsoft/boot/efisys_noprompt.bin", big),
		file("efi/microsoft/boot/bcd", "bcd"),
		file("sources/boot.wim", "wim"),
		file("empty.txt", ""),
	}
	// Enough files in one directory that its records cross a sector.
	for i := range 60 {
		files = append(files, file("efi/microsoft/boot/fonts/font-number-"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune('a'+i/26))+".ttf", "f"))
	}
	var buf bytes.Buffer
	if err := Write(&buf, Image{VolumeID: "DISCOVM", Files: files, EFIBoot: "efi/microsoft/boot/efisys_noprompt.bin"}); err != nil {
		t.Fatal(err)
	}
	img := buf.Bytes()
	if len(img)%sector != 0 {
		t.Fatalf("image is %d bytes, not whole sectors", len(img))
	}
	pvd := img[16*sector : 17*sector]
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		t.Fatal("no primary volume descriptor at sector 16")
	}
	if got := binary.LittleEndian.Uint32(pvd[80:]); int(got)*sector != len(img) {
		t.Fatalf("volume space %d sectors, image %d", got, len(img)/sector)
	}
	if got := strings.TrimSpace(string(pvd[40:72])); got != "DISCOVM" {
		t.Fatalf("volume ID %q", got)
	}
	br := img[17*sector : 18*sector]
	if br[0] != 0 || !strings.HasPrefix(string(br[7:]), "EL TORITO SPECIFICATION") {
		t.Fatal("no El Torito boot record at sector 17")
	}
	svd := img[18*sector : 19*sector]
	if svd[0] != 2 || string(svd[88:91]) != "%/E" {
		t.Fatal("no Joliet descriptor at sector 18")
	}
	if img[19*sector] != 255 {
		t.Fatal("no terminator at sector 19")
	}

	for _, joliet := range []bool{false, true} {
		desc := pvd
		if joliet {
			desc = svd
		}
		got := map[string]string{}
		readTree(t, img, desc[156:190], "", joliet, got)
		for _, f := range files {
			want, _ := io.ReadAll(must(f.Open()))
			key := f.Path
			if !joliet {
				key = primaryPath(f.Path)
			}
			if got[key] != string(want) {
				t.Errorf("joliet=%v: %s reads %d bytes, want %d", joliet, key, len(got[key]), len(want))
			}
		}
		if len(got) != len(files) {
			t.Errorf("joliet=%v: %d files, want %d", joliet, len(got), len(files))
		}
	}

	at := binary.LittleEndian.Uint32(br[71:])
	cat := img[int(at)*sector:]
	var sum uint16
	for i := 0; i < 32; i += 2 {
		sum += binary.LittleEndian.Uint16(cat[i:])
	}
	if cat[0] != 1 || sum != 0 || cat[30] != 0x55 || cat[31] != 0xAA {
		t.Fatal("the validation entry is not valid")
	}
	if cat[64] != 0x91 || cat[65] != 0xEF {
		t.Fatal("no final EFI section")
	}
	for _, e := range [][]byte{cat[32:64], cat[96:128]} {
		lba := binary.LittleEndian.Uint32(e[8:])
		count := binary.LittleEndian.Uint16(e[6:])
		if e[0] != 0x88 || int(count) != (len(big)+511)/512 {
			t.Fatalf("boot entry %x", e[:12])
		}
		if string(img[int(lba)*sector:int(lba)*sector+len(big)]) != big {
			t.Fatal("a boot entry does not point at the boot image")
		}
	}
}

func TestRefuses(t *testing.T) {
	for name, im := range map[string]Image{
		"a missing boot image": {Files: []File{file("a", "a")}, EFIBoot: "b"},
		"a file twice":         {Files: []File{file("a/b", "1"), file("A/B", "2")}},
		"a file and a dir":     {Files: []File{file("a", "1"), file("a/b", "2")}},
		"a long volume ID":     {VolumeID: strings.Repeat("V", 17)},
	} {
		if err := Write(io.Discard, im); err == nil {
			t.Errorf("%s: written", name)
		}
	}
}

func must(r io.ReadCloser, err error) io.ReadCloser {
	if err != nil {
		panic(err)
	}
	return r
}

func primaryPath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = primaryName(&node{name: part, dir: i < len(parts)-1})
	}
	return strings.Join(parts, "/")
}

// readTree reads the directory a record names into got, by path.
func readTree(t *testing.T, img, rec []byte, prefix string, joliet bool, got map[string]string) {
	t.Helper()
	at := int(binary.LittleEndian.Uint32(rec[2:])) * sector
	size := int(binary.LittleEndian.Uint32(rec[10:]))
	if binary.BigEndian.Uint32(rec[6:]) != binary.LittleEndian.Uint32(rec[2:]) {
		t.Fatal("an extent's two byte orders differ")
	}
	dir := img[at : at+size]
	for s := 0; s < size; s += sector {
		blk := dir[s : s+sector]
		for p := 0; p < sector && blk[p] != 0; p += int(blk[p]) {
			r := blk[p : p+int(blk[p])]
			id := r[33 : 33+int(r[32])]
			if len(id) == 1 && id[0] <= 1 {
				continue
			}
			name := string(id)
			if joliet {
				u := make([]uint16, len(id)/2)
				for i := range u {
					u[i] = binary.BigEndian.Uint16(id[2*i:])
				}
				name = string(utf16.Decode(u))
			}
			path := prefix + name
			if r[25]&2 != 0 {
				readTree(t, img, r, path+"/", joliet, got)
				continue
			}
			fat := int(binary.LittleEndian.Uint32(r[2:])) * sector
			got[path] = string(img[fat : fat+int(binary.LittleEndian.Uint32(r[10:]))])
		}
	}
}
