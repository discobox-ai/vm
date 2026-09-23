//go:build linux || darwin

package guest

import (
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// listenVsock is AF_VSOCK on the guest's own CID. Linux and macOS guests (13
// and newer) both have it, and x/sys/unix knows the address type on both; what
// neither has is a net.Listener, so the socket is made non-blocking and handed
// to the runtime poller through os.NewFile, as discobox's darwin transport does.
func listenVsock(port uint32) (net.Listener, error) {
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("vsock: socket: %w", err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock: bind port %d: %w", port, err)
	}
	if err := unix.Listen(fd, unix.SOMAXCONN); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock: listen on port %d: %w", port, err)
	}
	file := os.NewFile(uintptr(fd), fmt.Sprintf("vsock-listener:%d", port))
	raw, err := file.SyscallConn()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &vsockListener{file: file, raw: raw, addr: vsockAddr{CID: unix.VMADDR_CID_ANY, Port: port}}, nil
}

type vsockListener struct {
	file *os.File
	raw  syscall.RawConn
	addr vsockAddr
}

func (l *vsockListener) Accept() (net.Conn, error) {
	var (
		nfd int
		sa  unix.Sockaddr
		err error
	)
	if rerr := l.raw.Read(func(fd uintptr) bool {
		syscall.ForkLock.RLock()
		nfd, sa, err = unix.Accept(int(fd))
		if err == nil {
			unix.CloseOnExec(nfd)
		}
		syscall.ForkLock.RUnlock()
		return !errors.Is(err, unix.EAGAIN)
	}); rerr != nil {
		return nil, rerr
	}
	if err != nil {
		return nil, fmt.Errorf("vsock: accept: %w", err)
	}
	if err := unix.SetNonblock(nfd, true); err != nil {
		_ = unix.Close(nfd)
		return nil, err
	}
	remote := vsockAddr{}
	if vm, ok := sa.(*unix.SockaddrVM); ok {
		remote = vsockAddr{CID: vm.CID, Port: vm.Port}
	}
	return &vsockConn{File: os.NewFile(uintptr(nfd), "vsock-conn"), local: l.addr, remote: remote}, nil
}

func (l *vsockListener) Close() error   { return l.file.Close() }
func (l *vsockListener) Addr() net.Addr { return l.addr }

type vsockConn struct {
	*os.File
	local, remote vsockAddr
}

func (c *vsockConn) LocalAddr() net.Addr  { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr { return c.remote }

func (c *vsockConn) SetDeadline(t time.Time) error { return c.File.SetDeadline(t) }

type vsockAddr struct{ CID, Port uint32 }

func (vsockAddr) Network() string  { return "vsock" }
func (a vsockAddr) String() string { return fmt.Sprintf("vsock://%d:%d", a.CID, a.Port) }
