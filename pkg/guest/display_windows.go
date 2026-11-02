package guest

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	getDC                         = user32.NewProc("GetDC")
	releaseDC                     = user32.NewProc("ReleaseDC")
	getSystemMetrics              = user32.NewProc("GetSystemMetrics")
	enumDisplaySettings           = user32.NewProc("EnumDisplaySettingsW")
	changeDisplaySettingsEx       = user32.NewProc("ChangeDisplaySettingsExW")
	sendInput                     = user32.NewProc("SendInput")
	getCursorInfo                 = user32.NewProc("GetCursorInfo")
	loadCursor                    = user32.NewProc("LoadCursorW")
	setProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	createCompatibleDC            = gdi32.NewProc("CreateCompatibleDC")
	createCompatibleBitmap        = gdi32.NewProc("CreateCompatibleBitmap")
	selectObject                  = gdi32.NewProc("SelectObject")
	bitBlt                        = gdi32.NewProc("BitBlt")
	getDIBits                     = gdi32.NewProc("GetDIBits")
	deleteObject                  = gdi32.NewProc("DeleteObject")
	deleteDC                      = gdi32.NewProc("DeleteDC")
	wtsGetActiveConsoleSessionID  = kernel32.NewProc("WTSGetActiveConsoleSessionId")
	wtsConnectSession             = windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSConnectSessionW")
	dpiAwarenessPerMonitorV2      = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2, (HANDLE)-4
	errNoConsoleUser              = errors.New("guest: no one is logged on to the console, so there is no desktop to show")
	displayHostMu                 sync.Mutex
	displayHostCurrent            *os.Process
)

// ServeDisplay serves DisplayPort until the listener fails: each connection
// is a viewer, shown the desktop of whoever is logged on to the console by a
// display host (`disco-vm display-host`) the agent starts in their session.
// The agent is a SYSTEM service in session 0, which has no desktop to capture
// and no input to inject into. One viewer is served at a time: a new one
// takes over from the last.
func ServeDisplay() error {
	listener, err := listenVsock(DisplayPort)
	if err != nil {
		return err
	}
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go serveViewer(conn)
	}
}

func serveViewer(conn net.Conn) {
	defer conn.Close()
	host, stdin, stdout, err := startDisplayHost()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s agent: display: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}
	displayHostMu.Lock()
	if displayHostCurrent != nil {
		_ = displayHostCurrent.Kill()
	}
	displayHostCurrent = host
	displayHostMu.Unlock()
	defer func() {
		displayHostMu.Lock()
		if displayHostCurrent == host {
			displayHostCurrent = nil
		}
		displayHostMu.Unlock()
		_ = host.Kill()
		_, _ = host.Wait()
	}()
	go func() {
		_, _ = io.Copy(stdin, conn)
		_ = stdin.Close()
	}()
	_, _ = io.Copy(conn, stdout)
	_ = stdout.Close()
}

// startDisplayHost starts `disco-vm display-host` on the console session's
// desktop as the user logged on to it, elevated when they are an
// administrator so its input reaches elevated windows too, with its stdin
// and stdout the returned pipes.
func startDisplayHost() (*os.Process, *os.File, *os.File, error) {
	session, _, _ := wtsGetActiveConsoleSessionID.Call()
	if uint32(session) == 0xFFFFFFFF {
		return nil, nil, nil, errNoConsoleUser
	}
	var token windows.Token
	if err := windows.WTSQueryUserToken(uint32(session), &token); err != nil {
		// After a Remote Desktop connection the user's session is
		// disconnected and the console is at the sign-in screen: bring the
		// session back to the console, as `tscon ID /dest:console` does.
		if !reconnectToConsole(uint32(session)) {
			return nil, nil, nil, errNoConsoleUser
		}
		session, _, _ = wtsGetActiveConsoleSessionID.Call()
		if err := windows.WTSQueryUserToken(uint32(session), &token); err != nil {
			return nil, nil, nil, errNoConsoleUser
		}
	}
	if linked, err := token.GetLinkedToken(); err == nil {
		var primary windows.Token
		if windows.DuplicateTokenEx(linked, windows.MAXIMUM_ALLOWED, nil, windows.SecurityImpersonation, windows.TokenPrimary, &primary) == nil {
			token.Close()
			token = primary
		}
		linked.Close()
	}
	defer token.Close()
	var block *uint16
	if err := windows.CreateEnvironmentBlock(&block, token, false); err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = windows.DestroyEnvironmentBlock(block) }()

	self, err := os.Executable()
	if err != nil {
		return nil, nil, nil, err
	}
	// The child's ends are inheritable, and only they are inherited: the
	// handle list keeps every other inheritable handle in the agent out.
	inherit := &windows.SecurityAttributes{InheritHandle: 1}
	inherit.Length = uint32(unsafe.Sizeof(*inherit))
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, inherit, 0); err != nil {
		return nil, nil, nil, err
	}
	if err := windows.CreatePipe(&outRead, &outWrite, inherit, 0); err != nil {
		_ = windows.CloseHandle(inRead)
		_ = windows.CloseHandle(inWrite)
		return nil, nil, nil, err
	}
	_ = windows.SetHandleInformation(inWrite, windows.HANDLE_FLAG_INHERIT, 0)
	_ = windows.SetHandleInformation(outRead, windows.HANDLE_FLAG_INHERIT, 0)
	closeAll := func() {
		for _, h := range []windows.Handle{inRead, inWrite, outRead, outWrite} {
			_ = windows.CloseHandle(h)
		}
	}
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		closeAll()
		return nil, nil, nil, err
	}
	defer attrs.Delete()
	handles := []windows.Handle{inRead, outWrite}
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		closeAll()
		return nil, nil, nil, err
	}
	desktop, _ := windows.UTF16PtrFromString(`winsta0\default`)
	si := &windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Desktop = desktop
	si.Flags = windows.STARTF_USESTDHANDLES
	// No stderr: its stdout is the protocol, which a stray message would
	// corrupt.
	si.StdInput, si.StdOutput = inRead, outWrite
	app, _ := windows.UTF16PtrFromString(self)
	cmdline, _ := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{self, "display-host"}))
	var pi windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NO_WINDOW)
	err = windows.CreateProcessAsUser(token, app, cmdline, nil, nil, true, flags, block, nil, &si.StartupInfo, &pi)
	_ = windows.CloseHandle(inRead)
	_ = windows.CloseHandle(outWrite)
	if err != nil {
		_ = windows.CloseHandle(inWrite)
		_ = windows.CloseHandle(outRead)
		return nil, nil, nil, fmt.Errorf("guest: start the display host in session %d: %w", session, err)
	}
	_ = windows.CloseHandle(pi.Thread)
	process, err := os.FindProcess(int(pi.ProcessId))
	_ = windows.CloseHandle(pi.Process)
	if err != nil {
		_ = windows.CloseHandle(inWrite)
		_ = windows.CloseHandle(outRead)
		return nil, nil, nil, err
	}
	return process, os.NewFile(uintptr(inWrite), "display-in"), os.NewFile(uintptr(outRead), "display-out"), nil
}

// reconnectToConsole connects the first disconnected session a user is
// logged on to to the console session, and reports whether it did.
func reconnectToConsole(console uint32) bool {
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count); err != nil {
		return false
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))
	const wtsDisconnected = 4
	for _, s := range unsafe.Slice(sessions, count) {
		if s.State != wtsDisconnected || s.SessionID == 0 {
			continue
		}
		var token windows.Token
		if windows.WTSQueryUserToken(s.SessionID, &token) != nil {
			continue
		}
		token.Close()
		empty, _ := windows.UTF16PtrFromString("")
		if ok, _, _ := wtsConnectSession.Call(uintptr(s.SessionID), uintptr(console), uintptr(unsafe.Pointer(empty)), 1); ok != 0 {
			return true
		}
	}
	return false
}

// DisplayHost is `disco-vm display-host`: it captures the desktop it runs on
// and writes it to out, and injects the input it reads from in, until either
// ends. It runs in the session of the user whose desktop it shows.
func DisplayHost(in io.Reader, out io.Writer) error {
	// Physical pixels, so the capture is the whole desktop at any scaling and
	// the pointer lands where the viewer put it.
	_, _, _ = setProcessDpiAwarenessContext.Call(dpiAwarenessPerMonitorV2)
	d := &display{out: out, screenSent: make(chan struct{})}
	errs := make(chan error, 3)
	go func() { errs <- d.capture(33 * time.Millisecond) }()
	go func() {
		// DisplayScreen comes first; the viewer sizes its copy by it.
		<-d.screenSent
		errs <- d.cursor(16 * time.Millisecond)
	}()
	go func() { errs <- d.input(in) }()
	return <-errs
}

type display struct {
	mu         sync.Mutex
	out        io.Writer
	screenSent chan struct{}
	sentOnce   sync.Once
}

func (d *display) send(typ byte, payload []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return WriteFrame(d.out, typ, payload)
}

func screenSize() (int, int) {
	const smCXScreen, smCYScreen = 0, 1
	w, _, _ := getSystemMetrics.Call(smCXScreen)
	h, _, _ := getSystemMetrics.Call(smCYScreen)
	return int(int32(w)), int(int32(h))
}

// capture grabs the desktop every interval and sends what changed.
func (d *display) capture(interval time.Duration) error {
	var prev, cur []byte
	width, height := 0, 0
	for {
		w, h := screenSize()
		if w <= 0 || h <= 0 {
			time.Sleep(interval)
			continue
		}
		if w != width || h != height {
			width, height = w, h
			prev, cur = nil, make([]byte, w*h*4)
			if err := d.send(DisplayScreen, uint32s(uint32(w), uint32(h))); err != nil {
				return err
			}
			d.sentOnce.Do(func() { close(d.screenSent) })
		}
		if !grab(width, height, cur) {
			time.Sleep(interval)
			continue
		}
		var rects []DisplayRectangle
		if prev == nil {
			prev = make([]byte, len(cur))
			rects = []DisplayRectangle{{0, 0, width, height}}
		} else {
			rects = changedRows(prev, cur, width, height)
		}
		if len(rects) > 0 {
			d.mu.Lock()
			err := writeDisplayRects(d.out, width, cur, rects)
			if err == nil {
				err = WriteFrame(d.out, DisplayFlush, nil)
			}
			d.mu.Unlock()
			if err != nil {
				return err
			}
		}
		prev, cur = cur, prev
		time.Sleep(interval)
	}
}

// grab copies the desktop into buf as top-down BGRA with GDI, not Desktop
// Duplication: sandboxi chose BitBlt because it works on every edition and
// survives GPU partitioning, where DXGI loses its keyed mutex. It fails while
// the desktop cannot be read, such as on the secure desktop of a UAC prompt.
func grab(width, height int, buf []byte) bool {
	screen, _, _ := getDC.Call(0)
	if screen == 0 {
		return false
	}
	defer func() { _, _, _ = releaseDC.Call(0, screen) }()
	mem, _, _ := createCompatibleDC.Call(screen)
	if mem == 0 {
		return false
	}
	defer func() { _, _, _ = deleteDC.Call(mem) }()
	bmp, _, _ := createCompatibleBitmap.Call(screen, uintptr(width), uintptr(height))
	if bmp == 0 {
		return false
	}
	defer func() { _, _, _ = deleteObject.Call(bmp) }()
	old, _, _ := selectObject.Call(mem, bmp)
	const srcCopy = 0x00CC0020
	ok, _, _ := bitBlt.Call(mem, 0, 0, uintptr(width), uintptr(height), screen, 0, 0, srcCopy)
	_, _, _ = selectObject.Call(mem, old)
	if ok == 0 {
		return false
	}
	info := bitmapInfoHeader{Width: int32(width), Height: -int32(height), Planes: 1, BitCount: 32}
	info.Size = uint32(unsafe.Sizeof(info))
	const dibRGBColors = 0
	lines, _, _ := getDIBits.Call(mem, bmp, 0, uintptr(height), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&info)), dibRGBColors)
	return int(lines) == height
}

// bitmapInfoHeader is BITMAPINFOHEADER; a 32-bit BI_RGB bitmap needs no
// color table after it.
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

// cursorShapes are the standard cursors' resource IDs (IDC_*), by shape.
var cursorShapes = map[uint32]uintptr{
	CursorArrow: 32512, CursorIBeam: 32513, CursorWait: 32514, CursorCross: 32515, CursorUpArrow: 32516,
	CursorSizeNWSE: 32642, CursorSizeNESW: 32643, CursorSizeWE: 32644, CursorSizeNS: 32645, CursorSizeAll: 32646,
	CursorNo: 32648, CursorHand: 32649, CursorAppStarting: 32650, CursorHelp: 32651,
}

// cursor sends the shape and visibility of the system cursor when they
// change, as the standard cursor it is, which the viewer draws itself.
func (d *display) cursor(interval time.Duration) error {
	byHandle := map[uintptr]uint32{}
	for shape, id := range cursorShapes {
		if h, _, _ := loadCursor.Call(0, id); h != 0 {
			byHandle[h] = shape
		}
	}
	lastVisible, lastShape := uint32(2), uint32(0)
	for {
		info := cursorInfo{}
		info.Size = uint32(unsafe.Sizeof(info))
		visible, shape := uint32(1), CursorArrow
		if ok, _, _ := getCursorInfo.Call(uintptr(unsafe.Pointer(&info))); ok != 0 {
			const cursorShowing = 1
			visible = info.Flags & cursorShowing
			if s, known := byHandle[info.Cursor]; known {
				shape = s
			} else {
				shape = CursorOther
			}
		}
		if visible != lastVisible || shape != lastShape {
			lastVisible, lastShape = visible, shape
			if err := d.send(DisplayCursor, uint32s(visible, shape)); err != nil {
				return err
			}
		}
		time.Sleep(interval)
	}
}

type cursorInfo struct {
	Size   uint32
	Flags  uint32
	Cursor uintptr
	X, Y   int32
}

// input applies the viewer's input to the desktop until in ends.
func (d *display) input(in io.Reader) error {
	releaseModifiers()
	for {
		typ, payload, err := ReadFrame(in)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if typ != DisplayInput {
			continue
		}
		v, err := ParseUint32s(payload, 4)
		if err != nil {
			return err
		}
		applyInput(v[0], v[1], v[2], v[3])
	}
}

// mouseInput and keyInput are INPUT, of type INPUT_MOUSE and INPUT_KEYBOARD,
// as 64-bit Windows lays it out: 40 bytes either way.
type mouseInput struct {
	Type  uint32
	_     uint32
	DX    int32
	DY    int32
	Data  uint32
	Flags uint32
	Time  uint32
	_     uint32
	Extra uintptr
}

type keyInput struct {
	Type  uint32
	_     uint32
	VK    uint16
	Scan  uint16
	Flags uint32
	Time  uint32
	_     uint32
	Extra uintptr
	_     [8]byte
}

const (
	inputMouse, inputKeyboard = 0, 1

	mouseMove, mouseAbsolute          = 0x0001, 0x8000
	mouseLeftDown, mouseLeftUp        = 0x0002, 0x0004
	mouseRightDown, mouseRightUp      = 0x0008, 0x0010
	mouseMiddleDown, mouseMiddleUp    = 0x0020, 0x0040
	mouseWheel                        = 0x0800
	keyExtendedKey, keyKeyUp, keyScan = 0x0001, 0x0002, 0x0008
)

func sendMouse(flags uint32, dx, dy int32, data uint32) {
	in := mouseInput{Type: inputMouse, DX: dx, DY: dy, Data: data, Flags: flags}
	_, _, _ = sendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

func sendKey(vk, scan uint16, flags uint32) {
	in := keyInput{Type: inputKeyboard, VK: vk, Scan: scan, Flags: flags}
	_, _, _ = sendInput.Call(1, uintptr(unsafe.Pointer(&in)), unsafe.Sizeof(in))
}

// releaseModifiers lets go of every modifier, which a viewer that went away
// mid-combination may have left held down.
func releaseModifiers() {
	for _, vk := range []uint16{0x10, 0x11, 0x12, 0xA0, 0xA1, 0xA2, 0xA3, 0xA4, 0xA5, 0x5B, 0x5C} {
		sendKey(vk, 0, keyKeyUp)
	}
}

func applyInput(kind, a, b, _ uint32) {
	switch kind {
	case InputMove:
		// The size now, not when the viewer came: the desktop resizes.
		w, h := screenSize()
		x := min(max(int64(a)*65535/int64(max(w, 1)), 0), 65535)
		y := min(max(int64(b)*65535/int64(max(h, 1)), 0), 65535)
		sendMouse(mouseMove|mouseAbsolute, int32(x), int32(y), 0)
	case InputButton:
		flags := map[[2]uint32]uint32{
			{ButtonLeft, 1}: mouseLeftDown, {ButtonLeft, 0}: mouseLeftUp,
			{ButtonRight, 1}: mouseRightDown, {ButtonRight, 0}: mouseRightUp,
			{ButtonMiddle, 1}: mouseMiddleDown, {ButtonMiddle, 0}: mouseMiddleUp,
		}[[2]uint32{a, min(b, 1)}]
		if flags != 0 {
			sendMouse(flags, 0, 0, 0)
		}
	case InputWheel:
		sendMouse(mouseWheel, 0, 0, a)
	case InputKey:
		flags := uint32(keyScan)
		if b&KeyExtended != 0 {
			flags |= keyExtendedKey
		}
		if b&KeyUp != 0 {
			flags |= keyKeyUp
		}
		sendKey(0, uint16(a), flags)
	case InputResolution:
		setResolution(int(a), int(b))
	}
}

// devMode is DEVMODEW, display fields only named.
type devMode struct {
	DeviceName       [32]uint16
	SpecVersion      uint16
	DriverVersion    uint16
	Size             uint16
	DriverExtra      uint16
	Fields           uint32
	Position         [16]byte
	Color            int16
	Duplex           int16
	YResolution      int16
	TTOption         int16
	Collate          int16
	FormName         [32]uint16
	LogPixels        uint16
	BitsPerPel       uint32
	PelsWidth        uint32
	PelsHeight       uint32
	DisplayFlags     uint32
	DisplayFrequency uint32
	_                [8]uint32
}

// setResolution sets the desktop to the supported mode nearest the size the
// viewer asked for: the Hyper-V video adapter takes only modes from its list.
func setResolution(width, height int) {
	var best devMode
	bestDistance := int64(-1)
	for i := uintptr(0); ; i++ {
		mode := devMode{}
		mode.Size = uint16(unsafe.Sizeof(mode))
		if ok, _, _ := enumDisplaySettings.Call(0, i, uintptr(unsafe.Pointer(&mode))); ok == 0 {
			break
		}
		if mode.BitsPerPel < 32 {
			continue
		}
		dw, dh := int64(mode.PelsWidth)-int64(width), int64(mode.PelsHeight)-int64(height)
		if distance := dw*dw + dh*dh; bestDistance < 0 || distance < bestDistance {
			best, bestDistance = mode, distance
		}
	}
	if bestDistance < 0 {
		return
	}
	const dmBitsPerPel, dmPelsWidth, dmPelsHeight = 0x00040000, 0x00080000, 0x00100000
	mode := devMode{BitsPerPel: 32, PelsWidth: best.PelsWidth, PelsHeight: best.PelsHeight, Fields: dmBitsPerPel | dmPelsWidth | dmPelsHeight}
	mode.Size = uint16(unsafe.Sizeof(mode))
	_, _, _ = changeDisplaySettingsEx.Call(0, uintptr(unsafe.Pointer(&mode)), 0, 0, 0)
}
