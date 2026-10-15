package guest

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"

	"github.com/discobox-ai/vm/pkg/machine"
)

// Relay serves the agent's protocol from a machine's guest operations, so a
// Client in another process reaches them: the engine's shim serves its
// machine this way. Unlike the agent, it can say that an exec could not
// start before upgrading, and a Client sees that as Exec's error.
func Relay(g machine.Guest) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		info, err := g.Info(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, info)
	})
	mux.HandleFunc("GET /v1/files", func(w http.ResponseWriter, r *http.Request) {
		stream, err := g.CopyFrom(r.Context(), r.URL.Query().Get("path"))
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		defer stream.Close()
		w.Header().Set("Content-Type", "application/x-tar")
		w.WriteHeader(http.StatusOK)
		// Headers are sent; an error now can only truncate the stream.
		_, _ = io.Copy(w, stream)
	})
	mux.HandleFunc("PUT /v1/files", func(w http.ResponseWriter, r *http.Request) {
		if err := g.CopyTo(r.Context(), r.URL.Query().Get("path"), r.Body); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		if err := g.Shutdown(r.Context(), r.URL.Query().Get("reboot") == "1"); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /v1/exec", func(w http.ResponseWriter, r *http.Request) {
		var req machine.ExecRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if len(req.Argv) == 0 {
			writeError(w, http.StatusBadRequest, errors.New("argv is required"))
			return
		}
		p, err := g.Exec(r.Context(), req)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		defer p.Close()
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + UpgradeExec + "\r\n\r\n")
		if err := rw.Flush(); err != nil {
			return
		}
		relayExec(conn, rw.Reader, p)
	})
	return mux
}

// relayExec carries one exec session's frames between a Client and a
// machine's process. The client going away closes the process.
func relayExec(conn net.Conn, in *bufio.Reader, p machine.Process) {
	out := &session{conn: conn}
	var reading sync.WaitGroup
	reading.Go(func() {
		for {
			typ, payload, err := ReadFrame(in)
			if err != nil {
				_ = p.Close()
				return
			}
			switch typ {
			case FrameStdin:
				_, _ = p.Write(payload)
			case FrameStdinEOF:
				_ = p.CloseStdin()
			case FrameResize:
				if len(payload) == 4 {
					_ = p.Resize(uint16(payload[0])<<8|uint16(payload[1]), uint16(payload[2])<<8|uint16(payload[3]))
				}
			}
		}
	})
	code, err := p.Wait(frameWriter{out, FrameStdout}, frameWriter{out, FrameStderr})
	if err != nil {
		_ = out.send(FrameError, []byte(err.Error()))
	} else {
		_ = out.send(FrameExit, exitPayload(code))
	}
	// Closing the connection ends the reader.
	_ = conn.Close()
	reading.Wait()
}
