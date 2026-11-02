package guest

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A console host carries the session's keystrokes and resizes to its console,
// the console's output back, and the command's exit code.
func TestConsoleHost(t *testing.T) {
	in, frames := io.Pipe()
	defer in.Close()
	go func() {
		_ = WriteFrame(frames, FrameResize, resizePayload(40, 120))
		_ = WriteFrame(frames, FrameStdin, []byte("typed-in\r"))
	}()
	app := filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	var out bytes.Buffer
	code, err := ConsoleHost(30, 100, app, app+` /v:on /c "set /p x=&& echo got-!x!&& exit 4"`, in, &out)
	if err != nil {
		t.Fatal(err)
	}
	if code != 4 || !strings.Contains(out.String(), "got-typed-in") {
		t.Fatalf("code=%d output=%q", code, out.String())
	}
}
