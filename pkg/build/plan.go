package build

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/units"
	"github.com/discobox-ai/vm/pkg/machine"
)

// Plan is a spec with args substituted, OS conditions applied, and context
// paths resolved: exactly what will run, and therefore exactly what the cache
// keys hash.
type Plan struct {
	Ref     string
	GuestOS machine.OS
	// BaseImage is the from.image reference, or empty for an install.
	BaseImage string
	Install   *Install
	// MediaPath is the install media on the host, or "" when Media is not a
	// file (such as "latest").
	MediaPath string
	CPUs      int
	Memory    uint64
	Layers    []PlannedLayer
	// Skipped names layers whose `when` excluded them, for the build log.
	Skipped []string
}

// PlannedLayer is one layer to build.
type PlannedLayer struct {
	Name  string
	Steps []PlannedStep
}

// PlannedStep is one step, fully resolved.
type PlannedStep struct {
	Name string `json:"-"`
	Kind string `json:"kind"`
	// Argv is the shell plus the script, for run steps.
	Argv    []string `json:"argv,omitempty"`
	Env     []string `json:"env,omitempty"`
	Workdir string   `json:"workdir,omitempty"`
	User    string   `json:"user,omitempty"`
	// CopySrc is the host path; CopyDigest is what the cache sees of it.
	CopySrc    string        `json:"-"`
	CopyDigest string        `json:"copyDigest,omitempty"`
	CopyDst    string        `json:"copyDst,omitempty"`
	Timeout    time.Duration `json:"-"`
}

// Describe is the step's one-line summary for the build log.
func (s PlannedStep) Describe() string {
	if s.Name != "" {
		return s.Name
	}
	switch s.Kind {
	case "run":
		script := strings.TrimSpace(s.Argv[len(s.Argv)-1])
		first, _, more := strings.Cut(script, "\n")
		first = strings.TrimPrefix(first, windowsPrelude)
		if more {
			first += " ..."
		}
		return "run: " + first
	case "copy":
		return fmt.Sprintf("copy: %s -> %s", s.CopySrc, s.CopyDst)
	default:
		return s.Kind
	}
}

// DefaultShell is the shell run steps use on a guest OS when the spec names
// none. macOS gets a login zsh, so path_helper and a user's `brew shellenv`
// put what earlier steps installed on PATH.
func DefaultShell(os machine.OS) []string {
	switch os {
	case machine.Windows:
		return []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command"}
	case machine.Darwin:
		return []string{"/bin/zsh", "-l", "-c"}
	default:
		return []string{"/bin/sh", "-c"}
	}
}

// windowsPrelude makes the default Windows shell fail a step on the first
// error, the way `sh -e` users expect, and silences the progress bars that
// otherwise flood a non-interactive log.
const windowsPrelude = "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue'; "

// plan resolves a spec for a guest OS. The caller has already resolved the
// base, which is where the guest OS comes from for a from.image build.
func plan(spec *Spec, args map[string]string, contextDir string, guestOS machine.OS) (*Plan, error) {
	p := &Plan{Ref: spec.Ref(), GuestOS: guestOS, CPUs: spec.Resources.CPUs}
	memory, err := units.ParseBytes(expand(spec.Resources.Memory, args))
	if err != nil {
		return nil, fmt.Errorf("resources.memory: %w", err)
	}
	p.Memory = memory

	baseShell := spec.Shell
	for _, layer := range spec.Layers {
		if !layer.When.Matches(guestOS) {
			p.Skipped = append(p.Skipped, layer.Name)
			continue
		}
		planned := PlannedLayer{Name: layer.Name}
		for _, step := range layer.Steps {
			if !step.When.Matches(guestOS) {
				continue
			}
			ps := PlannedStep{Name: expand(step.Name, args), Workdir: expand(step.Workdir, args), Timeout: time.Hour}
			if step.Timeout != "" {
				ps.Timeout, _ = time.ParseDuration(step.Timeout)
			}
			switch {
			case step.Run != "":
				ps.Kind = "run"
				shell := step.Shell
				if len(shell) == 0 {
					shell = baseShell
				}
				script := expand(step.Run, args)
				if len(shell) == 0 {
					shell = DefaultShell(guestOS)
					if guestOS == machine.Windows {
						script = windowsPrelude + script
					}
				}
				ps.Argv = append(expandAll(shell, args), script)
				ps.Env = environment(args, spec.Env, step.Env)
				ps.User = expand(step.User, args)
				if ps.User == "" {
					ps.User = expand(spec.User, args)
				}
			case step.Copy != nil:
				ps.Kind = "copy"
				src, err := resolveContext(contextDir, expand(step.Copy.Src, args))
				if err != nil {
					return nil, fmt.Errorf("layer %s: copy: %w", layer.Name, err)
				}
				digest, err := digestTree(src)
				if err != nil {
					return nil, fmt.Errorf("layer %s: copy: %w", layer.Name, err)
				}
				ps.CopySrc, ps.CopyDigest, ps.CopyDst = src, digest, expand(step.Copy.Dst, args)
			case step.Reboot:
				ps.Kind = "reboot"
			}
			planned.Steps = append(planned.Steps, ps)
		}
		if len(planned.Steps) == 0 {
			p.Skipped = append(p.Skipped, layer.Name)
			continue
		}
		p.Layers = append(p.Layers, planned)
	}
	return p, nil
}

func expandAll(in []string, args map[string]string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = expand(s, args)
	}
	return out
}

// environment is a run step's KEY=VALUE list: args, then the spec's env, then
// the step's, later ones winning. Sorted, so the cache key does not depend on
// map order.
func environment(args map[string]string, layers ...map[string]string) []string {
	merged := map[string]string{}
	for k, v := range args {
		merged[k] = v
	}
	for _, env := range layers {
		for k, v := range env {
			merged[k] = expand(v, args)
		}
	}
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// digestTree hashes a file or directory's names, modes, and contents.
// Timestamps are left out: touching a file does not change what it installs.
func digestTree(root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%o\x00", filepath.ToSlash(rel), info.Mode())
		switch {
		case info.Mode().IsRegular():
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(h, f); err != nil {
				return err
			}
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			h.Write([]byte(link))
		}
		h.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// keyVersion changes whenever what a key covers changes, so an old store's
// layers are rebuilt rather than trusted under a new meaning.
const keyVersion = 1

func hashJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err) // plain structs of strings; cannot fail
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// layerKey is a layer's ID: its parent, the driver and guest OS it was built
// on, and its steps. Names and timeouts are left out; renaming a step does not
// change what it built.
func layerKey(parent, driver string, guestOS machine.OS, layer PlannedLayer, salt string) string {
	return hashJSON(struct {
		V       int           `json:"v"`
		Parent  string        `json:"parent"`
		Driver  string        `json:"driver"`
		GuestOS machine.OS    `json:"guestOS"`
		Steps   []PlannedStep `json:"steps"`
		Salt    string        `json:"salt,omitempty"`
	}{keyVersion, parent, driver, guestOS, layer.Steps, salt})
}

// installKey is a base layer's ID. The media is identified by path, size, and
// modification time rather than by hashing gigabytes of ISO on every build.
func installKey(driver string, install Install, mediaPath string, salt string) (string, error) {
	media := install.Media
	if mediaPath != "" {
		info, err := os.Stat(mediaPath)
		if err != nil {
			return "", fmt.Errorf("install media: %w", err)
		}
		media = fmt.Sprintf("%s|%d|%d", mediaPath, info.Size(), info.ModTime().UnixNano())
	}
	options := make([]string, 0, len(install.Options))
	for k, v := range install.Options {
		options = append(options, k+"="+v)
	}
	slices.Sort(options)
	return hashJSON(struct {
		V       int        `json:"v"`
		Driver  string     `json:"driver"`
		OS      machine.OS `json:"os"`
		Media   string     `json:"media"`
		Edition string     `json:"edition"`
		Disk    string     `json:"disk"`
		Options []string   `json:"options"`
		Salt    string     `json:"salt,omitempty"`
	}{keyVersion, driver, install.OS, media, install.Edition, install.Disk, options, salt}), nil
}

// expandMap substitutes args into a map's values, such as install options.
func expandMap(in map[string]string, args map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = expand(v, args)
	}
	return out
}
