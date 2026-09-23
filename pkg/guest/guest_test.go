package guest

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// shell runs a one-line script with the host's shell.
func shell(script string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", script}
	}
	return []string{"/bin/sh", "-c", script}
}

func startAgent(t *testing.T) (*Client, string) {
	t.Helper()
	root := t.TempDir()
	l, err := Listen("tcp:127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{Version: "test", Root: root, Fake: true, Exit: func(int) {}}
	go func() { _ = server.Serve(l) }()
	t.Cleanup(func() { _ = l.Close() })
	client := NewClient(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", l.Addr().String())
	})
	t.Cleanup(client.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.WaitReady(ctx, nil); err != nil {
		t.Fatal(err)
	}
	return client, root
}

func TestExecOutputExitCodeAndStdin(t *testing.T) {
	client, _ := startAgent(t)
	ctx := context.Background()

	var stdout, stderr bytes.Buffer
	code, err := client.Run(ctx, ExecRequest{Argv: shell("echo out&& echo err 1>&2&& exit 3")}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 || strings.TrimSpace(stdout.String()) != "out" || strings.TrimSpace(stderr.String()) != "err" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	cat := []string{"/bin/cat"}
	if runtime.GOOS == "windows" {
		cat = []string{"findstr", "^"}
	}
	code, err = client.Run(ctx, ExecRequest{Argv: cat}, strings.NewReader("piped\n"), &stdout, nil)
	if err != nil || code != 0 || strings.TrimSpace(stdout.String()) != "piped" {
		t.Fatalf("stdin: code=%d err=%v stdout=%q", code, err, stdout.String())
	}

	if _, err := client.Run(ctx, ExecRequest{Argv: []string{"definitely-not-a-program-xyz"}}, nil, nil, nil); err == nil {
		t.Fatal("running a missing program did not fail")
	}
}

func TestExecEnvAndWorkdir(t *testing.T) {
	client, root := startAgent(t)
	if err := os.MkdirAll(filepath.Join(root, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "echo $GREETING && pwd"
	if runtime.GOOS == "windows" {
		script = "echo %GREETING%&& cd"
	}
	var out bytes.Buffer
	code, err := client.Run(context.Background(), ExecRequest{Argv: shell(script), Env: []string{"GREETING=hi"}, Dir: "work"}, nil, &out, &out)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v out=%q", code, err, out.String())
	}
	lines := strings.Fields(out.String())
	if len(lines) != 2 || lines[0] != "hi" || !strings.HasSuffix(filepath.ToSlash(lines[1]), "/work") {
		t.Fatalf("out = %q", out.String())
	}
}

func TestFilesRoundTrip(t *testing.T) {
	client, root := startAgent(t)
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "tools")
	if err := os.MkdirAll(filepath.Join(src, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "bin", "tool.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	reader, writer := io.Pipe()
	go func() { writer.CloseWithError(Pack(writer, src, false)) }()
	// An absolute guest path is re-rooted under the fake guest's root.
	guestDir := "/opt"
	if runtime.GOOS == "windows" {
		guestDir = `C:\opt`
	}
	if err := client.CopyTo(ctx, guestDir, reader); err != nil {
		t.Fatal(err)
	}
	landed := filepath.Join(root, "opt", "tools", "bin", "tool.txt")
	if runtime.GOOS == "windows" {
		landed = filepath.Join(root, "C", "opt", "tools", "bin", "tool.txt")
	}
	if data, err := os.ReadFile(landed); err != nil || string(data) != "payload" {
		t.Fatalf("copy in: %q %v", data, err)
	}

	stream, err := client.CopyFrom(ctx, guestDir+"/tools/bin")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	out := t.TempDir()
	if err := Unpack(stream, out); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(out, "bin", "tool.txt")); err != nil || string(data) != "payload" {
		t.Fatalf("copy out: %q %v", data, err)
	}
}

func TestUnpackStaysInDestination(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"../escaped", "/abs"} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte("x"))
	}
	_ = tw.Close()
	parent := t.TempDir()
	target := filepath.Join(parent, "dst")
	if err := Unpack(&buf, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(parent, "escaped")); err == nil {
		t.Fatal("a ../ entry escaped the destination")
	}
	for _, name := range []string{"escaped", "abs"} {
		if _, err := os.Stat(filepath.Join(target, name)); err != nil {
			t.Fatalf("%s was not re-rooted into the destination: %v", name, err)
		}
	}
}

func TestInfo(t *testing.T) {
	client, _ := startAgent(t)
	info, err := client.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.OS != runtime.GOOS || !info.Fake || info.Version != "test" {
		t.Fatalf("info = %+v", info)
	}
}
