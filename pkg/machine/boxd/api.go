package boxd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

const (
	// APIKeyEnv is a boxd API key (bxd_...), from `boxd auth keys create`.
	// BOXD_TOKEN, which the boxd CLI reads, is accepted too, and may hold a
	// key or an already-exchanged JWT.
	APIKeyEnv = "BOXD_API_KEY" //nolint:gosec // G101: an environment variable's name, not a credential
	tokenEnv  = "BOXD_TOKEN"
	// AddrEnv and TokenURLEnv point the driver at another boxd cluster.
	AddrEnv     = "BOXD_GRPC_ADDR"
	TokenURLEnv = "BOXD_TOKEN_URL" //nolint:gosec // G101: an environment variable's name, not a credential

	defaultAddr     = "boxd.sh:9443"
	defaultTokenURL = "https://app.boxd.sh/api/v1/auth/token" //nolint:gosec // G101: an endpoint URL, not a credential
)

// errNoCredentials names the fix, since Check is where a user meets it first.
var errNoCredentials = fmt.Errorf("boxd: no credentials: set %s to an API key from `boxd auth keys create disco-vm`", APIKeyEnv)

// client is a lazily dialed connection to the boxd API. Construction touches
// nothing, so `disco-vm info` can list the driver on a host with no account.
type client struct {
	addr     string
	tokenURL string
	secret   string

	once sync.Once
	conn *grpc.ClientConn
	api  boxdapi.BoxdApiClient
	err  error

	mu      sync.Mutex
	jwt     string
	expires time.Time
}

func newClient() *client {
	secret := os.Getenv(APIKeyEnv)
	if secret == "" {
		secret = os.Getenv(tokenEnv)
	}
	return &client{
		addr:     envOr(AddrEnv, defaultAddr),
		tokenURL: envOr(TokenURLEnv, defaultTokenURL),
		secret:   strings.TrimSpace(secret),
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// API returns the generated client, dialing on first use.
func (c *client) API() (boxdapi.BoxdApiClient, error) {
	c.once.Do(func() {
		if c.secret == "" {
			c.err = errNoCredentials
			return
		}
		opts := []grpc.DialOption{
			grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})),
			grpc.WithPerRPCCredentials(c),
		}
		dialer, err := socksDialer()
		if err != nil {
			c.err = err
			return
		}
		target := c.addr
		if dialer != nil {
			// passthrough hands the proxy the host name, as socks5h means:
			// gRPC's own resolver would look it up here first.
			opts = append(opts, grpc.WithContextDialer(dialer))
			target = "passthrough:///" + c.addr
		}
		c.conn, c.err = grpc.NewClient(target, opts...)
		if c.err == nil {
			c.api = boxdapi.NewBoxdApiClient(c.conn)
		}
	})
	return c.api, c.err
}

// GetRequestMetadata makes the client its own per-RPC credentials: every call
// carries a JWT, exchanged from the API key and renewed before it expires.
func (c *client) GetRequestMetadata(ctx context.Context, _ ...string) (map[string]string, error) {
	token, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]string{"authorization": "Bearer " + token}, nil
}

func (*client) RequireTransportSecurity() bool { return true }

// token is the bearer for the next call. An API key (bxd_...) is exchanged for
// a JWT that lives an hour; anything else is taken to be a JWT already.
func (c *client) token(ctx context.Context) (string, error) {
	if !strings.HasPrefix(c.secret, "bxd_") {
		return c.secret, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jwt != "" && time.Until(c.expires) > 5*time.Minute {
		return c.jwt, nil
	}
	body, err := json.Marshal(map[string]string{"api_key": c.secret})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("boxd: exchange API key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("boxd: exchange API key: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("boxd: exchange API key: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("boxd: exchange API key: no token in the answer")
	}
	c.jwt, c.expires = out.Token, time.Unix(out.ExpiresAt, 0)
	return c.jwt, nil
}

// socksDialer honors a SOCKS5 ALL_PROXY (socks5:// or socks5h://), as curl
// does. Without one, gRPC takes HTTPS_PROXY by itself, through HTTP CONNECT. A
// proxy that intercepts TLS and does not offer HTTP/2 (ALPN h2) breaks gRPC
// there, while SOCKS passes the stream through untouched.
func socksDialer() (func(context.Context, string) (net.Conn, error), error) {
	raw := os.Getenv("ALL_PROXY")
	if raw == "" {
		raw = os.Getenv("all_proxy")
	}
	u, err := url.Parse(raw)
	if raw == "" || err != nil || (u.Scheme != "socks5" && u.Scheme != "socks5h") {
		return nil, nil //nolint:nilerr // an ALL_PROXY that is not a SOCKS URL is not ours to honor; gRPC goes direct or through HTTPS_PROXY
	}
	var auth *proxy.Auth
	if u.User != nil {
		password, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: password}
	}
	socks, err := proxy.SOCKS5("tcp", u.Host, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("boxd: ALL_PROXY: %w", err)
	}
	dial, ok := socks.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("boxd: ALL_PROXY: the SOCKS5 dialer cannot dial with a context")
	}
	return func(ctx context.Context, addr string) (net.Conn, error) {
		return dial.DialContext(ctx, "tcp", addr)
	}, nil
}

func isNotFound(err error) bool { return status.Code(err) == codes.NotFound }
