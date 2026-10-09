package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// HostEnv names the Docker daemon, as the docker CLI reads it:
// unix:///var/run/docker.sock (the default) or tcp://HOST:PORT without TLS.
const HostEnv = "DOCKER_HOST"

const defaultHost = "unix:///var/run/docker.sock"

// api is a client of the Docker Engine API, the few calls the driver makes.
// It talks HTTP to the daemon's socket itself rather than through the docker
// CLI or SDK, so it works from a server that embeds the engine.
type api struct {
	host    string
	network string
	addr    string
	http    *http.Client
}

// apiError is a non-2xx answer from the daemon.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("docker: %s (%d)", e.Message, e.Status) }

func isNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

func newAPI() *api {
	host := os.Getenv(HostEnv)
	if host == "" {
		host = defaultHost
	}
	a := &api{host: host}
	switch {
	case strings.HasPrefix(host, "unix://"):
		a.network, a.addr = "unix", strings.TrimPrefix(host, "unix://")
	case strings.HasPrefix(host, "tcp://"):
		a.network, a.addr = "tcp", strings.TrimPrefix(host, "tcp://")
	}
	a.http = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return a.dial(ctx) },
	}}
	return a
}

func (a *api) dial(ctx context.Context) (net.Conn, error) {
	if a.network == "" {
		return nil, fmt.Errorf("docker: %s=%q: only unix:// and tcp:// (without TLS) are supported", HostEnv, a.host)
	}
	var d net.Dialer
	return d.DialContext(ctx, a.network, a.addr)
}

func endpoint(path string, query url.Values) string {
	u := "http://docker" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// do sends a request and returns the response for a 2xx status, or the
// daemon's message as an *apiError.
func (a *api) do(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint(path, query), body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker: %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var msg struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &msg) != nil || msg.Message == "" {
			msg.Message = strings.TrimSpace(string(data))
		}
		return nil, &apiError{Status: resp.StatusCode, Message: msg.Message}
	}
	return resp, nil
}

// call sends in as JSON (when not nil) and decodes the answer into out (when
// not nil).
func (a *api) call(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body io.Reader
	contentType := ""
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, contentType = bytes.NewReader(data), "application/json"
	}
	resp, err := a.do(ctx, method, path, query, body, contentType)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type info struct {
	OSType        string   `json:"OSType"`
	Architecture  string   `json:"Architecture"`
	CgroupVersion string   `json:"CgroupVersion"`
	ServerVersion string   `json:"ServerVersion"`
	SecurityOpts  []string `json:"SecurityOptions"`
}

func (a *api) info(ctx context.Context) (info, error) {
	var out info
	return out, a.call(ctx, http.MethodGet, "/info", nil, nil, &out)
}

type imageInfo struct {
	ID string `json:"Id"`
}

func (a *api) inspectImage(ctx context.Context, ref string) (imageInfo, error) {
	var out imageInfo
	return out, a.call(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, &out)
}

// pull fetches an image, following its progress stream to the end: the
// daemon reports a failed pull inside a 200 answer.
func (a *api) pull(ctx context.Context, ref string, log func(string, ...any)) error {
	name, tag := splitRef(ref)
	resp, err := a.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {name}, "tag": {tag}}, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	last := ""
	for {
		var msg struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&msg); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("docker: pull %s: %w", ref, err)
		}
		if msg.Error != "" {
			return fmt.Errorf("docker: pull %s: %s", ref, msg.Error)
		}
		// Per-layer progress is noise in a build log; the overall status
		// lines (pulling, digest, downloaded) are not.
		if msg.Status != last && !strings.HasPrefix(msg.Status, "Downloading") && !strings.HasPrefix(msg.Status, "Extracting") {
			last = msg.Status
			log("docker: %s", msg.Status)
		}
	}
}

// splitRef splits an image reference into the repository and tag that the
// pull endpoint takes. A digest stays in the repository.
func splitRef(ref string) (string, string) {
	if strings.Contains(ref, "@") {
		return ref, ""
	}
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, "latest"
}

func (a *api) removeImage(ctx context.Context, ref string) error {
	err := a.call(ctx, http.MethodDelete, "/images/"+ref, nil, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// containerConfig is the body of a container create, with only the fields the
// driver sets.
type containerConfig struct {
	Image      string            `json:"Image"`
	Hostname   string            `json:"Hostname,omitempty"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Cmd        []string          `json:"Cmd"`
	Env        []string          `json:"Env,omitempty"`
	User       string            `json:"User,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig hostConfig        `json:"HostConfig"`
}

type hostConfig struct {
	SecurityOpt  []string          `json:"SecurityOpt,omitempty"`
	CgroupnsMode string            `json:"CgroupnsMode,omitempty"`
	Tmpfs        map[string]string `json:"Tmpfs,omitempty"`
	// MaskedPaths and ReadonlyPaths, set to empty lists, are the API's
	// spelling of the CLI's --security-opt systempaths=unconfined. They are
	// pointers so that nil leaves the daemon's defaults.
	MaskedPaths   *[]string `json:"MaskedPaths,omitempty"`
	ReadonlyPaths *[]string `json:"ReadonlyPaths,omitempty"`
	NanoCPUs      int64     `json:"NanoCpus,omitempty"`
	Memory        int64     `json:"Memory,omitempty"`
	MemorySwap    int64     `json:"MemorySwap,omitempty"`
}

func (a *api) createContainer(ctx context.Context, name string, cfg containerConfig) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := a.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, cfg, &out)
	return out.ID, err
}

type containerState struct {
	Running  bool   `json:"Running"`
	Status   string `json:"Status"`
	ExitCode int    `json:"ExitCode"`
	Error    string `json:"Error"`
}

type containerInfo struct {
	ID         string         `json:"Id"`
	Name       string         `json:"Name"`
	State      containerState `json:"State"`
	HostConfig struct {
		NanoCPUs int64 `json:"NanoCpus"`
		Memory   int64 `json:"Memory"`
	} `json:"HostConfig"`
}

func (a *api) inspectContainer(ctx context.Context, id string) (containerInfo, error) {
	var out containerInfo
	return out, a.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &out)
}

func (a *api) start(ctx context.Context, id string) error {
	return a.call(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
}

// wait blocks until the container is not running and returns its exit code.
func (a *api) wait(ctx context.Context, id string) (int, error) {
	var out struct {
		StatusCode int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if err := a.call(ctx, http.MethodPost, "/containers/"+id+"/wait", url.Values{"condition": {"not-running"}}, nil, &out); err != nil {
		return -1, err
	}
	if out.Error != nil && out.Error.Message != "" {
		return out.StatusCode, fmt.Errorf("docker: wait %s: %s", id, out.Error.Message)
	}
	return out.StatusCode, nil
}

func (a *api) kill(ctx context.Context, id string) error {
	err := a.call(ctx, http.MethodPost, "/containers/"+id+"/kill", url.Values{"signal": {"KILL"}}, nil, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusConflict {
		return nil // not running
	}
	return err
}

func (a *api) update(ctx context.Context, id string, res hostConfig) error {
	return a.call(ctx, http.MethodPost, "/containers/"+id+"/update", nil, res, nil)
}

func (a *api) removeContainer(ctx context.Context, id string) error {
	err := a.call(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"1"}, "v": {"1"}}, nil, nil)
	if isNotFound(err) {
		return nil
	}
	return err
}

// putArchive extracts a tar stream into the container's filesystem at dir.
func (a *api) putArchive(ctx context.Context, id, dir string, tarball io.Reader) error {
	resp, err := a.do(ctx, http.MethodPut, "/containers/"+id+"/archive", url.Values{"path": {dir}}, tarball, "application/x-tar")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// logs returns the tail of a stopped container's output.
func (a *api) logs(ctx context.Context, id string) (string, error) {
	resp, err := a.do(ctx, http.MethodGet, "/containers/"+id+"/logs", url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {"40"}}, nil, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	err = demux(bufio.NewReader(resp.Body), &out, &out)
	return out.String(), err
}

// commit captures a stopped container's filesystem as a new image, tagged
// repo:tag, and returns the image's ID. The image keeps the container's
// config: its entrypoint, user, and environment.
func (a *api) commit(ctx context.Context, id, repo, tag string, changes []string) (string, error) {
	q := url.Values{"container": {id}, "repo": {repo}, "tag": {tag}, "pause": {"false"}}
	for _, c := range changes {
		q.Add("changes", c)
	}
	var out struct {
		ID string `json:"Id"`
	}
	err := a.call(ctx, http.MethodPost, "/commit", q, nil, &out)
	return out.ID, err
}

// exec starts a command in a running container and returns its stdin and its
// multiplexed output as a hijacked connection, and the exec's ID.
func (a *api) exec(ctx context.Context, id string, cmd []string) (net.Conn, *bufio.Reader, string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	err := a.call(ctx, http.MethodPost, "/containers/"+id+"/exec", nil, map[string]any{
		"AttachStdin": true, "AttachStdout": true, "AttachStderr": true, "Tty": false, "Cmd": cmd,
	}, &created)
	if err != nil {
		return nil, nil, "", err
	}
	conn, err := a.dial(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	// The upgrade is written by hand, because net/http does not hand a
	// client the connection under a 101 answer to anything but a websocket.
	body := `{"Detach":false,"Tty":false}`
	req := "POST /exec/" + created.ID + "/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\n" +
		"Connection: Upgrade\r\nUpgrade: tcp\r\n" + fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)) + body
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, nil, "", err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil) //nolint:bodyclose // on 101 the body is the upgraded conn, which the caller owns; otherwise conn is closed
	if err != nil {
		conn.Close()
		return nil, nil, "", fmt.Errorf("docker: exec start: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		conn.Close()
		return nil, nil, "", &apiError{Status: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, br, created.ID, nil
}

func (a *api) execExitCode(ctx context.Context, execID string) (int, bool, error) {
	var out struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	}
	err := a.call(ctx, http.MethodGet, "/exec/"+execID+"/json", nil, nil, &out)
	return out.ExitCode, !out.Running, err
}

// demux copies Docker's multiplexed stream, frames of an 8-byte header (the
// stream, then a big-endian length) and a payload, to stdout and stderr.
func demux(r *bufio.Reader, stdout, stderr io.Writer) error {
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		n := int64(hdr[4])<<24 | int64(hdr[5])<<16 | int64(hdr[6])<<8 | int64(hdr[7])
		w := stdout
		if hdr[0] == 2 {
			w = stderr
		}
		if _, err := io.CopyN(w, r, n); err != nil {
			return err
		}
	}
}
