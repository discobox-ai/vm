package archil

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ContainerImageEnv names an image the container test runs the fake's
// sandboxes in, such as debian:sid, with Docker: set, a sandbox of the fake
// is a container whose PID 1 is sh, as Archil's guest's PID 1 is Archil's
// own init, and the driver's lines run in it as Archil runs them.
const ContainerImageEnv = "DISCO_VM_ARCHIL_CONTAINER_IMAGE"

// fakeInit is the containers' PID 1, standing in for /opt/archil/init: sh,
// which starts each service Archil's supervisor holds, as Archil's init does
// at a boot, and then waits.
const fakeInit = `#!/bin/sh
for f in /var/lib/fake-archil/services/*; do
	[ -f "$f" ] && /opt/archil/archil-client services start "${f##*/}"
done
trap 'exit 0' TERM
while :; do sleep 1 & wait $!; done
`

// fakeArchilClient stands in for Archil's archil-client services: a service
// is its argv in a file of the root disk, which a fork carries, run again
// whenever it exits, and started at every boot by fakeInit.
const fakeArchilClient = `#!/bin/sh
d=/var/lib/fake-archil/services
[ "$1" = services ] || { echo "fake archil-client: $*" >&2; exit 2; }
case $2 in
get) [ -f "$d/$3" ] ;;
create)
	n=$3; shift 3
	[ "$1" = -- ] && shift
	[ -e "$d/$n" ] && { echo "a service named $n exists" >&2; exit 1; }
	mkdir -p "$d" && printf '%s\0' "$@" > "$d/$n" || exit
	exec "$0" services start "$n" ;;
start)
	setsid sh -c 'while :; do xargs -0 sh -c '\''exec "$@"'\'' sh < "$1"; sleep 1; done' sh "$d/$3" </dev/null >/dev/null 2>&1 &
	;;
*) echo "fake archil-client: services $2" >&2; exit 2 ;;
esac
`

// fakeContainers runs the fake's sandboxes as Docker containers: a root disk
// is a container's, a fork a container of its source committed as an image,
// a stop a docker stop, and a disk a directory of the fake's, bound at its
// path once the container has started, as Archil's client mounts a disk in
// a guest that is booting. A sandbox fenced to nothing has no network.
type fakeContainers struct {
	t     *testing.T
	image string
	// opt is /opt/archil in every container.
	opt string
	// host is the fake's directory, at /.fake-host in every container.
	host string
}

func newFakeContainers(t *testing.T, f *fakeAPI, image string) *fakeContainers {
	t.Helper()
	c := &fakeContainers{t: t, image: image, opt: t.TempDir(), host: f.dir}
	for name, text := range map[string]string{"init": fakeInit, "archil-client": fakeArchilClient} {
		if err := os.WriteFile(filepath.Join(c.opt, name), []byte(text), 0o755); err != nil { //nolint:gosec // G306: run in the containers
			t.Fatal(err)
		}
	}
	return c
}

func (c *fakeContainers) docker(ctx context.Context, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker %s: %w: %s", args[0], err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

func containerOf(s *fakeSandbox) string { return "dvm-fake-" + s.id }

// create makes s's container, of image, fenced to nothing where its
// policy denies all.
func (c *fakeContainers) create(s *fakeSandbox, image string) error {
	args := []string{
		"create", "--name", containerOf(s), "--hostname", s.name, "--privileged",
		"--volume", c.opt + ":/opt/archil:ro", "--volume", c.host + ":/.fake-host",
		"--entrypoint", "/opt/archil/init",
	}
	var p egressPolicy
	if err := json.Unmarshal(s.network, &p); err == nil && p.Egress != nil && p.Egress.Default == "deny" && len(p.Egress.Allow) == 0 {
		args = append(args, "--network", "none")
	}
	return c.docker(context.Background(), append(args, image)...)
}

// fork makes s's container from src's root disk.
func (c *fakeContainers) fork(src, s *fakeSandbox) error {
	image := "dvm-fake-fork:" + s.id[len(s.id)-12:]
	if err := c.docker(context.Background(), "commit", containerOf(src), image); err != nil {
		return err
	}
	return c.create(s, image)
}

// boot starts s's container, then binds each disk at its path.
func (c *fakeContainers) boot(f *fakeAPI, s *fakeSandbox) error {
	ctx := context.Background()
	if err := c.docker(ctx, "start", containerOf(s)); err != nil {
		return err
	}
	for _, m := range s.mounts {
		d := f.disk(m.DiskID)
		if d == nil {
			return fmt.Errorf("no disk %s", m.DiskID)
		}
		rel, err := filepath.Rel(c.host, d.dir)
		if err != nil {
			return err
		}
		if err := c.docker(ctx, "exec", containerOf(s), "sh", "-c", `mkdir -p "$1" && mount --bind "$2" "$1"`, "sh", m.Path, "/.fake-host/"+rel); err != nil {
			return err
		}
	}
	return nil
}

func (c *fakeContainers) halt(s *fakeSandbox) {
	if err := c.docker(context.Background(), "stop", "--time", "5", containerOf(s)); err != nil {
		c.t.Errorf("stop %s: %v", s.name, err)
	}
}

func (c *fakeContainers) remove(s *fakeSandbox) {
	_ = c.docker(context.Background(), "rm", "--force", containerOf(s))
	if s.forkOf != "" {
		_ = c.docker(context.Background(), "rmi", "dvm-fake-fork:"+s.id[len(s.id)-12:])
	}
}

// run runs a process's line in s's container, with sh, as Archil runs it.
func (c *fakeContainers) run(ctx context.Context, s *fakeSandbox, req startRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	args := []string{"exec", "--interactive", "--workdir", cmp.Or(req.Cwd, "/")}
	// Archil gives a sandbox's env to every process; a host whose Docker
	// gives its proxy to a container's PID 1 alone has it passed on.
	if environ, err := exec.CommandContext(ctx, "docker", "exec", containerOf(s), "cat", "/proc/1/environ").Output(); err == nil { //nolint:gosec // G204: the fake's own container
		for kv := range strings.SplitSeq(string(environ), "\x00") {
			if k, _, _ := strings.Cut(kv, "="); strings.HasSuffix(strings.ToLower(k), "_proxy") {
				args = append(args, "--env", kv)
			}
		}
	}
	for k, v := range req.Env {
		args = append(args, "--env", k+"="+v)
	}
	cmd := exec.CommandContext(ctx, "docker", append(args, containerOf(s), "sh", "-c", req.Command)...) //nolint:gosec // G204: the driver's line, run as Archil runs it
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}
