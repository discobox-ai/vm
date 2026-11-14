package hcs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/discobox-ai/vm/pkg/guest"
)

// The window that shows the agent's display stream (guest.DisplayPort): the
// host half of sandboxi's fbview. It draws the guest's desktop scaled to fit,
// draws the guest's cursor shape, and sends the mouse and every key, Win and
// Alt+Tab included, while it is in front. Resizing it asks the guest to match.

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	gdi32  = windows.NewLazySystemDLL("gdi32.dll")

	registerClassEx               = user32.NewProc("RegisterClassExW")
	createWindowEx                = user32.NewProc("CreateWindowExW")
	defWindowProc                 = user32.NewProc("DefWindowProcW")
	getMessage                    = user32.NewProc("GetMessageW")
	translateMessage              = user32.NewProc("TranslateMessage")
	dispatchMessage               = user32.NewProc("DispatchMessageW")
	postMessage                   = user32.NewProc("PostMessageW")
	postQuitMessage               = user32.NewProc("PostQuitMessage")
	showWindow                    = user32.NewProc("ShowWindow")
	updateWindow                  = user32.NewProc("UpdateWindow")
	invalidateRect                = user32.NewProc("InvalidateRect")
	beginPaint                    = user32.NewProc("BeginPaint")
	endPaint                      = user32.NewProc("EndPaint")
	getClientRect                 = user32.NewProc("GetClientRect")
	loadCursor                    = user32.NewProc("LoadCursorW")
	setCursor                     = user32.NewProc("SetCursor")
	setWindowsHookEx              = user32.NewProc("SetWindowsHookExW")
	unhookWindowsHookEx           = user32.NewProc("UnhookWindowsHookEx")
	callNextHookEx                = user32.NewProc("CallNextHookEx")
	getForegroundWindow           = user32.NewProc("GetForegroundWindow")
	setProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	createCompatibleDC            = gdi32.NewProc("CreateCompatibleDC")
	createCompatibleBitmap        = gdi32.NewProc("CreateCompatibleBitmap")
	selectObject                  = gdi32.NewProc("SelectObject")
	patBlt                        = gdi32.NewProc("PatBlt")
	setStretchBltMode             = gdi32.NewProc("SetStretchBltMode")
	stretchDIBits                 = gdi32.NewProc("StretchDIBits")
	bitBlt                        = gdi32.NewProc("BitBlt")
	deleteObject                  = gdi32.NewProc("DeleteObject")
	deleteDC                      = gdi32.NewProc("DeleteDC")
)

const (
	wmDestroy       = 0x0002
	wmSize          = 0x0005
	wmPaint         = 0x000F
	wmClose         = 0x0010
	wmEraseBkgnd    = 0x0014
	wmSetCursor     = 0x0020
	wmMouseMove     = 0x0200
	wmLButtonDown   = 0x0201
	wmLButtonUp     = 0x0202
	wmRButtonDown   = 0x0204
	wmRButtonUp     = 0x0205
	wmMButtonDown   = 0x0207
	wmMButtonUp     = 0x0208
	wmMouseWheel    = 0x020A
	wmEnterSizeMove = 0x0231
	wmExitSizeMove  = 0x0232
	wmFrameReady    = 0x0400 + 1 // WM_USER+1

	wsOverlappedWindow = 0x00CF0000
	cwUseDefault       = 0x80000000 // CW_USEDEFAULT, read as an int
	swShow             = 5
	htClient           = 1
	whKeyboardLL       = 13
	llkhfExtended      = 0x01
	llkhfUp            = 0x80
	sizeRestored       = 0
	sizeMaximized      = 2
	srcCopy            = 0x00CC0020
	blackness          = 0x00000042
	colorOnColor       = 3
)

type rect struct{ Left, Top, Right, Bottom int32 }

type msg struct {
	Hwnd    uintptr
	Message uint32
	_       uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	X, Y    int32
	_       uint32
}

type paintStruct struct {
	Hdc       uintptr
	Erase     int32
	Paint     rect
	Restore   int32
	IncUpdate int32
	_         [32]byte
	_         [4]byte
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type kbdLLHook struct {
	VK, Scan, Flags, Time uint32
	Extra                 uintptr
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// screen is the window's copy of the guest's desktop, and how to reach the
// guest. One window is open in a process at a time.
type screen struct {
	hwnd          atomic.Uintptr
	mu            sync.Mutex
	width, height int
	pixels        []byte
	input         chan [4]uint32
	cursorVisible atomic.Bool
	cursorShape   atomic.Uint32
	inSizeMove    bool
}

var (
	current     atomic.Pointer[screen]
	viewing     sync.Mutex
	registered  sync.Once
	className   *uint16
	wndProcCB   uintptr
	keyHookCB   uintptr
	registerErr error
)

// viewDisplay opens the agent's display stream and shows it in a window
// until the window closes, the stream ends, or ctx is done.
func viewDisplay(ctx context.Context, display func(context.Context) (net.Conn, error), title string) error {
	if !viewing.TryLock() {
		return errors.New("hcs: a window is already open in this process")
	}
	defer viewing.Unlock()
	conn, err := display(ctx)
	if err != nil {
		return fmt.Errorf("hcs: reach the guest's display: %w", err)
	}
	defer conn.Close()
	// The first frame says the desktop is there: the agent closes the stream
	// at once when no one is logged on to the guest's console.
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	typ, payload, err := guest.ReadFrame(conn)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil || typ != guest.DisplayScreen {
		return fmt.Errorf("hcs: the guest shows no desktop (is someone logged on?): %v", errOr(err, "unexpected first frame"))
	}
	size, err := guest.ParseUint32s(payload, 2)
	if err != nil {
		return err
	}
	s := &screen{width: int(size[0]), height: int(size[1]), input: make(chan [4]uint32, 1024)}
	s.pixels = make([]byte, s.width*s.height*4)
	s.cursorVisible.Store(true)

	go func() {
		for in := range s.input {
			if guest.WriteDisplayInput(conn, in[0], in[1], in[2], in[3]) != nil {
				return
			}
		}
	}()
	defer close(s.input)

	windowDone := make(chan struct{})
	streamErr := make(chan error, 1)
	go func() {
		streamErr <- s.receive(conn)
		s.close()
	}()
	go func() {
		select {
		case <-ctx.Done():
			s.close()
		case <-windowDone:
		}
	}()
	err = s.run(title)
	close(windowDone)
	_ = conn.Close()
	if err != nil {
		return err
	}
	select {
	case err := <-streamErr:
		// The window closed because the stream ended, not by hand.
		if err != nil && !errors.Is(err, net.ErrClosed) && ctx.Err() == nil {
			return fmt.Errorf("hcs: the guest's display ended: %w", err)
		}
	default:
	}
	return nil
}

func errOr(err error, otherwise string) any {
	if err != nil {
		return err
	}
	return otherwise
}

// receive applies the stream to the window's copy of the desktop.
func (s *screen) receive(r io.Reader) error {
	for {
		typ, payload, err := guest.ReadFrame(r)
		if err != nil {
			return err
		}
		switch typ {
		case guest.DisplayScreen:
			v, err := guest.ParseUint32s(payload, 2)
			if err != nil {
				return err
			}
			s.mu.Lock()
			s.width, s.height = int(v[0]), int(v[1])
			s.pixels = make([]byte, s.width*s.height*4)
			s.mu.Unlock()
		case guest.DisplayRect:
			v, err := guest.ParseUint32s(payload, 4)
			if err != nil {
				return err
			}
			x, y, w, h := int(v[0]), int(v[1]), int(v[2]), int(v[3])
			data := payload[16:]
			s.mu.Lock()
			if x+w <= s.width && y+h <= s.height && len(data) >= w*h*4 {
				for row := range h {
					copy(s.pixels[((y+row)*s.width+x)*4:], data[row*w*4:(row+1)*w*4])
				}
			}
			s.mu.Unlock()
		case guest.DisplayFlush:
			if hwnd := s.hwnd.Load(); hwnd != 0 {
				_, _, _ = postMessage.Call(hwnd, wmFrameReady, 0, 0)
			}
		case guest.DisplayCursor:
			v, err := guest.ParseUint32s(payload, 2)
			if err != nil {
				return err
			}
			s.cursorVisible.Store(v[0] != 0)
			s.cursorShape.Store(v[1])
		}
	}
}

func (s *screen) send(kind, a, b, c uint32) {
	select {
	case s.input <- [4]uint32{kind, a, b, c}:
	default: // the guest is not keeping up; drop rather than stall the window
	}
}

func (s *screen) close() {
	if hwnd := s.hwnd.Load(); hwnd != 0 {
		_, _, _ = postMessage.Call(hwnd, wmClose, 0, 0)
	}
}

// run creates the window and pumps its messages until it is destroyed, on a
// thread of its own: a window belongs to the thread that made it.
func (s *screen) run(title string) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		done <- s.loop(title)
	}()
	return <-done
}

func (s *screen) loop(title string) error {
	registered.Do(func() {
		// Physical pixels, so the window's coordinates are the frame's.
		_, _, _ = setProcessDpiAwarenessContext.Call(^uintptr(3)) // PER_MONITOR_AWARE_V2
		className, _ = windows.UTF16PtrFromString("disco-vm-display")
		wndProcCB = windows.NewCallback(wndProc)
		keyHookCB = windows.NewCallback(keyHook)
		arrow, _, _ := loadCursor.Call(0, 32512)
		wc := wndClassEx{WndProc: wndProcCB, Cursor: arrow, ClassName: className}
		wc.Size = uint32(unsafe.Sizeof(wc))
		if atom, _, e := registerClassEx.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
			registerErr = fmt.Errorf("hcs: register the window class: %w", e)
		}
	})
	if registerErr != nil {
		return registerErr
	}
	titlep, _ := windows.UTF16PtrFromString(title)
	current.Store(s)
	defer current.Store(nil)
	hwnd, _, e := createWindowEx.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(titlep)),
		wsOverlappedWindow, cwUseDefault, cwUseDefault, 1280, 800, 0, 0, 0, 0)
	if hwnd == 0 {
		return fmt.Errorf("hcs: create the window: %w", e)
	}
	s.hwnd.Store(hwnd)
	defer s.hwnd.Store(0)
	hook, _, _ := setWindowsHookEx.Call(whKeyboardLL, keyHookCB, 0, 0)
	if hook != 0 {
		defer func() { _, _, _ = unhookWindowsHookEx.Call(hook) }()
	}
	_, _, _ = showWindow.Call(hwnd, swShow)
	_, _, _ = updateWindow.Call(hwnd)
	var m msg
	for {
		r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return nil
		}
		_, _, _ = translateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = dispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// keyHook sends every key to the guest while the window is in front, and
// keeps it from the host, so neither host shortcuts fire nor modifiers stick.
func keyHook(code, wParam, lParam uintptr) uintptr {
	if s := current.Load(); int32(code) >= 0 && s != nil && s.hwnd.Load() != 0 {
		if fg, _, _ := getForegroundWindow.Call(); fg == s.hwnd.Load() {
			// lParam is a KBDLLHOOKSTRUCT's address, which Windows owns.
			k := *(**kbdLLHook)(unsafe.Pointer(&lParam))
			var flags uint32
			if k.Flags&llkhfExtended != 0 {
				flags |= guest.KeyExtended
			}
			if k.Flags&llkhfUp != 0 {
				flags |= guest.KeyUp
			}
			s.send(guest.InputKey, k.Scan, flags, 0)
			return 1
		}
	}
	r, _, _ := callNextHookEx.Call(0, code, wParam, lParam)
	return r
}

// layout is where the desktop is drawn in the window: scaled to fit, its
// aspect kept, centered.
func (s *screen) layout(cw, ch int32) (dx, dy, dw, dh int32) {
	if s.width == 0 || s.height == 0 {
		return 0, 0, 0, 0
	}
	scale := min(float64(cw)/float64(s.width), float64(ch)/float64(s.height))
	dw, dh = max(int32(float64(s.width)*scale), 1), max(int32(float64(s.height)*scale), 1)
	return (cw - dw) / 2, (ch - dh) / 2, dw, dh
}

func (s *screen) client() (int32, int32) {
	var r rect
	_, _, _ = getClientRect.Call(s.hwnd.Load(), uintptr(unsafe.Pointer(&r)))
	return max(r.Right-r.Left, 1), max(r.Bottom-r.Top, 1)
}

// toGuest is a point in the window as a pixel of the guest's desktop.
func (s *screen) toGuest(lParam uintptr) (uint32, uint32) {
	cx, cy := int32(int16(lParam&0xFFFF)), int32(int16((lParam>>16)&0xFFFF))
	cw, ch := s.client()
	s.mu.Lock()
	dx, dy, dw, dh := s.layout(cw, ch)
	w, h := int32(s.width), int32(s.height)
	s.mu.Unlock()
	if dw == 0 || dh == 0 {
		return 0, 0
	}
	gx := min(max((cx-dx)*w/dw, 0), w-1)
	gy := min(max((cy-dy)*h/dh, 0), h-1)
	return uint32(gx), uint32(gy)
}

func (s *screen) requestResolution() {
	cw, ch := s.client()
	s.send(guest.InputResolution, uint32(cw), uint32(ch), 0)
}

// standardCursors are the IDC_* resource IDs, by guest cursor shape.
var standardCursors = map[uint32]uintptr{
	guest.CursorArrow: 32512, guest.CursorIBeam: 32513, guest.CursorWait: 32514, guest.CursorCross: 32515,
	guest.CursorUpArrow: 32516, guest.CursorSizeNWSE: 32642, guest.CursorSizeNESW: 32643, guest.CursorSizeWE: 32644,
	guest.CursorSizeNS: 32645, guest.CursorSizeAll: 32646, guest.CursorNo: 32648, guest.CursorHand: 32649,
	guest.CursorAppStarting: 32650, guest.CursorHelp: 32651,
}

func wndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	s := current.Load()
	if s == nil {
		r, _, _ := defWindowProc.Call(hwnd, message, wParam, lParam)
		return r
	}
	switch message {
	case wmPaint:
		s.paint(hwnd)
		return 0
	case wmEraseBkgnd:
		return 1 // painted whole in wmPaint, so it does not flash
	case wmFrameReady:
		_, _, _ = invalidateRect.Call(hwnd, 0, 0)
		return 0
	case wmMouseMove:
		x, y := s.toGuest(lParam)
		s.send(guest.InputMove, x, y, 0)
		return 0
	case wmLButtonDown, wmLButtonUp, wmRButtonDown, wmRButtonUp, wmMButtonDown, wmMButtonUp:
		x, y := s.toGuest(lParam)
		s.send(guest.InputMove, x, y, 0)
		button, down := guest.ButtonLeft, uint32(0)
		switch message {
		case wmRButtonDown, wmRButtonUp:
			button = guest.ButtonRight
		case wmMButtonDown, wmMButtonUp:
			button = guest.ButtonMiddle
		}
		if message == wmLButtonDown || message == wmRButtonDown || message == wmMButtonDown {
			down = 1
		}
		s.send(guest.InputButton, button, down, 0)
		return 0
	case wmMouseWheel:
		s.send(guest.InputWheel, uint32(int32(int16((wParam>>16)&0xFFFF))), 0, 0)
		return 0
	case wmSetCursor:
		if lParam&0xFFFF == htClient {
			var cursor uintptr
			if s.cursorVisible.Load() {
				id, ok := standardCursors[s.cursorShape.Load()]
				if !ok {
					id = 32512
				}
				cursor, _, _ = loadCursor.Call(0, id)
			}
			_, _, _ = setCursor.Call(cursor)
			return 1
		}
	case wmEnterSizeMove:
		s.inSizeMove = true
		return 0
	case wmExitSizeMove:
		s.inSizeMove = false
		s.requestResolution()
		return 0
	case wmSize:
		// Once a drag ends (wmExitSizeMove), or at once for a maximize or a
		// restore that is not part of one.
		if wParam == sizeMaximized || (wParam == sizeRestored && !s.inSizeMove) {
			s.requestResolution()
		}
		return 0
	case wmDestroy:
		_, _, _ = postQuitMessage.Call(0)
		return 0
	}
	r, _, _ := defWindowProc.Call(hwnd, message, wParam, lParam)
	return r
}

// paint draws the desktop scaled into the window, through an off-screen
// bitmap so the bars and the picture appear at once.
func (s *screen) paint(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := beginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	defer func() { _, _, _ = endPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps))) }()
	cw, ch := s.client()
	mem, _, _ := createCompatibleDC.Call(hdc)
	bmp, _, _ := createCompatibleBitmap.Call(hdc, uintptr(cw), uintptr(ch))
	old, _, _ := selectObject.Call(mem, bmp)
	_, _, _ = patBlt.Call(mem, 0, 0, uintptr(cw), uintptr(ch), blackness)
	s.mu.Lock()
	if s.width > 0 && s.height > 0 {
		dx, dy, dw, dh := s.layout(cw, ch)
		info := bitmapInfoHeader{Width: int32(s.width), Height: -int32(s.height), Planes: 1, BitCount: 32}
		info.Size = uint32(unsafe.Sizeof(info))
		_, _, _ = setStretchBltMode.Call(mem, colorOnColor)
		_, _, _ = stretchDIBits.Call(mem, uintptr(dx), uintptr(dy), uintptr(dw), uintptr(dh), 0, 0, uintptr(s.width), uintptr(s.height),
			uintptr(unsafe.Pointer(&s.pixels[0])), uintptr(unsafe.Pointer(&info)), 0, srcCopy)
	}
	s.mu.Unlock()
	_, _, _ = bitBlt.Call(hdc, 0, 0, uintptr(cw), uintptr(ch), mem, 0, 0, srcCopy)
	_, _, _ = selectObject.Call(mem, old)
	_, _, _ = deleteObject.Call(bmp)
	_, _, _ = deleteDC.Call(mem)
}
