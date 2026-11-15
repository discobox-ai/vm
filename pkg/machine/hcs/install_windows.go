package hcs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/discobox-ai/vm/internal/udf"
	"github.com/discobox-ai/vm/internal/units"
	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/local"
)

const (
	defaultDiskBytes = 64 << 30
	minDiskBytes     = 32 << 30
	// minMemory is what Windows 11 Setup asks of a machine.
	minMemory   = 4 << 30
	defaultUser = "disco"

	// mediumName is the install medium disco-vm writes, in the first boot's
	// working directory, which goes with it.
	mediumName = "install-medium.iso"

	setupTimeout = 90 * time.Minute
)

// Install installs Windows from a retail ISO the way a person would on
// Hyper-V, inside the VM, and attaches and mounts nothing on the host (ADR
// 26-11-14-816). The VM boots WinPE from a small medium disco-vm writes,
// with the retail ISO beside it as a read-only DVD. Windows Setup partitions
// a new dynamic disk, applies the edition, installs the agent as a
// boot-start service during specialize, and runs OOBE to a logged-on
// desktop. The committed VM is that disk, once the guest has shut down
// through the agent.
//
// Options: user and password name the local administrator Setup creates and
// logs on automatically (default "disco", and the user's own name for a
// password, which the agent and `disco-vm gui` sign in with). A password of
// the person's own is kept nowhere: `gui` asks for it, and running as the
// account is unsupported (ADR 26-11-14-593).
func (d *Hypervisor) Install(ctx context.Context, spec local.InstallSpec, dst local.Layer) (*machine.Release, error) {
	log := spec.Log
	if log == nil {
		log = io.Discard
	}
	if spec.GuestOS != machine.Windows {
		return nil, fmt.Errorf("hcs: can only install windows guests, not %s", spec.GuestOS)
	}
	if spec.Media == "" || spec.Media == "latest" {
		return nil, errors.New("hcs: install needs a Windows ISO as its media; Microsoft allows no unattended download (https://www.microsoft.com/software-download/windows11)")
	}
	if fi, err := os.Stat(spec.Media); err != nil || fi.IsDir() {
		return nil, fmt.Errorf("hcs: install media %s is not an ISO file", spec.Media)
	}
	if _, err := os.Stat(spec.Agent); err != nil {
		return nil, fmt.Errorf("hcs: the agent to install: %w", err)
	}
	if spec.Memory != 0 && spec.Memory < minMemory {
		return nil, fmt.Errorf("hcs: Windows Setup needs %s of memory, not %s", units.FormatBytes(minMemory), units.FormatBytes(spec.Memory))
	}
	if err := d.Check(ctx); err != nil {
		return nil, err
	}
	size := uint64(spec.DiskBytes)
	if size == 0 {
		size = defaultDiskBytes
	}
	if size < minDiskBytes {
		return nil, fmt.Errorf("hcs: a %s disk is too small for Windows; give it at least %s", units.FormatBytes(size), units.FormatBytes(minDiskBytes))
	}
	user := spec.Options["user"]
	if user == "" {
		user = defaultUser
	}
	password := spec.Options["password"]
	if password == "" {
		password = user
	}

	// The edition, read from the ISO before anything is written or booted.
	isoFile, err := os.Open(spec.Media)
	if err != nil {
		return nil, err
	}
	defer isoFile.Close()
	iso, err := udf.Open(isoFile)
	if err != nil {
		return nil, fmt.Errorf("hcs: read %s: %w", spec.Media, err)
	}
	wimPath, image, err := retailImage(iso, spec.Edition)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(log, "hcs: image: %s index %d (%s, %s)\n", wimPath, image.Index, image.Name, image.EditionID)
	answer, err := answerFile(answers{Index: image.Index, WIM: wimPath, User: user, Password: password})
	if err != nil {
		return nil, err
	}
	files, err := mediumFiles(iso, answer, spec.Agent)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(dst.Dir, 0o755); err != nil {
		return nil, err
	}
	// The guest grows its dynamic disk as Setup writes; the smallest disk
	// hcs installs to is what must fit.
	if err := needSpace(dst.Dir, minDiskBytes); err != nil {
		return nil, err
	}
	work := filepath.Join(dst.Dir, "firstboot")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return nil, err
	}
	defer func() { _ = removeAll(work) }()

	started := time.Now()
	medium := filepath.Join(work, mediumName)
	if err := writeFile(medium, func(w io.Writer) error { return writeMedium(w, files) }); err != nil {
		return nil, fmt.Errorf("hcs: write the install medium: %w", err)
	}
	fmt.Fprintf(log, "hcs: wrote the install medium in %s\n", time.Since(started).Round(time.Second))
	disk := filepath.Join(dst.Dir, diskName)
	if err := createDynamic(disk, size); err != nil {
		return nil, err
	}
	// The ISO is the person's own; hcs resolves no release from it.
	return nil, firstBoot(ctx, spec.InstallSpec, dst.Dir, work, disk, []string{medium, spec.Media}, log)
}

// writeFile writes path through write, and removes it on failure.
func writeFile(path string, write func(io.Writer) error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	w := bufio.NewWriterSize(f, 1<<20)
	err = write(w)
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
	}
	return err
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

// firstBoot boots the new disk with the install's media, lets Windows Setup
// run to a logged-on desktop, and shuts it down through the agent. Its
// state files live in work, and go with it: the committed VM is the disk.
// Its NIC's record lives in dir's state.json until the VM is gone, so that
// Destroy of dir frees the NIC if this process is killed. Nothing hands a
// killed install's dir to Destroy yet: removing one is vm#165.
func firstBoot(ctx context.Context, spec machine.InstallSpec, dir, work, disk string, media []string, log io.Writer) error {
	if err := freshState(work); err != nil {
		return err
	}
	id := newID()
	paths := []string{dir, disk, work, filepath.Join(work, guestFileName), filepath.Join(work, stateFileName)}
	// Revoke whatever was granted, the person's ISO included: a grant that
	// fails partway leaves no entry behind.
	defer func() {
		for _, path := range append(paths, media...) {
			revokeVMAccess(id, path)
		}
	}()
	if err := grantAll(id, paths); err != nil {
		return err
	}
	for _, path := range media {
		// HCS opens the ISO as the VM, so its file must take an access entry
		// for it: NTFS, and the person's to change.
		if err := grantVMAccess(id, path); err != nil {
			return fmt.Errorf("hcs: give the VM read access to %s (%w); an ISO on FAT, exFAT, or a network share cannot take it, so copy it to an NTFS volume of your own", path, err)
		}
	}
	// The NIC is recorded in dir's state.json while the VM runs, so Destroy
	// frees it if this process is killed first. A committed VM keeps its disk
	// alone, so the record goes once the VM is gone.
	defer func() { _ = os.Remove(filepath.Join(dir, stateJSON)) }()
	nic, err := newNIC(dir, instanceState{})
	if err != nil {
		return err
	}
	fmt.Fprintln(log, "hcs: Windows Setup runs inside the VM now (copying Windows, then specialize and OOBE, with reboots)")
	m, err := startVM(id, vmConfig{
		Disk: disk, Media: media, GuestFile: filepath.Join(work, guestFileName), StateFile: filepath.Join(work, stateFileName),
		CPUs: spec.CPUs, Memory: spec.Memory, NIC: nic,
	})
	if err != nil {
		return err
	}
	defer m.shutdown()
	started := time.Now()
	forget, err := recordInstall(dir, m)
	if err != nil {
		return err
	}
	defer forget()
	client := guest.NewClient(func(ctx context.Context) (net.Conn, error) { return m.Dial(ctx, guest.AgentPort) })

	ctx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	if spec.GUI {
		// The agent's stream shows a logged-on desktop, and Setup has none
		// until its automatic logon.
		fmt.Fprintln(log, "hcs: the window opens at the first logon; Windows Setup before it has no desktop to show")
		go showInstall(ctx, m, log)
	}
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
		_, _ = machine.Run(pctx, client, machine.ExecRequest{Argv: argv}, nil, &out, io.Discard)
		return out.String()
	}
	report := time.Now().Add(time.Minute)
	stage := "the agent"
	settled := time.Time{}
	for {
		select {
		case <-m.Done():
			if err := m.Err(); err != nil {
				return fmt.Errorf("hcs: the guest stopped during Setup (waiting for %s): %w", stage, err)
			}
			return fmt.Errorf("hcs: the guest stopped during Setup (waiting for %s)", stage)
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
	for field := range strings.FieldsSeq(out) {
		if strings.HasPrefix(field, "IMAGE_STATE_") {
			return field
		}
	}
	return "starting"
}
