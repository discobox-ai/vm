// Package netutil holds the stream plumbing that disco-vm's HTTP upgrades
// share: a shim's control API (pkg/machine/local) and the served engine
// (pkg/machine/served) both hand a hijacked connection on, and splice one
// into a guest port. They are the shim's, moved here to be shared, not new;
// github.com/discobox-ai/x has no stream package for them yet, and moving
// them there is a change of its own.
package netutil

import (
	"bufio"
	"io"
	"net"
	"sync"
)

// Splice copies both ways and passes each end's EOF on as a half-close, so
// one side can finish sending and still read the answer. It closes both
// once both directions are done. An end that cannot half-close is closed.
func Splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	copyHalf := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go copyHalf(a, b)
	go copyHalf(b, a)
	wg.Wait()
	_ = a.Close()
	_ = b.Close()
}

// BufConn is a hijacked connection whose first bytes may already be
// buffered in R.
type BufConn struct {
	net.Conn
	R *bufio.Reader
}

func (c *BufConn) Read(p []byte) (int, error) { return c.R.Read(p) }

// CloseWrite half-closes the connection underneath, so a splice through it
// passes an EOF on; one that cannot is closed.
func (c *BufConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}
