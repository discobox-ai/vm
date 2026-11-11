package archil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func newAPI(cfg Config, keys machine.KeySource) *api {
	base := cfg.BaseURL
	if base == "" {
		base = controlPlanes[cfg.Region]
	}
	return &api{base: strings.TrimRight(base, "/"), region: cfg.Region, keys: keys, http: &http.Client{Timeout: 5 * time.Minute}}
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
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden && env.Code == "" {
			return "", fmt.Errorf("%w: %w", machine.ErrUnauthorized, ae)
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
}

// mount is a disk Archil mounts in a sandbox (SandboxMount).
type mount struct {
	DiskID string `json:"disk_id"`
	Path   string `json:"path,omitempty"`
}

// egressPolicy is a sandbox's network policy (SandboxNetwork).
type egressPolicy struct {
	Egress *struct {
		Default string `json:"default"`
		Allow   []any  `json:"allow,omitempty"`
	} `json:"egress,omitempty"`
}

// createSandbox is the body of POST /api/sandboxes.
type createSandbox struct {
	Name      string        `json:"name"`
	BaseImage string        `json:"base_image,omitempty"`
	VCPUs     int           `json:"vcpu_count,omitempty"`
	MemoryMiB uint64        `json:"mem_size_mib,omitempty"`
	IdleTTL   int           `json:"idle_ttl_seconds"`
	Mounts    []mount       `json:"mounts,omitempty"`
	Network   *egressPolicy `json:"network,omitempty"`
}

func (a *api) createSandbox(ctx context.Context, body createSandbox) (sandbox, error) {
	var out sandbox
	return out, a.call(ctx, http.MethodPost, "/api/sandboxes", nil, body, &out)
}

func (a *api) sandbox(ctx context.Context, id string) (sandbox, error) {
	var out sandbox
	return out, a.call(ctx, http.MethodGet, "/api/sandboxes/"+url.PathEscape(id), nil, nil, &out)
}

func (a *api) sandboxes(ctx context.Context) ([]sandbox, error) {
	var out struct {
		Sandboxes []sandbox `json:"sandboxes"`
	}
	return out.Sandboxes, a.call(ctx, http.MethodGet, "/api/sandboxes", nil, nil, &out)
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

func (a *api) fork(ctx context.Context, id, name string) (sandbox, error) {
	var out sandbox
	return out, a.call(ctx, http.MethodPost, "/api/sandboxes/"+url.PathEscape(id)+"/fork", nil, map[string]string{"name": name}, &out)
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

// exposePort makes a port public and returns its hostname; a port already
// exposed answers its hostname again.
func (a *api) exposePort(ctx context.Context, id string, port uint32) (string, error) {
	var out struct {
		Hostname string `json:"hostname"`
	}
	err := a.call(ctx, http.MethodPut, fmt.Sprintf("/api/sandboxes/%s/ports/%d", url.PathEscape(id), port), nil, nil, &out)
	return out.Hostname, err
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
		if next == "" || next == query.Get("cursor") {
			return all, nil
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
