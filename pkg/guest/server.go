package guest

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Server is the guest agent.
type Server struct {
	// Version is reported by Info.
	Version string
	// Root, when set, confines the agent to a directory: guest paths resolve
	// under it and processes start in it. The fake driver sets it; a real
	// guest leaves it empty and the agent sees the whole machine.
	Root string
	// Fake makes shutdown exit the agent instead of powering the machine off.
	Fake bool
	// Exit is called to end a fake guest. It defaults to os.Exit.
	Exit func(code int)
}

// Serve answers requests on l until it is closed.
func (s *Server) Serve(l net.Listener) error {
	server := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 30 * time.Second}
	return server.Serve(l)
}

// Handler is the agent's HTTP API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/info", s.info)
	mux.HandleFunc("POST /v1/exec", s.exec)
	mux.HandleFunc("GET /v1/files", s.getFiles)
	mux.HandleFunc("PUT /v1/files", s.putFiles)
	mux.HandleFunc("POST /v1/shutdown", s.shutdown)
	return mux
}

func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	hostname, _ := os.Hostname()
	writeJSON(w, http.StatusOK, Info{
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		Hostname: hostname,
		Version:  s.Version,
		Fake:     s.Fake,
	})
}

// resolve maps a guest path onto the agent's filesystem. Without a Root it is
// the identity. With one, a relative path is under Root and an absolute path is
// re-rooted there, its drive letter becoming a directory, so specs can be
// written with the guest's real paths and still run against the fake driver.
func (s *Server) resolve(path string) string {
	if s.Root == "" {
		if path == "" {
			return "."
		}
		return path
	}
	if path == "" {
		return s.Root
	}
	if volume := filepath.VolumeName(path); volume != "" {
		path = filepath.Join(strings.TrimSuffix(volume, ":"), path[len(volume):])
	}
	return filepath.Join(s.Root, filepath.Clean(string(filepath.Separator)+path))
}

func (s *Server) getFiles(w http.ResponseWriter, r *http.Request) {
	path := s.resolve(r.URL.Query().Get("path"))
	if _, err := os.Lstat(path); err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	// Headers are sent; an error now can only truncate the stream, which the
	// reader sees as a corrupt tar.
	_ = Pack(w, path, false)
}

func (s *Server) putFiles(w http.ResponseWriter, r *http.Request) {
	dir := s.resolve(r.URL.Query().Get("path"))
	if err := Unpack(r.Body, dir); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	reboot := r.URL.Query().Get("reboot") == "1"
	w.WriteHeader(http.StatusAccepted)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// After the response, so the host learns the request was accepted before
	// the machine (or, for the fake driver, the process) goes away.
	go func() {
		time.Sleep(100 * time.Millisecond)
		if s.Fake {
			exit := s.Exit
			if exit == nil {
				exit = os.Exit
			}
			exit(0)
			return
		}
		if err := powerOff(reboot); err != nil {
			fmt.Fprintf(os.Stderr, "guest: shutdown: %v\n", err)
		}
	}()
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	var req ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Argv) == 0 {
		writeError(w, http.StatusBadRequest, errors.New("argv is required"))
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("connection cannot be upgraded"))
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + UpgradeExec + "\r\n\r\n")
	if err := rw.Flush(); err != nil {
		return
	}
	s.runSession(conn, rw.Reader, req)
}

// session is one exec connection's writer side: frames from several
// goroutines, one at a time.
type session struct {
	mu   sync.Mutex
	conn net.Conn
}

func (s *session) send(typ byte, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return WriteFrame(s.conn, typ, payload)
}

// frameWriter is an io.Writer that becomes frames of one type.
type frameWriter struct {
	s   *session
	typ byte
}

func (f frameWriter) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		n := min(len(p)-written, maxFrame)
		if err := f.s.send(f.typ, p[written:written+n]); err != nil {
			return written, err
		}
		written += n
	}
	return written, nil
}

func (s *Server) runSession(conn net.Conn, in *bufio.Reader, req ExecRequest) {
	out := &session{conn: conn}
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = s.resolve(req.Dir)
	var login []string
	if req.User != "" {
		var err error
		if login, err = runAs(cmd, req.User); err != nil {
			_ = out.send(FrameError, []byte(err.Error()))
			return
		}
	}
	// The request's env comes last, so it can override the login's.
	base := os.Environ()
	if !s.Fake {
		base = processEnv()
	}
	cmd.Env = append(append(base, login...), req.Env...)

	var (
		stdin  io.WriteCloser
		resize func(rows, cols uint16) error
		copies sync.WaitGroup
	)
	if req.TTY {
		pty, err := startPTY(cmd, req.Rows, req.Cols)
		if err != nil {
			_ = out.send(FrameError, []byte(err.Error()))
			return
		}
		defer pty.Close()
		stdin, resize = pty, pty.Resize
		copies.Add(1)
		go func() {
			defer copies.Done()
			// A PTY reports the child's exit as a read error (EIO), not EOF.
			_, _ = io.Copy(frameWriter{out, FrameStdout}, pty)
		}()
	} else {
		var err error
		if stdin, err = cmd.StdinPipe(); err != nil {
			_ = out.send(FrameError, []byte(err.Error()))
			return
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			_ = out.send(FrameError, []byte(err.Error()))
			return
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			_ = out.send(FrameError, []byte(err.Error()))
			return
		}
		if err := cmd.Start(); err != nil {
			_ = out.send(FrameError, []byte(err.Error()))
			return
		}
		copies.Add(2)
		go func() { defer copies.Done(); _, _ = io.Copy(frameWriter{out, FrameStdout}, stdout) }()
		go func() { defer copies.Done(); _, _ = io.Copy(frameWriter{out, FrameStderr}, stderr) }()
	}

	// The host's side: stdin, resizes, and the connection closing. A host that
	// goes away abandons the process, so it is killed rather than left running
	// with nobody to read it.
	exited := make(chan struct{})
	go func() {
		for {
			typ, payload, err := ReadFrame(in)
			if err != nil {
				select {
				case <-exited:
				default:
					if cmd.Process != nil {
						_ = cmd.Process.Kill()
					}
				}
				return
			}
			switch typ {
			case FrameStdin:
				_, _ = stdin.Write(payload)
			case FrameStdinEOF:
				_ = stdin.Close()
			case FrameResize:
				if resize != nil && len(payload) == 4 {
					_ = resize(uint16(payload[0])<<8|uint16(payload[1]), uint16(payload[2])<<8|uint16(payload[3]))
				}
			}
		}
	}()

	var code int
	if req.TTY {
		err := cmd.Wait()
		close(exited)
		code = exitCode(err)
		copies.Wait()
	} else {
		copies.Wait()
		err := cmd.Wait()
		close(exited)
		code = exitCode(err)
	}
	_ = out.send(FrameExit, exitPayload(code))
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	http.Error(w, err.Error(), status)
}
