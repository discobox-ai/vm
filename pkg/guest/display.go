package guest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// The display protocol carries a guest's desktop to a host viewer, and the
// viewer's input back, on one stream to DisplayPort, framed as exec is
// (WriteFrame). It is sandboxi's guestfb, whose three hvsocket channels are
// one stream here: the desktop is captured with GDI and sent as raw BGRA
// rectangles of what changed, the cursor as a standard shape's number, and
// input is injected with SendInput. It needs nothing of the host's but the
// stream, so it works where the host's own video console does not.
//
// The guest sends DisplayScreen first, then every rectangle of the desktop
// and a DisplayFlush; after that, the rectangles that changed and a flush per
// frame, and a DisplayScreen again whenever the desktop's size changes.
const (
	DisplayInput byte = 5 // host to guest: kind, a, b, c uint32

	DisplayScreen byte = 20 // width, height uint32
	DisplayRect   byte = 21 // x, y, width, height uint32, then width*height BGRA pixels, top-down
	DisplayFlush  byte = 22 // no payload: the rectangles since the last flush are one frame
	DisplayCursor byte = 23 // visible, shape uint32
)

// Input kinds, the a, b, c of a DisplayInput.
const (
	InputMove       uint32 = 0 // x, y in desktop pixels
	InputButton     uint32 = 1 // button, down (0 or 1)
	InputWheel      uint32 = 2 // delta, an int32 in WHEEL_DELTA units
	InputKey        uint32 = 3 // scan code, KeyExtended|KeyUp
	InputResolution uint32 = 4 // width, height the viewer would like the desktop to be
)

// Buttons of an InputButton.
const (
	ButtonLeft   uint32 = 0
	ButtonRight  uint32 = 1
	ButtonMiddle uint32 = 2
)

// Flags of an InputKey.
const (
	KeyExtended uint32 = 1
	KeyUp       uint32 = 2
)

// Cursor shapes of a DisplayCursor: the standard system cursors, so the
// viewer draws the shape itself rather than receiving bitmaps.
const (
	CursorArrow uint32 = iota
	CursorIBeam
	CursorHand
	CursorWait
	CursorAppStarting
	CursorSizeAll
	CursorSizeNS
	CursorSizeWE
	CursorSizeNESW
	CursorSizeNWSE
	CursorCross
	CursorNo
	CursorHelp
	CursorUpArrow
	CursorOther uint32 = 255
)

// DisplayRectangle is a part of the desktop, in pixels.
type DisplayRectangle struct{ X, Y, W, H int }

func uint32s(values ...uint32) []byte {
	b := make([]byte, 4*len(values))
	for i, v := range values {
		binary.BigEndian.PutUint32(b[4*i:], v)
	}
	return b
}

// ParseUint32s reads n big-endian uint32s from the front of a payload.
func ParseUint32s(payload []byte, n int) ([]uint32, error) {
	if len(payload) < 4*n {
		return nil, fmt.Errorf("guest: display payload of %d bytes, want at least %d", len(payload), 4*n)
	}
	values := make([]uint32, n)
	for i := range values {
		values[i] = binary.BigEndian.Uint32(payload[4*i:])
	}
	return values, nil
}

// WriteDisplayInput sends one input event.
func WriteDisplayInput(w io.Writer, kind, a, b, c uint32) error {
	return WriteFrame(w, DisplayInput, uint32s(kind, a, b, c))
}

// writeDisplayRects sends the rectangles of a width-pixel-wide desktop,
// pixels being all of it, top-down BGRA, splitting any rectangle too large for
// one frame into strips.
func writeDisplayRects(w io.Writer, width int, pixels []byte, rects []DisplayRectangle) error {
	for _, r := range rects {
		rows := max(1, (maxFrame-16)/(r.W*4))
		for y := r.Y; y < r.Y+r.H; y += rows {
			h := min(rows, r.Y+r.H-y)
			payload := make([]byte, 16, 16+r.W*h*4)
			copy(payload, uint32s(uint32(r.X), uint32(y), uint32(r.W), uint32(h)))
			for row := y; row < y+h; row++ {
				start := (row*width + r.X) * 4
				payload = append(payload, pixels[start:start+r.W*4]...)
			}
			if err := WriteFrame(w, DisplayRect, payload); err != nil {
				return err
			}
		}
	}
	return nil
}

// changedRows is the full-width strips of rows that differ between two
// frames of the same size, as guestfb finds them: coarse, and cheap.
func changedRows(prev, cur []byte, width, height int) []DisplayRectangle {
	stride := width * 4
	var rects []DisplayRectangle
	start := -1
	for y := range height {
		row := y * stride
		changed := !bytes.Equal(prev[row:row+stride], cur[row:row+stride])
		switch {
		case changed && start < 0:
			start = y
		case !changed && start >= 0:
			rects = append(rects, DisplayRectangle{0, start, width, y - start})
			start = -1
		}
	}
	if start >= 0 {
		rects = append(rects, DisplayRectangle{0, start, width, height - start})
	}
	return rects
}
