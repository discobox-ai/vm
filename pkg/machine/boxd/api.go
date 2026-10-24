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
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

const (
	// APIKeyEnv is a boxd API key (bxd_...), from `boxd auth keys create`.
	// BOXD_TOKEN, which the boxd CLI reads, is accepted too, and may hold a
	// key or an already-exchanged JWT.
	APIKeyEnv = "BOXD_API_KEY" //nolint:gosec // G101: an environment variable's name, not a credential
	TokenEnv  = "BOXD_TOKEN"
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
	// keys is asked for the key on every call, so a key replaced where it
	// reads is used from the next one.
	keys machine.KeySource

	once sync.Once
	conn *grpc.ClientConn
	api  boxdapi.BoxdApiClient
	err  error

	mu sync.Mutex
	// jwt is exchanged from the API key jwtFor, and lives until expires.
	jwt     string
	jwtFor  string
	expires time.Time
}

func newClient(keys machine.KeySource) *client {
	return &client{
		addr:     envOr(AddrEnv, defaultAddr),
		tokenURL: envOr(TokenURLEnv, defaultTokenURL),
		keys:     keys,
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
		opts := []grpc.DialOption{
			grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})),
		}
		// Debugging first, so a call its key refuses is reported too.
		opts = append(opts, debugOptions()...)
		opts = append(opts, c.authOptions()...)
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

// authOptions have every call carry a JWT, exchanged from the API key and
// renewed before it expires. They are interceptors, not per-RPC credentials,
// whose errors gRPC flattens to a status: a key that is missing or refused
// reaches the caller as errNoCredentials or machine.ErrUnauthorized, and so
// does boxd's Unauthenticated answer to a call.
func (c *client) authOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			ctx, err := c.authorize(ctx)
			if err != nil {
				return err
			}
			return c.refused(invoker(ctx, method, req, reply, cc, opts...))
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			ctx, err := c.authorize(ctx)
			if err != nil {
				return nil, err
			}
			stream, err := streamer(ctx, desc, cc, method, opts...)
			if err != nil {
				return nil, c.refused(err)
			}
			return &refusedStream{ClientStream: stream, c: c}, nil
		}),
	}
}

// authorize is ctx carrying the bearer of the key's JWT.
func (c *client) authorize(ctx context.Context) (context.Context, error) {
	token, err := c.token(ctx)
	if err != nil {
		return nil, err
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token), nil
}

// refused marks boxd's Unauthenticated answer as machine.ErrUnauthorized,
// keeping its status, and drops the JWT it was sent with, so the next call
// exchanges the key again rather than resend a JWT boxd no longer takes.
func (c *client) refused(err error) error {
	if status.Code(err) != codes.Unauthenticated {
		return err
	}
	c.mu.Lock()
	c.jwt = ""
	c.mu.Unlock()
	return fmt.Errorf("%w: %w", err, machine.ErrUnauthorized)
}

// refusedStream is a stream whose Unauthenticated answer, which boxd gives
// on a message rather than at its start, is marked as a call's is.
type refusedStream struct {
	grpc.ClientStream
	c *client
}

func (s *refusedStream) RecvMsg(m any) error { return s.c.refused(s.ClientStream.RecvMsg(m)) }
func (s *refusedStream) SendMsg(m any) error { return s.c.refused(s.ClientStream.SendMsg(m)) }
func (s *refusedStream) CloseSend() error    { return s.c.refused(s.ClientStream.CloseSend()) }

func (s *refusedStream) Header() (metadata.MD, error) {
	md, err := s.ClientStream.Header()
	return md, s.c.refused(err)
}

// secret is the key's secret for the next call.
func (c *client) secret(ctx context.Context) (string, error) {
	if c.keys == nil {
		return "", errNoCredentials
	}
	key, err := c.keys(ctx)
	if err != nil {
		return "", fmt.Errorf("boxd: key: %w", err)
	}
	secret := strings.TrimSpace(key.Secret)
	if secret == "" {
		return "", errNoCredentials
	}
	return secret, nil
}

// token is the bearer for the next call. An API key (bxd_...) is exchanged for
// a JWT that lives an hour, again whenever the key changes; anything else is
// taken to be a JWT already.
func (c *client) token(ctx context.Context) (string, error) {
	secret, err := c.secret(ctx)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(secret, "bxd_") {
		return secret, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jwt != "" && c.jwtFor == secret && time.Until(c.expires) > 5*time.Minute {
		return c.jwt, nil
	}
	body, err := json.Marshal(map[string]string{"api_key": secret})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	machine.DebugCall(started, err, "boxd POST %s", c.tokenURL)
	if err != nil {
		return "", fmt.Errorf("boxd: exchange API key: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("boxd: exchange API key: %s: %s", resp.Status, bytes.TrimSpace(msg))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			err = fmt.Errorf("%w: %w", err, machine.ErrUnauthorized)
		}
		return "", err
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
	c.jwt, c.jwtFor, c.expires = out.Token, secret, time.Unix(out.ExpiresAt, 0)
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
