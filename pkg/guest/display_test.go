package guest

import (
	"bytes"
	"testing"
)

// Rows that changed come back as full-width strips, runs of them as one.
func TestChangedRows(t *testing.T) {
	const w, h = 4, 6
	prev := make([]byte, w*h*4)
	cur := bytes.Clone(prev)
	for _, y := range []int{1, 2, 5} {
		cur[y*w*4] = 1
	}
	got := changedRows(prev, cur, w, h)
	want := []DisplayRectangle{{0, 1, w, 2}, {0, 5, w, 1}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("changedRows = %v, want %v", got, want)
	}
	if got := changedRows(prev, prev, w, h); len(got) != 0 {
		t.Fatalf("an unchanged frame has changes: %v", got)
	}
}

// A rectangle too large for one frame goes as strips that fit, and together
// carry exactly its pixels.
func TestDisplayRectsSplit(t *testing.T) {
	const w, h = 1920, 1080
	pixels := make([]byte, w*h*4)
	for i := range pixels {
		pixels[i] = byte(i)
	}
	var stream bytes.Buffer
	if err := writeDisplayRects(&stream, w, pixels, []DisplayRectangle{{0, 0, w, h}}); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(pixels))
	frames := 0
	for stream.Len() > 0 {
		typ, payload, err := ReadFrame(&stream)
		if err != nil {
			t.Fatal(err)
		}
		if typ != DisplayRect {
			t.Fatalf("frame type %d", typ)
		}
		v, err := ParseUint32s(payload, 4)
		if err != nil {
			t.Fatal(err)
		}
		x, y, rw, rh := int(v[0]), int(v[1]), int(v[2]), int(v[3])
		for row := range rh {
			copy(got[((y+row)*w+x)*4:], payload[16+row*rw*4:16+(row+1)*rw*4])
		}
		frames++
	}
	if frames < 2 || !bytes.Equal(got, pixels) {
		t.Fatalf("%d frames, pixels equal: %v", frames, bytes.Equal(got, pixels))
	}
}
