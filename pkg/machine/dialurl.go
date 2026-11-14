package machine

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

// DialURL connects to the service at a Service URL: the host and port of an
// http:// or https:// URL (the scheme's port where it names none), the Unix
// socket of a unix:///PATH URL, or, on Windows, the named pipe of an
// npipe:////./pipe/NAME URL. The caller speaks HTTP on the connection, and
// TLS first for https.
func DialURL(ctx context.Context, rawURL string) (net.Conn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	switch u.Scheme {
	case "http", "https":
		host := u.Host
		if u.Port() == "" {
			port := "80"
			if u.Scheme == "https" {
				port = "443"
			}
			host = net.JoinHostPort(u.Hostname(), port)
		}
		return d.DialContext(ctx, "tcp", host)
	case "unix":
		if u.Host != "" || u.Path == "" {
			return nil, fmt.Errorf("a unix URL of %q, not unix:///PATH", rawURL)
		}
		return d.DialContext(ctx, "unix", filepath.FromSlash(socketPath(u.Path)))
	case "npipe":
		if u.Host != "" || !strings.HasPrefix(u.Path, "//./pipe/") {
			return nil, fmt.Errorf("a named pipe URL of %q, not npipe:////./pipe/NAME", rawURL)
		}
		return dialPipe(ctx, strings.ReplaceAll(u.Path, "/", `\`))
	default:
		return nil, fmt.Errorf("a service URL of %q, not http, https, unix, or npipe", rawURL)
	}
}

// SocketURL is the unix:// URL of the Unix socket at path, an absolute
// path: unix:///run/s.sock, or on Windows, where a path starts with its
// drive, unix:///C:/dir/s.sock.
func SocketURL(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "unix", Path: p}).String()
}

// PipeURL is the npipe:// URL of the named pipe \\.\pipe\name.
func PipeURL(name string) string { return "npipe:////./pipe/" + name }
