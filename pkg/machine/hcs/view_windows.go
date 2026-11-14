package hcs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// A window on a running VM shows its logged-on desktop, from whichever process
// opens it: the shim for `start --gui`, `disco-vm gui` for one opened after
// the fact. Nothing of the host's but the agent is used: not the VM worker's
// video console, which serves RDP only on some host editions.
//
//   - Where the guest serves Remote Desktop itself (Pro and up, as Install
//     turns it on), mstsc is opened on a loopback port relayed to the guest's
//     own 127.0.0.1:3389 through the agent, signed in as the image's user.
//     The guest's firewall stays closed: nothing reaches it from the network.
//   - Otherwise (Home, or Remote Desktop off) the agent streams the desktop
//     on guest.DisplayPort, and a window of disco-vm's own shows it. A
//     process that attached the machine reaches the stream through the
//     shim's /display, since the machine's Dial reaches the agent's and the
//     service's ports alone (guest.DialablePort).

// agentPath is where Install puts the agent in the guest; `pipe` relays a
// guest address from there.
const agentPath = `C:\disco-vm\disco-vm.exe`

// rdpAddress is the guest's own Remote Desktop listener, as the agent's pipe
// reaches it.
const rdpAddress = "tcp:127.0.0.1:3389"

// View opens a window on the running machine's desktop, titled title, and
// returns once it is closed, or once ctx is done or the machine stops, which
// close it. display opens the guest's display stream.
func (d *Hypervisor) View(ctx context.Context, m machine.Machine, display func(context.Context) (net.Conn, error), title string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-m.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	if d.view != ViewDisplay && servesRDP(ctx, m) {
		return viewRDP(ctx, m, title)
	}
	return viewDisplay(ctx, display, title)
}

// watch keeps a window open on a machine the shim booted with GUI: opened
// once the desktop can be shown, and again after the guest restarts, until
// someone closes it or the machine stops. The shim holds the machine, so it
// dials the display itself.
func (d *Hypervisor) watch(m machine.Machine, title string, log io.Writer) {
	keepOpen(context.Background(), m, log, func(ctx context.Context) error {
		return d.View(ctx, m, dialDisplay(m), title)
	})
}

// dialDisplay opens the display stream of a machine this process holds.
func dialDisplay(m machine.Machine) func(context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) { return m.Dial(ctx, guest.DisplayPort) }
}

// keepOpen keeps a window that open shows on m: open again when it fails (no
// desktop yet, or the guest restarting under it), until someone closes it by
// hand, ctx is done, or the machine stops. A failure is logged once until a
// different one follows it.
func keepOpen(ctx context.Context, m machine.Machine, log io.Writer, open func(context.Context) error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-m.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	last := ""
	for ctx.Err() == nil {
		err := open(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		if msg := err.Error(); log != nil && msg != last {
			fmt.Fprintf(log, "hcs: window: %v (trying again)\n", err)
			last = msg
		}
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
		}
	}
}

// pipe starts the agent relaying a guest address, and joins it to conn
// until either ends.
func pipe(ctx context.Context, g machine.Guest, address string, conn io.ReadWriteCloser) error {
	p, err := g.Exec(ctx, machine.ExecRequest{Argv: []string{agentPath, "pipe", address}})
	if err != nil {
		return err
	}
	defer p.Close()
	go func() {
		_, _ = io.Copy(p, conn)
		_ = p.CloseStdin()
	}()
	_, err = p.Wait(&afterReady{w: conn}, io.Discard)
	_ = conn.Close()
	return err
}

// afterReady writes what follows the guest.PipeReady byte that `pipe` sends
// once it has connected.
type afterReady struct {
	w     io.Writer
	ready bool
}

func (a *afterReady) Write(p []byte) (int, error) {
	n := len(p)
	if !a.ready && len(p) > 0 {
		if p[0] != guest.PipeReady {
			return 0, fmt.Errorf("hcs: the guest's pipe sent %#x before its ready byte", p[0])
		}
		a.ready, p = true, p[1:]
	}
	if len(p) == 0 {
		return n, nil
	}
	if _, err := a.w.Write(p); err != nil {
		return 0, err
	}
	return n, nil
}

// servesRDP reports whether the guest's own Remote Desktop answers an RDP
// connection request (X.224, offering TLS and CredSSP) inside the guest.
func servesRDP(ctx context.Context, g machine.Guest) bool {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	local, remote := net.Pipe()
	defer local.Close()
	go func() { _ = pipe(ctx, g, rdpAddress, remote) }()
	_ = local.SetDeadline(time.Now().Add(10 * time.Second))
	// TPKT(4) + X.224 CR(7) + RDP_NEG_REQ(8) requesting PROTOCOL_SSL|PROTOCOL_HYBRID.
	request := []byte{3, 0, 0, 19, 14, 0xE0, 0, 0, 0, 0, 0, 1, 0, 8, 0, 3, 0, 0, 0}
	if _, err := local.Write(request); err != nil {
		return false
	}
	reply := make([]byte, 4)
	_, err := io.ReadFull(local, reply)
	return err == nil && reply[0] == 3
}

// autologonUser is the account the guest logs on automatically, which the
// image's install made; defaultUser if the guest does not say.
func autologonUser(ctx context.Context, g machine.Guest) string {
	var out bytes.Buffer
	code, err := machine.Run(ctx, g, machine.ExecRequest{Argv: []string{"reg", "query",
		`HKLM\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon`, "/v", "DefaultUserName"}}, nil, &out, io.Discard)
	if err == nil && code == 0 {
		scanner := bufio.NewScanner(&out)
		for scanner.Scan() {
			if fields := strings.Fields(scanner.Text()); len(fields) == 3 && fields[0] == "DefaultUserName" {
				return fields[2]
			}
		}
	}
	return defaultUser
}

// viewRDP opens mstsc on the guest's Remote Desktop, through a loopback port
// that relays every connection mstsc makes, signed in as the image's user
// with its name for a password, as Install sets it.
func viewRDP(ctx context.Context, g machine.Guest, title string) error {
	user := autologonUser(ctx, g)
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { _ = pipe(ctx, g, rdpAddress, conn) }()
		}
	}()
	// The guest's own account, named by its computer name: a bare name, as
	// mstsc completes it, would be the host's account of that name.
	account := user
	if info, err := g.Info(ctx); err == nil && info.Hostname != "" {
		account = info.Hostname + `\` + user
	}
	// mstsc takes a password from the credential manager, not from the
	// file: one for the loopback address, for as long as mstsc is open.
	forget, err := rememberRDPCredential("127.0.0.1", account, user)
	if err != nil {
		return err
	}
	defer forget()

	dir, err := os.MkdirTemp("", "disco-vm-rdp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// mstsc titles its window after the file.
	rdp := filepath.Join(dir, safeTitle(title)+".rdp")
	width, height := hostScreen()
	// The guest's certificate is its own self-signed one, and the stream is
	// the agent's, so mstsc is told not to check it (authentication level 0).
	// Full screen at the host's own resolution, as mstsc's Default.rdp has
	// it, which lets mstsc pick the scale; dynamic resolution resizes the
	// guest's desktop with the window once it is one, where smart sizing
	// would scale the picture instead.
	body := strings.Join([]string{
		"full address:s:" + listener.Addr().String(),
		"username:s:" + account,
		"prompt for credentials:i:0",
		"promptcredentialonce:i:0",
		"authentication level:i:0",
		"enablecredsspsupport:i:1",
		"negotiate security layer:i:1",
		"screen mode id:i:2",
		fmt.Sprintf("desktopwidth:i:%d", width),
		fmt.Sprintf("desktopheight:i:%d", height),
		"dynamic resolution:i:1",
		"smart sizing:i:0",
		"autoreconnection enabled:i:1",
	}, "\r\n")
	if err := os.WriteFile(rdp, []byte(body), 0o600); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "mstsc.exe", rdp)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("hcs: open mstsc: %w", err)
	}
	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		// mstsc's exit code says nothing of note; only failing to wait does.
		if _, exited := errors.AsType[*exec.ExitError](err); !exited {
			return err
		}
	}
	return nil
}

var (
	advapi32         = windows.NewLazySystemDLL("advapi32.dll")
	credWrite        = advapi32.NewProc("CredWriteW")
	credDelete       = advapi32.NewProc("CredDeleteW")
	credRead         = advapi32.NewProc("CredReadW")
	credFree         = advapi32.NewProc("CredFree")
	getSystemMetrics = user32.NewProc("GetSystemMetrics")
)

// credential is CREDENTIALW.
type credential struct {
	Flags          uint32
	Type           uint32
	TargetName     *uint16
	Comment        *uint16
	LastWritten    windows.Filetime
	BlobSize       uint32
	Blob           *byte
	Persist        uint32
	AttributeCount uint32
	Attributes     uintptr
	TargetAlias    *uint16
	UserName       *uint16
}

// rememberRDPCredential stores user (COMPUTER\name) and password for Remote
// Desktop to host in this logon session's credential manager, as mstsc's own
// "Remember me" does, and returns what forgets it again. One already stored
// for host is someone's own, and is left as it is.
func rememberRDPCredential(host, user, password string) (func(), error) {
	const credTypeDomainPassword, credPersistSession = 2, 1
	target, _ := windows.UTF16PtrFromString("TERMSRV/" + host)
	var existing uintptr
	if ok, _, _ := credRead.Call(uintptr(unsafe.Pointer(target)), credTypeDomainPassword, 0, uintptr(unsafe.Pointer(&existing))); ok != 0 {
		_, _, _ = credFree.Call(existing)
		return func() {}, nil
	}
	userp, _ := windows.UTF16PtrFromString(user)
	secret := windows.StringToUTF16(password)
	secret = secret[:len(secret)-1] // without its NUL
	c := credential{Type: credTypeDomainPassword, TargetName: target, UserName: userp, Persist: credPersistSession, BlobSize: uint32(len(secret) * 2)}
	if len(secret) > 0 {
		c.Blob = (*byte)(unsafe.Pointer(&secret[0]))
	}
	if ok, _, e := credWrite.Call(uintptr(unsafe.Pointer(&c)), 0); ok == 0 {
		return nil, fmt.Errorf("hcs: store the guest's sign-in for mstsc: %w", e)
	}
	return func() { _, _, _ = credDelete.Call(uintptr(unsafe.Pointer(target)), credTypeDomainPassword, 0) }, nil
}

// hostScreen is the host's primary screen in physical pixels.
func hostScreen() (int, int) {
	_, _, _ = setProcessDpiAwarenessContext.Call(^uintptr(3)) // PER_MONITOR_AWARE_V2: the real size
	const smCXScreen, smCYScreen = 0, 1
	w, _, _ := getSystemMetrics.Call(smCXScreen)
	h, _, _ := getSystemMetrics.Call(smCYScreen)
	return int(int32(w)), int(int32(h))
}

// safeTitle is title with only what a file name may hold.
func safeTitle(title string) string {
	return strings.Map(func(r rune) rune {
		if r < 128 && (r == ' ' || r == '-' || r == '_' || r == '.' || r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			return r
		}
		return '_'
	}, title)
}
