package archil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
)

const (
	// APIKeyEnv is the key the command line calls Archil with.
	APIKeyEnv = "ARCHIL_API_KEY" //nolint:gosec // G101: an environment variable's name, not a credential
)

// controlPlanes are the regions that run sandboxes, from Archil's OpenAPI
// document's servers. gcp-us-central1 has disks and no sandboxes.
var controlPlanes = map[string]string{
	"aws-us-east-1": "https://control.green.us-east-1.aws.prod.archil.com",
	"aws-us-west-2": "https://control.green.us-west-2.aws.prod.archil.com",
	"aws-eu-west-1": "https://control.green.eu-west-1.aws.prod.archil.com",
}

// errNoKey names the fix, since Check is where a user meets it first.
var errNoKey = fmt.Errorf("archil: no key: set %s to an API key from the Archil console", APIKeyEnv)

// api is Archil's control plane over REST, as its OpenAPI document gives it.
// It asks keys for the key on every call.
type api struct {
	base   string
	region string
	keys   machine.KeySource
	http   *http.Client
}

func newAPI(cfg Config, keys machine.KeySource, transport *http.Transport) *api {
	base := cfg.BaseURL
	if base == "" {
		base = controlPlanes[cfg.Region]
	}
	return &api{base: strings.TrimRight(base, "/"), region: cfg.Region, keys: keys, http: &http.Client{Transport: transport, Timeout: 5 * time.Minute}}
}

// apiError is an error Archil answered with.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("archil: %s (%s, %d)", e.Message, e.Code, e.Status)
	}
	return fmt.Sprintf("archil: %s (%d)", e.Message, e.Status)
}

func statusOf(err error) (int, string) {
	if ae, ok := errors.AsType[*apiError](err); ok {
		return ae.Status, ae.Code
	}
	return 0, ""
}

// unavailable reports an HTTP status that says Archil, or its gateway, could
// not serve the request for now: machine.ErrUnavailable. A 500 or a 429 is
// Archil answering, and stays its own error.
func unavailable(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func isNotFound(err error) bool { s, _ := statusOf(err); return s == http.StatusNotFound }

func isConflict(err error) bool { s, _ := statusOf(err); return s == http.StatusConflict }

// call sends in as JSON (none when nil) and decodes the envelope's data into
// out (skipped when nil). A 401 or 403 is machine.ErrUnauthorized.
func (a *api) call(ctx context.Context, method, path string, query url.Values, in, out any) error {
	_, err := a.page(ctx, method, path, query, in, out)
	return err
}

// page is call for a listing Archil pages, and returns the envelope's
// nextCursor: "" on its last page.
func (a *api) page(ctx context.Context, method, path string, query url.Values, in, out any) (next string, err error) {
	started := time.Now()
	defer func() { machine.DebugCall(started, err, "archil %s %s", method, path) }()
	if a.base == "" {
		return "", fmt.Errorf("archil: region %q runs no sandboxes (one of aws-us-east-1, aws-us-west-2, aws-eu-west-1)", a.region)
	}
	if a.keys == nil {
		return "", errNoKey
	}
	key, err := a.keys(ctx)
	if err != nil {
		return "", err
	}
	if key.Secret == "" {
		return "", errNoKey
	}
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return "", err
		}
		body = bytes.NewReader(raw)
	}
	u := a.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer key-"+strings.TrimPrefix(key.Secret, "key-"))
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("archil: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}
	var env struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
		Error   string          `json:"error"`
		Code    string          `json:"code"`
		Next    string          `json:"nextCursor"`
	}
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode >= 300 || (len(raw) > 0 && env.Error != "" && !env.Success) {
		msg := env.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		if msg == "" {
			msg = resp.Status
		}
		ae := &apiError{Status: resp.StatusCode, Code: env.Code, Message: msg}
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden && env.Code == "":
			return "", fmt.Errorf("%w: %w", machine.ErrUnauthorized, ae)
		case unavailable(resp.StatusCode):
			return "", fmt.Errorf("%w: %w", machine.ErrUnavailable, ae)
		}
		return "", ae
	}
	if out == nil || len(env.Data) == 0 || string(env.Data) == "null" {
		return env.Next, nil
	}
	return env.Next, json.Unmarshal(env.Data, out)
}

// sandbox is a sandbox as Archil reports it, with the fields the driver
// reads.
type sandbox struct {
	ID         string    `json:"sandbox_id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	VCPUs      int       `json:"vcpu_count"`
	MemoryMiB  uint64    `json:"mem_size_mib"`
	BaseImage  string    `json:"base_image"`
	Created    time.Time `json:"created_at"`
	ExitReason string    `json:"exit_reason"`
	Checkpoint string    `json:"checkpoint"`
	Mounts     []mount   `json:"mounts"`
	// Labels are set when it is made and never change (names.go).
	Labels map[string]string `json:"labels"`
}

// mount is a disk Archil mounts in a sandbox (SandboxMount).
type mount struct {
	DiskID string `json:"disk_id"`
	Path   string `json:"path,omitempty"`
}

// egressPolicy is a sandbox's network policy (SandboxNetwork).
type egressPolicy struct {
	Egress *egressRules `json:"egress,omitempty"`
}

// egressRules is a policy's egress (SandboxEgressPolicy): Deny wins over
// Allow where both match.
type egressRules struct {
	Default string   `json:"default"`
	Allow   []any    `json:"allow,omitempty"`
	Deny    []string `json:"deny,omitempty"`
}

// createSandbox is the body of POST /api/sandboxes.
type createSandbox struct {
	Name      string            `json:"name"`
	BaseImage string            `json:"base_image,omitempty"`
	VCPUs     int               `json:"vcpu_count,omitempty"`
	MemoryMiB uint64            `json:"mem_size_mib,omitempty"`
	IdleTTL   int               `json:"idle_ttl_seconds"`
	Mounts    []mount           `json:"mounts,omitempty"`
	Network   *egressPolicy     `json:"network,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	// ServiceIngress lets a service in the guest publish a port of its own
	// choosing. It is always sent, false: the guest's root opens nothing.
	ServiceIngress bool `json:"enable_service_ingress"`
}

func (a *api) createSandbox(ctx context.Context, body createSandbox) (sandbox, error) {
	var out sandbox
	return out, a.call(ctx, http.MethodPost, "/api/sandboxes", nil, body, &out)
}

func (a *api) sandbox(ctx context.Context, id string) (sandbox, error) {
	var out sandbox
	return out, a.call(ctx, http.MethodGet, "/api/sandboxes/"+url.PathEscape(id), nil, nil, &out)
}

// sandboxes lists the account's sandboxes that pass filter's filters
// (name, name_prefix, label), every page of them. Archil's
// document gives this listing no pages, and it returned every sandbox when
// asked for fewer (2026-11-16), but a listing read short would lose sight of
// VMs, so a nextCursor, wherever Archil puts it, is followed as the disks'
// is.
func (a *api) sandboxes(ctx context.Context, filter url.Values) ([]sandbox, error) {
	query := maps.Clone(filter)
	if query == nil {
		query = url.Values{}
	}
	var all []sandbox
	for {
		var out struct {
			Sandboxes []sandbox `json:"sandboxes"`
			Next      string    `json:"nextCursor"`
		}
		next, err := a.page(ctx, http.MethodGet, "/api/sandboxes", query, nil, &out)
		if err != nil {
			return nil, err
		}
		all = append(all, out.Sandboxes...)
		if next == "" {
			next = out.Next
		}
		if next == "" {
			return all, nil
		}
		if next == query.Get("cursor") {
			return nil, fmt.Errorf("archil: list sandboxes: the cursor %q came back again", next)
		}
		query.Set("cursor", next)
	}
}

// start cold-starts a stopped sandbox, with mounts in place of its own
// when mounts is not nil (an empty list removes them).
func (a *api) start(ctx context.Context, id string, mounts []mount) error {
	var body any
	if mounts != nil {
		body = map[string]any{"mounts": mounts}
	}
	return a.call(ctx, http.MethodPost, "/api/sandboxes/"+url.PathEscape(id)+"/start", nil, body, nil)
}

func (a *api) stop(ctx context.Context, id string) error {
	return a.call(ctx, http.MethodPost, "/api/sandboxes/"+url.PathEscape(id)+"/stop", nil, nil, nil)
}

func (a *api) resume(ctx context.Context, id string) error {
	return a.call(ctx, http.MethodPost, "/api/sandboxes/"+url.PathEscape(id)+"/resume", nil, nil, nil)
}

// fork forks a sandbox under a name, with labels in place of its source's,
// which Archil copies to a fork given none: always sent, even empty.
func (a *api) fork(ctx context.Context, id, name string, labels map[string]string) (sandbox, error) {
	var out sandbox
	if labels == nil {
		labels = map[string]string{}
	}
	body := map[string]any{"name": name, "labels": labels}
	return out, a.call(ctx, http.MethodPost, "/api/sandboxes/"+url.PathEscape(id)+"/fork", nil, body, &out)
}

func (a *api) deleteSandbox(ctx context.Context, id string) error {
	err := a.call(ctx, http.MethodDelete, "/api/sandboxes/"+url.PathEscape(id), nil, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

func (a *api) network(ctx context.Context, id string) (egressPolicy, error) {
	var out egressPolicy
	return out, a.call(ctx, http.MethodGet, "/api/sandboxes/"+url.PathEscape(id)+"/network", nil, nil, &out)
}

func (a *api) setNetwork(ctx context.Context, id string, p egressPolicy) error {
	return a.call(ctx, http.MethodPut, "/api/sandboxes/"+url.PathEscape(id)+"/network", nil, p, nil)
}

// unexposePort makes a port public no more. One not exposed is not an
// error.
func (a *api) unexposePort(ctx context.Context, id string, port uint32) error {
	err := a.call(ctx, http.MethodDelete, fmt.Sprintf("/api/sandboxes/%s/ports/%d", url.PathEscape(id), port), nil, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// portToken is a sandbox's port token as Archil lists it: never its secret.
type portToken struct {
	ID      string    `json:"id"`
	Port    uint32    `json:"port"`
	Created time.Time `json:"created_at"`
	Expires time.Time `json:"expires_at"`
}

// mintedToken is a port token as Archil makes it: its secret, which it
// never returns again, and the port's stable hostname.
type mintedToken struct {
	portToken
	Hostname string `json:"hostname"`
	Token    string `json:"token"`
}

// mintToken makes a token for a sandbox's port that lasts ttl.
func (a *api) mintToken(ctx context.Context, id string, port uint32, ttl time.Duration) (mintedToken, error) {
	var out mintedToken
	body := map[string]any{"port": port, "ttl": ttl.String()}
	return out, a.call(ctx, http.MethodPost, "/api/sandboxes/"+url.PathEscape(id)+"/port-tokens", nil, body, &out)
}

// portTokens lists a sandbox's unexpired port tokens, every page of them.
func (a *api) portTokens(ctx context.Context, id string) ([]portToken, error) {
	query := url.Values{"limit": {"100"}}
	var all []portToken
	for {
		var out struct {
			Tokens []portToken `json:"tokens"`
			Next   string      `json:"nextCursor"`
		}
		next, err := a.page(ctx, http.MethodGet, "/api/sandboxes/"+url.PathEscape(id)+"/port-tokens", query, nil, &out)
		if err != nil {
			return nil, err
		}
		all = append(all, out.Tokens...)
		if next == "" {
			next = out.Next
		}
		if next == "" {
			return all, nil
		}
		if next == query.Get("cursor") {
			return nil, fmt.Errorf("archil: list port tokens: the cursor %q came back again", next)
		}
		query.Set("cursor", next)
	}
}

// revokeToken revokes a port token. One expired or gone is not an error.
func (a *api) revokeToken(ctx context.Context, id, token string) error {
	err := a.call(ctx, http.MethodDelete, "/api/sandboxes/"+url.PathEscape(id)+"/port-tokens/"+url.PathEscape(token), nil, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// connection is a short-lived URL of the sandbox's process API.
func (a *api) connection(ctx context.Context, id string) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	err := a.call(ctx, http.MethodPost, "/api/sandboxes/"+url.PathEscape(id)+"/connections", nil, nil, &out)
	return out.URL, err
}

// disk is a disk as Archil lists it.
type disk struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Created time.Time `json:"createdAt"`
}

// disks lists the account's disks, every page of them, or with a name only
// the disks of that name.
func (a *api) disks(ctx context.Context, name string) ([]disk, error) {
	query := url.Values{"limit": {"100"}}
	if name != "" {
		query.Set("name", name)
	}
	var all []disk
	for {
		var out []disk
		next, err := a.page(ctx, http.MethodGet, "/api/disks", query, nil, &out)
		if err != nil {
			return nil, err
		}
		all = append(all, out...)
		if next == "" {
			return all, nil
		}
		if next == query.Get("cursor") {
			return nil, fmt.Errorf("archil: list disks: the cursor %q came back again", next)
		}
		query.Set("cursor", next)
	}
}

// createDisk makes a disk of a name, or answers the one of that name: Archil
// does not refuse a name it has.
func (a *api) createDisk(ctx context.Context, name string) (string, error) {
	var out struct {
		DiskID string `json:"diskId"`
	}
	err := a.call(ctx, http.MethodPost, "/api/disks", nil, map[string]string{"name": name}, &out)
	return out.DiskID, err
}

func (a *api) deleteDisk(ctx context.Context, id string) error {
	err := a.call(ctx, http.MethodDelete, "/api/disks/"+url.PathEscape(id), nil, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}
