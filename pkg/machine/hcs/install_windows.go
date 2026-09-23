package hcs

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Microsoft/go-winio/vhd"
	"golang.org/x/sys/windows"

	"github.com/discobox-ai/vm/internal/units"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

//go:embed install.ps1
var installScript []byte

const (
	installDiskName = "install.vhdx"
	// breadcrumb names the disk an install has attached to the host. The
	// attach dies with the installing process, so the breadcrumb only matters
	// after a power loss: the next install detaches what it names.
	breadcrumb = ".attached"

	defaultDiskBytes = 64 << 30
	minDiskBytes     = 32 << 30
	defaultUser      = "disco"

	setupTimeout = 90 * time.Minute
)

// Install lays Windows from a retail ISO onto a new disk in dst.Dir, bakes in
// the agent as a boot-start service, and boots it once through Windows Setup,
// so the base layer is a Windows that has already been set up. It commits only
// after an orderly shutdown through the agent.
//
// Options: user and password name the local administrator Setup creates and
// logs on automatically (default "disco", blank).
func (d *Driver) Install(ctx context.Context, spec machine.InstallSpec, dst machine.Layer) error {
	log := spec.Log
	if log == nil {
		log = io.Discard
	}
	if spec.GuestOS != machine.Windows {
		return fmt.Errorf("hcs: can only install windows guests, not %s", spec.GuestOS)
	}
	if spec.Media == "" || spec.Media == "latest" {
		return errors.New("hcs: install needs a Windows ISO as its media; Microsoft allows no unattended download (https://www.microsoft.com/software-download/windows11)")
	}
	if fi, err := os.Stat(spec.Media); err != nil || fi.IsDir() {
		return fmt.Errorf("hcs: install media %s is not an ISO file", spec.Media)
	}
	if _, err := os.Stat(spec.Agent); err != nil {
		return fmt.Errorf("hcs: the agent to bake in: %w", err)
	}
	if err := d.Check(ctx); err != nil {
		return err
	}
	size := uint64(spec.DiskBytes)
	if size == 0 {
		size = defaultDiskBytes
	}
	if size < minDiskBytes {
		return fmt.Errorf("hcs: a %s disk is too small for Windows; give it at least %s", units.FormatBytes(size), units.FormatBytes(minDiskBytes))
	}
	if err := os.MkdirAll(dst.Dir, 0o755); err != nil {
		return err
	}
	sweepAttached(dst.Dir, log)
	// The fixed disk, its dynamic copy, and Setup's growth all at once.
	if err := needSpace(dst.Dir, size+30<<30); err != nil {
		return err
	}

	fixed := filepath.Join(dst.Dir, installDiskName)
	fmt.Fprintf(log, "hcs: allocating a fixed %s disk (this is silent, and takes a while)\n", units.FormatBytes(size))
	if err := createFixed(fixed, size); err != nil {
		return err
	}
	if err := writeDisk(ctx, fixed, spec, log); err != nil {
		return err
	}
	fmt.Fprintln(log, "hcs: compacting the disk")
	disk := filepath.Join(dst.Dir, diskName)
	if err := createDynamicFrom(disk, fixed); err != nil {
		return err
	}
	if err := os.Remove(fixed); err != nil {
		return err
	}
	return firstBoot(ctx, spec, dst.Dir, disk, log)
}

// needSpace fails early when dir's volume cannot hold what Install writes.
func needSpace(dir string, need uint64) error {
	var free uint64
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	if err := windows.GetDiskFreeSpaceEx(p, &free, nil, nil); err != nil {
		return err
	}
	if free < need {
		return fmt.Errorf("hcs: %s has %s free, and installing needs about %s; point DISCO_VM_ROOT at a roomier volume", dir, units.FormatBytes(free), units.FormatBytes(need))
	}
	return nil
}

// sweepAttached detaches disks that installs which lost power left attached.
// Installs run in sibling layer directories of the same shape as dst's.
func sweepAttached(dir string, log io.Writer) {
	pattern := filepath.Join(filepath.Dir(filepath.Dir(dir)), "*", filepath.Base(dir), breadcrumb)
	crumbs, _ := filepath.Glob(pattern)
	for _, crumb := range crumbs {
		if data, err := os.ReadFile(crumb); err == nil {
			if path := strings.TrimSpace(string(data)); path != "" && vhd.DetachVhd(path) == nil {
				fmt.Fprintf(log, "hcs: detached %s, left attached by an earlier install\n", path)
			}
		}
		_ = os.Remove(crumb)
	}
}

// writeDisk attaches the fixed disk to the host, runs the install script over
// it, and detaches it. This is the only place the driver mounts a disk.
func writeDisk(ctx context.Context, fixed string, spec machine.InstallSpec, log io.Writer) error {
	dir := filepath.Dir(fixed)
	crumb := filepath.Join(dir, breadcrumb)
	if err := os.WriteFile(crumb, []byte(fixed), 0o644); err != nil {
		return err
	}
	defer os.Remove(crumb)
	disk, err := attach(fixed)
	if err != nil {
		return err
	}
	defer disk.detach()
	fmt.Fprintf(log, "hcs: disk attached to the host as disk %d\n", disk.diskNumber)

	script := filepath.Join(dir, "install.ps1")
	// A BOM, so Windows PowerShell reads the script as UTF-8.
	if err := os.WriteFile(script, append([]byte("\xEF\xBB\xBF"), installScript...), 0o644); err != nil {
		return err
	}
	defer os.Remove(script)
	user := spec.Options["user"]
	if user == "" {
		user = defaultUser
	}
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-File", script,
		"-Iso", spec.Media, "-Vhdx", fixed, "-DiskNumber", strconv.Itoa(disk.diskNumber),
		"-Agent", spec.Agent, "-Edition", spec.Edition, "-User", user, "-Password", spec.Options["password"])
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("hcs: writing Windows to the disk: %w", err)
	}
	return nil
}

// firstBoot boots the new disk directly, lets Windows Setup run to a logged-on
// desktop, and shuts it down through the agent.
func firstBoot(ctx context.Context, spec machine.InstallSpec, dir, disk string, log io.Writer) error {
	work := filepath.Join(dir, "firstboot")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	defer removeAll(work)
	if err := freshState(work); err != nil {
		return err
	}
	id := newID()
	paths := []string{dir, disk, work, filepath.Join(work, guestFileName), filepath.Join(work, stateFileName)}
	if err := grantAll(id, paths); err != nil {
		return err
	}
	defer func() {
		for _, path := range paths {
			revokeVMAccess(id, path)
		}
	}()
	nic, err := createEndpoint("")
	if err != nil {
		return err
	}
	fmt.Fprintln(log, "hcs: first boot: Windows Setup runs now (several minutes, with reboots)")
	m, err := startVM(id, vmConfig{
		Disk: disk, GuestFile: filepath.Join(work, guestFileName), StateFile: filepath.Join(work, stateFileName),
		CPUs: spec.CPUs, Memory: spec.Memory, NIC: nic,
		Console: consolePipe(id), ConsoleSID: currentUserSID(),
	})
	if err != nil {
		return err
	}
	defer m.shutdown()
	if spec.GUI {
		// Setup from its first screen: the console needs nothing in the guest.
		if v, err := openViewer(id, "disco-vm install: Windows Setup"); err != nil {
			fmt.Fprintf(log, "hcs: %v\n", err)
		} else {
			defer v.Close()
		}
	}
	client := guest.NewClient(func(ctx context.Context) (net.Conn, error) { return m.Dial(ctx, guest.AgentPort) })
	defer client.Close()

	ctx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	started := time.Now()
	if err := waitSetup(ctx, client, m, started, log); err != nil {
		return err
	}
	fmt.Fprintf(log, "hcs: Setup finished after %s; shutting down to commit\n", time.Since(started).Round(time.Second))
	if err := client.Shutdown(ctx, false); err != nil {
		return fmt.Errorf("hcs: ask the guest to shut down: %w", err)
	}
	select {
	case <-m.Done():
		return m.Err()
	case <-time.After(10 * time.Minute):
		return errors.New("hcs: the guest did not power off after Setup; a disk that has to be forced off is not committed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// waitSetup waits for Windows Setup to finish, the automatic logon to reach a
// desktop, and OOBE to record that it is complete. The agent is up long before
// that (it starts during Setup), and Setup and OOBE reboot under it, so every
// probe tolerates a dropped connection.
func waitSetup(ctx context.Context, client *guest.Client, m *vm, started time.Time, log io.Writer) error {
	probe := func(argv ...string) string {
		var out bytes.Buffer
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, _ = client.Run(pctx, guest.ExecRequest{Argv: argv}, nil, &out, io.Discard)
		return out.String()
	}
	report := time.Now().Add(time.Minute)
	stage := "the agent"
	settled := time.Time{}
	for {
		select {
		case <-m.Done():
			return fmt.Errorf("hcs: the guest stopped during Setup (waiting for %s): %v", stage, m.Err())
		case <-ctx.Done():
			return fmt.Errorf("hcs: Setup did not finish in %s (waiting for %s)", setupTimeout, stage)
		case <-time.After(5 * time.Second):
		}
		if time.Now().After(report) {
			fmt.Fprintf(log, "hcs: waiting for %s, %s in\n", stage, time.Since(started).Round(time.Second))
			report = time.Now().Add(time.Minute)
		}
		if client.Health(ctx) != nil {
			stage, settled = "the agent", time.Time{}
			continue
		}
		state := probe("reg.exe", "query", `HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\Setup\State`, "/v", "ImageState")
		if !strings.Contains(state, "IMAGE_STATE_COMPLETE") {
			stage, settled = "Setup ("+imageState(state)+")", time.Time{}
			continue
		}
		if !strings.Contains(strings.ToLower(probe("tasklist.exe", "/FI", "IMAGENAME eq explorer.exe", "/NH")), "explorer.exe") {
			stage, settled = "the automatic logon", time.Time{}
			continue
		}
		// OOBE is not over at the desktop: at the first logon it fetches a
		// Zero Day Patch and reboots to install it, and only then records
		// its completion. A disk cut before that makes every instance do it
		// on its own, rebooting under whatever the instance was running.
		if !strings.Contains(probe("reg.exe", "query", `HKLM\SOFTWARE\Microsoft\Windows\CurrentVersion\OOBE\OOBECompleteTimestamp`, "/v", "OOBECompleteTimestamp"), "REG_BINARY") {
			stage, settled = "OOBE to finish (its Zero Day Patch reboots the guest once)", time.Time{}
			continue
		}
		// The first logon keeps provisioning the user's apps after the shell
		// appears; a profile cut mid-way is a broken one in every clone.
		if settled.IsZero() {
			stage, settled = "the first logon to settle", time.Now()
		}
		if time.Since(settled) >= time.Minute {
			return nil
		}
	}
}

func imageState(out string) string {
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, "IMAGE_STATE_") {
			return field
		}
	}
	return "starting"
}
