package vz

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isoEntry is a file the test reads back from an image's root directory.
type isoEntry struct {
	id, name string
	mode     uint32
	data     []byte
}

// readISO reads the root directory of an image writeISO made, as a reader
// that knows Rock Ridge does: the volume label, and each file's ISO 9660
// identifier, Rock Ridge name and mode, and data.
func readISO(t *testing.T, image []byte) (string, []isoEntry) {
	t.Helper()
	sector := func(n uint32) []byte { return image[int(n)*isoSector : int(n+1)*isoSector] }
	pvd := sector(16)
	if pvd[0] != 1 || string(pvd[1:6]) != "CD001" {
		t.Fatalf("no primary volume descriptor at sector 16")
	}
	if got := binary.LittleEndian.Uint32(pvd[80:]); int(got)*isoSector != len(image) {
		t.Fatalf("the descriptor says %d sectors, and the image is %d bytes", got, len(image))
	}
	if term := sector(17); term[0] != 255 || string(term[1:6]) != "CD001" {
		t.Fatalf("no terminator at sector 17")
	}
	root := pvd[156:]
	dir := sector(binary.LittleEndian.Uint32(root[2:]))
	var entries []isoEntry
	for off, i := 0, 0; off < len(dir) && dir[off] != 0; i++ {
		rec := dir[off : off+int(dir[off])]
		off += len(rec)
		idLen := int(rec[32])
		su := rec[33+idLen+(1-idLen%2):]
		if i == 0 && !bytes.HasPrefix(su, []byte{'S', 'P', 7, 1, 0xBE, 0xEF}) {
			t.Fatalf("the root's own record has no SUSP indicator: % x", su)
		}
		if i < 2 {
			continue
		}
		e := isoEntry{id: string(rec[33 : 33+idLen])}
		for len(su) >= 4 && su[2] >= 4 {
			field := su[:su[2]]
			switch string(field[:2]) {
			case "PX":
				e.mode = binary.LittleEndian.Uint32(field[4:])
			case "NM":
				e.name = string(field[5:])
			}
			su = su[su[2]:]
		}
		extent, size := binary.LittleEndian.Uint32(rec[2:]), binary.LittleEndian.Uint32(rec[10:])
		e.data = image[int(extent)*isoSector : int(extent)*isoSector+int(size)]
		entries = append(entries, e)
	}
	// The root's continuation area holds the Rock Ridge extension reference.
	if er := sector(21); string(er[:2]) != "ER" || string(er[8:18]) != rripID {
		t.Fatalf("no Rock Ridge extension reference in the continuation area: %q", er[:18])
	}
	return strings.TrimRight(string(pvd[40:72]), " "), entries
}

func TestWriteISO(t *testing.T) {
	agent := bytes.Repeat([]byte("agent"), 1000)
	files := []isoFile{
		{Name: "disco-vm.sha256", Mode: 0o644, Data: []byte("abc\n")},
		{Name: "disco-vm", Mode: 0o755, Data: agent},
	}
	var a, b bytes.Buffer
	if err := writeISO(&a, driveLabel, files); err != nil {
		t.Fatal(err)
	}
	if err := writeISO(&b, driveLabel, files); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("the same files gave different images")
	}
	if a.Len()%isoSector != 0 {
		t.Fatalf("the image is %d bytes, not whole sectors", a.Len())
	}
	label, entries := readISO(t, a.Bytes())
	if label != driveLabel {
		t.Fatalf("label %q", label)
	}
	want := []isoEntry{
		{id: "DISCO_VM.;1", name: "disco-vm", mode: 0o100755, data: agent},
		{id: "DISCO_VM.SHA256;1", name: "disco-vm.sha256", mode: 0o100644, data: []byte("abc\n")},
	}
	if len(entries) != len(want) {
		t.Fatalf("%d files, want %d", len(entries), len(want))
	}
	for i, e := range entries {
		w := want[i]
		if e.id != w.id || e.name != w.name || e.mode != w.mode || !bytes.Equal(e.data, w.data) {
			t.Errorf("file %d is %s %q %o (%d bytes), want %s %q %o (%d bytes)", i, e.id, e.name, e.mode, len(e.data), w.id, w.name, w.mode, len(w.data))
		}
	}
	for _, bad := range []struct {
		label string
		files []isoFile
	}{
		{"lower", nil},
		{driveLabel, []isoFile{{Name: "a/b"}}},
		{driveLabel, []isoFile{{Name: "a-b"}, {Name: "a_b"}}},
	} {
		if err := writeISO(io.Discard, bad.label, bad.files); err == nil {
			t.Errorf("wrote an image labeled %q of %v", bad.label, bad.files)
		}
	}
}

func TestAgentDrive(t *testing.T) {
	agent := filepath.Join(t.TempDir(), "disco-vm")
	if err := os.WriteFile(agent, []byte("an agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("an agent"))
	cache := t.TempDir()
	iso, err := agentDrive(cache, agent)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cache, "agent", hex.EncodeToString(sum[:])+".iso"); iso != want {
		t.Fatalf("the drive is %s, want %s", iso, want)
	}
	image, err := os.ReadFile(iso)
	if err != nil {
		t.Fatal(err)
	}
	_, entries := readISO(t, image)
	if len(entries) != 2 || string(entries[0].data) != "an agent" || string(entries[1].data) != hex.EncodeToString(sum[:])+"\n" {
		t.Fatalf("the drive holds %+v", entries)
	}
	// Windows reports no owner-only mode.
	if info, err := os.Stat(iso); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("the drive's mode: %v, %v", info, err)
	}

	// A drive whose content is not its recorded digest is built again.
	if err := os.WriteFile(iso, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if again, err := agentDrive(cache, agent); err != nil || again != iso {
		t.Fatalf("%s, %v", again, err)
	}
	if rebuilt, err := os.ReadFile(iso); err != nil || !bytes.Equal(rebuilt, image) {
		t.Fatalf("a damaged drive was not built again: %v", err)
	}
	// A drive in the cache is used as it is.
	before, _ := os.Stat(iso)
	if _, err := agentDrive(cache, agent); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(iso); !os.SameFile(before, after) {
		t.Fatal("a drive in the cache was written again")
	}
}

// TestStub runs the launch daemon's stub with stand-ins for macOS's mount
// and diskutil: it copies the agent off the mounted drive, checks it, and
// runs it as the agent; it asks Disk Arbitration to mount a drive that is
// not; and it refuses an agent whose digest is not the drive's.
func TestStub(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub is a macOS guest's /bin/sh script")
	}
	for _, c := range []struct {
		name        string
		mounted     bool
		badDigest   bool
		wantCode    int
		wantOut     string
		wantDiskCmd string
	}{
		{name: "mounted", mounted: true, wantOut: "agent ran: guest", wantDiskCmd: "unmount"},
		{name: "mounted by the stub", wantOut: "agent ran: guest", wantDiskCmd: "mount readOnly"},
		{name: "a copy that is not the drive's", mounted: true, badDigest: true, wantCode: 1, wantOut: "the drive says"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			mnt, run, bin, dev := filepath.Join(dir, "mnt"), filepath.Join(dir, "run"), filepath.Join(dir, "bin"), filepath.Join(dir, "dev")
			for _, d := range []string{mnt, bin, dev} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			agent := "#!/bin/sh\necho \"agent ran: $*\"\n"
			sum := sha256.Sum256([]byte(agent))
			digest := hex.EncodeToString(sum[:])
			if c.badDigest {
				digest = strings.Repeat("0", 64)
			}
			write := func(path, data string, mode os.FileMode) {
				t.Helper()
				if err := os.WriteFile(path, []byte(data), mode); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(mnt, driveAgent), agent, 0o755)
			write(filepath.Join(mnt, driveDigest), digest+"\n", 0o644)
			for _, d := range []string{"disk0", "disk0s1", "disk1"} {
				write(filepath.Join(dev, d), "", 0o644)
			}
			state := filepath.Join(dir, "mounted")
			if c.mounted {
				write(state, "", 0o644)
			}
			log := filepath.Join(dir, "diskutil.log")
			// mount lists the drive once it is mounted; diskutil names
			// disk1's volume as the drive's, mounts it, and logs each call.
			write(filepath.Join(bin, "mount"), `#!/bin/sh
[ -e `+state+` ] && echo "/dev/disk1 on `+mnt+` (cd9660, local, nodev, nosuid, read-only, noowners)"
echo "/dev/disk3s1 on / (apfs, local, read-only, journaled)"
`, 0o755)
			write(filepath.Join(bin, "diskutil"), `#!/bin/sh
echo "$*" >> `+log+`
case "$1 $2" in
"info "*/disk1) echo "   Volume Name:               `+driveLabel+`" ;;
"info "*) echo "   Volume Name:               Macintosh HD" ;;
"mount readOnly") [ "${3##*/}" = disk1 ] && touch `+state+` ;;
esac
`, 0o755)

			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", stubScript(mnt, filepath.Join(dev, "disk[0-9]*"), run)) //nolint:gosec // G204: the stub under test, in this test's directories
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				code = cmd.ProcessState.ExitCode()
			}
			if code != c.wantCode || !strings.Contains(string(out), c.wantOut) {
				t.Fatalf("exit %d, %q; want exit %d, %q", code, out, c.wantCode, c.wantOut)
			}
			calls, _ := os.ReadFile(log)
			if !strings.Contains(string(calls), c.wantDiskCmd) {
				t.Fatalf("diskutil was called %q, want %q", calls, c.wantDiskCmd)
			}
			if strings.Contains(string(calls), "disk0s1") {
				t.Fatalf("the stub asked about a partition: %q", calls)
			}
			copied, err := os.Stat(filepath.Join(run, "disco-vm"))
			if c.wantCode != 0 {
				if err == nil {
					t.Fatal("an agent that is not the drive's was kept")
				}
				return
			}
			if err != nil || copied.Mode().Perm() != 0o755 {
				t.Fatalf("the copy: %v, %v", copied, err)
			}
			if info, err := os.Stat(run); err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("the copy's directory: %v, %v", info, err)
			}
		})
	}
}

// TestDaemonPlist reads the launch daemon back as XML: its program is
// /bin/sh, given the stub whole.
func TestDaemonPlist(t *testing.T) {
	dec := xml.NewDecoder(strings.NewReader(daemonPlist()))
	dec.Strict = false
	var strs []string
	inString := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			inString = tok.Name.Local == "string"
		case xml.EndElement:
			inString = false
		case xml.CharData:
			if inString {
				strs = append(strs, string(tok))
			}
		}
	}
	want := []string{daemonID, "/bin/sh", "-c", stubScript("/Volumes/"+driveLabel, "/dev/disk[0-9]*", agentRunDir), "/var/log/" + daemonID + ".log", "/var/log/" + daemonID + ".log"}
	if len(strs) != len(want) {
		t.Fatalf("the plist's strings are %q", strs)
	}
	for i := range want {
		if strs[i] != want[i] {
			t.Errorf("string %d is %q, want %q", i, strs[i], want[i])
		}
	}
}

func TestCheckMacOSAgent(t *testing.T) {
	dir := t.TempDir()
	machO := func(name string, cpu, fileType uint32) string {
		header := make([]byte, 32)
		binary.LittleEndian.PutUint32(header, 0xfeedfacf)
		binary.LittleEndian.PutUint32(header[4:], cpu)
		binary.LittleEndian.PutUint32(header[12:], fileType)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, header, 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	const arm64, amd64, exec, dylib = 0x0100000c, 0x01000007, 2, 6
	if err := checkMacOSAgent(machO("ok", arm64, exec), "arm64"); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"none":           "",
		"not Mach-O":     filepath.Join(dir, "missing"),
		"a library":      machO("dylib", arm64, dylib),
		"another arch":   machO("amd64", amd64, exec),
		"a shell script": filepath.Join(dir, "script"),
	} {
		_ = os.WriteFile(filepath.Join(dir, "script"), []byte("#!/bin/sh\n"), 0o755)
		if err := checkMacOSAgent(path, "arm64"); err == nil || !strings.Contains(err.Error(), "macosAgent") {
			t.Errorf("%s: %v, want a refusal naming macosAgent", name, err)
		}
	}
}
