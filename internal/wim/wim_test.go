package wim

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unicode/utf16"
)

// fakeWIM is a header and an XML resource after it, which is all Images reads.
func fakeWIM(xmlText string) []byte {
	u := utf16.Encode([]rune(string(rune(0xFEFF)) + xmlText))
	data := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(data[2*i:], c)
	}
	h := make([]byte, headerSize)
	copy(h, "MSWIM\x00\x00\x00")
	binary.LittleEndian.PutUint64(h[72+8:], headerSize)
	binary.LittleEndian.PutUint64(h[72+16:], uint64(len(data)))
	return append(h, data...)
}

const editions = `<WIM><TOTALBYTES>1</TOTALBYTES>
<IMAGE INDEX="1"><NAME>Windows 11 Home</NAME><WINDOWS><EDITIONID>Core</EDITIONID></WINDOWS></IMAGE>
<IMAGE INDEX="2"><NAME>Windows 11 Education</NAME><WINDOWS><EDITIONID>Education</EDITIONID></WINDOWS></IMAGE>
<IMAGE INDEX="3"><NAME> Windows 11 Pro </NAME><WINDOWS><EDITIONID>Professional</EDITIONID></WINDOWS></IMAGE>
<IMAGE INDEX="4"><NAME>Windows 11 Pro N</NAME><WINDOWS><EDITIONID>ProfessionalN</EDITIONID></WINDOWS></IMAGE>
</WIM>`

func TestImages(t *testing.T) {
	images, err := Images(bytes.NewReader(fakeWIM(editions)))
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 4 {
		t.Fatalf("%d images", len(images))
	}
	if got := images[2]; got != (Image{Index: 3, Name: "Windows 11 Pro", EditionID: "Professional"}) {
		t.Fatalf("image 3 is %+v", got)
	}
}

func TestRefuses(t *testing.T) {
	huge := fakeWIM(editions)
	binary.LittleEndian.PutUint64(huge[72+16:], maxXML+2)
	for name, b := range map[string][]byte{
		"not a WIM":     append([]byte("NOTAWIM\x00"), make([]byte, headerSize)...),
		"a short file":  []byte("MSWIM\x00\x00\x00"),
		"no images":     fakeWIM(`<WIM></WIM>`),
		"a bad index":   fakeWIM(`<WIM><IMAGE INDEX="x"><NAME>a</NAME></IMAGE></WIM>`),
		"a huge XML":    huge,
		"malformed XML": fakeWIM(`<WIM><IMAGE`),
	} {
		if _, err := Images(bytes.NewReader(b)); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}
