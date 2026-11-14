package boxd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
)

const (
	// metadataPortNumber and metadataPath are boxd's in-machine metadata adapter,
	// which listens on a non-isolated machine's default gateway and mints
	// the machine's owner a token, asking for nothing (the boxd SDK's
	// _credentials.py, and discobox-ai/discobox#1090's spike).
	metadataPortNumber = 9002
	metadataPath       = "/v1/auth/token"
	// vmTokenSkew is how long before its expiry an in-machine token is
	// minted again. One lives 15 minutes and is minted, not refreshed, so a
	// minute covers a call's latency and a clock's drift, as boxd's SDK
	// keeps it.
	vmTokenSkew = time.Minute
	// metadataDialTimeout bounds the connection to the adapter, which is on
	// the machine's own gateway: an isolated machine has none, and its
	// gateway does not answer.
	metadataDialTimeout = 2 * time.Second
	metadataTimeout     = 10 * time.Second
)

// vmToken is boxd's in-machine credential: a token the metadata adapter
// mints for the machine's owner, fenced to its org, which the driver uses
// when its configuration's inMachine is set and it is given no key
// (machine.Config.Keys nil). It is kept until a
// minute before it expires, and dropped once boxd refuses it.
type vmToken struct {
	// locate is the adapter's token URL, or errNoCredentials when there is
	// no default gateway to find it on.
	locate func() (string, error)
	// http asks the adapter, through machine.Config.Dial and no proxy: the
	// adapter knows the machine by where its request comes from.
	http *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newVMToken(cfg machine.Config) *vmToken {
	dial := cfg.Dialer()
	return &vmToken{
		locate: metadataURL,
		http: &http.Client{
			Timeout: metadataTimeout,
			Transport: &http.Transport{
				Proxy: nil,
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					ctx, cancel := context.WithTimeout(ctx, metadataDialTimeout)
					defer cancel()
					return dial(ctx, network, addr)
				},
			},
		},
	}
}

// get is the token for the next call: the one kept, or one minted now.
func (v *vmToken) get(ctx context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.token != "" && time.Until(v.expires) > vmTokenSkew {
		return v.token, nil
	}
	v.token = ""
	url, err := v.locate()
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	started := time.Now()
	resp, err := v.http.Do(req)
	machine.DebugCall(started, err, "boxd GET %s", url)
	if err != nil {
		return "", fmt.Errorf("%w (the machine's metadata adapter at %s did not answer: an isolated machine has none): %w", errNoCredentials, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("boxd: in-machine token from %s: %s: %s", url, resp.Status, strings.TrimSpace(string(msg)))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			err = fmt.Errorf("%w: %w", err, machine.ErrUnauthorized)
		}
		return "", err
	}
	var out struct {
		Token     string      `json:"token"`
		ExpiresAt json.Number `json:"expires_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", fmt.Errorf("boxd: in-machine token from %s: %w", url, err)
	}
	secs, err := out.ExpiresAt.Float64()
	if out.Token == "" || err != nil || secs <= 0 || math.IsInf(secs, 0) {
		return "", fmt.Errorf("boxd: in-machine token from %s: the answer has no token or no expiry", url)
	}
	v.token, v.expires = out.Token, time.Unix(0, int64(secs*float64(time.Second)))
	return v.token, nil
}

// drop forgets the token kept, which boxd has refused, so the next call
// mints another.
func (v *vmToken) drop() {
	v.mu.Lock()
	v.token = ""
	v.mu.Unlock()
}

// metadataURL is the adapter's token URL on this machine: its default
// gateway's (/proc/net/route), where boxd's CLI and SDK find it too.
func metadataURL() (string, error) {
	table, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return "", fmt.Errorf("%w (no default gateway, where a boxd machine's metadata adapter is: %w)", errNoCredentials, err)
	}
	gateway, ok := defaultGateway(string(table))
	if !ok {
		return "", fmt.Errorf("%w (no default gateway, where a boxd machine's metadata adapter is)", errNoCredentials)
	}
	return "http://" + net.JoinHostPort(gateway, strconv.Itoa(metadataPortNumber)) + metadataPath, nil
}

// defaultGateway is the IPv4 gateway of the default route in a Linux route
// table (/proc/net/route), whose Gateway column is little-endian hex, as
// boxd's CLI and SDK read it.
func defaultGateway(table string) (string, bool) {
	lines := strings.Split(table, "\n")
	for _, line := range lines[min(1, len(lines)):] {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != "00000000" {
			continue
		}
		ip, err := strconv.ParseUint(fields[2], 16, 32)
		if err != nil || ip == 0 {
			// A default route with no gateway (a point-to-point link):
			// another may have one.
			continue
		}
		return net.IPv4(byte(ip), byte(ip>>8), byte(ip>>16), byte(ip>>24)).String(), true
	}
	return "", false
}
