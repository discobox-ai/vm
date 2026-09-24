// Package build turns a YAML build spec into an image: an OS installed from
// media, or an existing image, then layers of steps run inside the booted
// guest, each layer committed to the store and cached by its inputs.
//
// It is Dockerfile-shaped on purpose — a base, then ordered steps, cached so an
// unchanged prefix is never rebuilt — with two differences that OS images
// force. Steps are grouped into explicit layers, because a layer boundary is
// a full guest shutdown and disk commit rather than a cheap filesystem diff,
// so where the boundaries go is a decision the author makes. And steps can be
// conditioned on the guest OS, so one spec (the discobox base image) can
// describe the same toolset on Windows and macOS.
package build

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/discobox-ai/vm/pkg/machine"
)

// Spec is a build spec file.
type Spec struct {
	From From `yaml:"from"`
	// Args are build arguments, substituted as ${NAME} into the spec and
	// exported to run steps as environment variables. --build-arg overrides.
	Args map[string]string `yaml:"args,omitempty"`
	// Env is set for every run step.
	Env map[string]string `yaml:"env,omitempty"`
	// Shell runs run steps: the script is appended as the last argument. It
	// defaults per guest OS (PowerShell on Windows, /bin/sh -c elsewhere).
	Shell []string `yaml:"shell,omitempty"`
	// User runs run steps as this guest account instead of the agent's own
	// (root or SYSTEM); a step's own user wins.
	User string `yaml:"user,omitempty"`
	// Resources sizes the build VM; it does not affect the cache.
	Resources Resources `yaml:"resources,omitempty"`
	Layers    []Layer   `yaml:"layers,omitempty"`
	// LegacyName and LegacyTag are the spec's old name and tag, which `build
	// -t` replaced, as a Dockerfile has none. They are read only to refuse
	// them with that message rather than as unknown keys.
	LegacyName string `yaml:"name,omitempty"`
	LegacyTag  string `yaml:"tag,omitempty"`
}

// From is the build's base: exactly one of Image or Install.
type From struct {
	Image   string   `yaml:"image,omitempty"`
	Install *Install `yaml:"install,omitempty"`
}

// Install installs an OS from media into a new base layer.
type Install struct {
	OS machine.OS `yaml:"os"`
	// Media is a Windows ISO, a macOS IPSW, or "latest" where the driver can
	// fetch one. Relative paths are relative to the build context.
	Media   string `yaml:"media"`
	Edition string `yaml:"edition,omitempty"`
	Disk    string `yaml:"disk,omitempty"`
	// Options pass through to the driver untouched.
	Options map[string]string `yaml:"options,omitempty"`
}

// Resources sizes the build VM.
type Resources struct {
	CPUs   int    `yaml:"cpus,omitempty"`
	Memory string `yaml:"memory,omitempty"`
}

// Layer is a group of steps committed together.
type Layer struct {
	Name  string `yaml:"name"`
	When  *When  `yaml:"when,omitempty"`
	Steps []Step `yaml:"steps"`
}

// Step is one action in a booted guest: exactly one of Run, Copy, or Reboot.
type Step struct {
	Name string `yaml:"name,omitempty"`
	When *When  `yaml:"when,omitempty"`
	// Run is a script for the shell.
	Run   string   `yaml:"run,omitempty"`
	Shell []string `yaml:"shell,omitempty"`
	// Env adds to the spec's env for this step.
	Env     map[string]string `yaml:"env,omitempty"`
	Workdir string            `yaml:"workdir,omitempty"`
	User    string            `yaml:"user,omitempty"`
	// Copy puts context files into the guest.
	Copy *Copy `yaml:"copy,omitempty"`
	// Reboot restarts the guest, for installers that need one.
	Reboot bool `yaml:"reboot,omitempty"`
	// Timeout bounds the step; the default is an hour.
	Timeout string `yaml:"timeout,omitempty"`
}

// Copy puts a file or directory from the build context into a guest
// directory. A directory's contents land in Dst, as with Dockerfile COPY.
type Copy struct {
	Src string `yaml:"src"`
	Dst string `yaml:"dst"`
}

// When conditions a layer or step. Every set field must match.
type When struct {
	OS []machine.OS `yaml:"os,omitempty"`
}

// Matches reports whether the condition holds for a guest.
func (w *When) Matches(os machine.OS) bool {
	if w == nil || len(w.OS) == 0 {
		return true
	}
	for _, candidate := range w.OS {
		if candidate == os {
			return true
		}
	}
	return false
}

// UnmarshalYAML accepts `os: windows` as well as `os: [windows, darwin]`.
func (w *When) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		OS yaml.Node `yaml:"os"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	switch raw.OS.Kind {
	case 0:
	case yaml.ScalarNode:
		w.OS = []machine.OS{machine.OS(raw.OS.Value)}
	default:
		if err := raw.OS.Decode(&w.OS); err != nil {
			return err
		}
	}
	return nil
}

// Load reads a spec file strictly: an unknown key is an error, not a silently
// ignored typo.
func Load(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse reads a spec.
func Parse(data []byte) (*Spec, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var spec Spec
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("build spec: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}

var knownOS = map[machine.OS]bool{machine.Windows: true, machine.Darwin: true, machine.Linux: true}

// Validate checks the spec's shape. Values that depend on args are checked
// after substitution, when the build plans.
func (s *Spec) Validate() error {
	var errs []error
	if s.LegacyName != "" || s.LegacyTag != "" {
		errs = append(errs, errors.New("name and tag are not part of a spec; name the result with `disco-vm build -t NAME[:TAG]`"))
	}
	if (s.From.Image == "") == (s.From.Install == nil) {
		errs = append(errs, errors.New("from needs exactly one of image or install"))
	}
	if in := s.From.Install; in != nil {
		if !knownOS[in.OS] {
			errs = append(errs, fmt.Errorf("from.install.os %q is not windows, darwin, or linux", in.OS))
		}
		if in.Media == "" {
			errs = append(errs, errors.New("from.install.media is required"))
		}
	}
	names := map[string]bool{}
	for i, layer := range s.Layers {
		where := fmt.Sprintf("layers[%d]", i)
		if layer.Name == "" {
			errs = append(errs, fmt.Errorf("%s: name is required", where))
		} else if names[layer.Name] {
			errs = append(errs, fmt.Errorf("%s: name %q is used twice", where, layer.Name))
		}
		names[layer.Name] = true
		errs = append(errs, validateWhen(where, layer.When))
		if len(layer.Steps) == 0 {
			errs = append(errs, fmt.Errorf("%s (%s): no steps", where, layer.Name))
		}
		for j, step := range layer.Steps {
			errs = append(errs, step.validate(fmt.Sprintf("%s.steps[%d]", where, j)))
		}
	}
	return errors.Join(errs...)
}

func validateWhen(where string, w *When) error {
	if w == nil {
		return nil
	}
	for _, os := range w.OS {
		if !knownOS[os] {
			return fmt.Errorf("%s.when.os: %q is not windows, darwin, or linux", where, os)
		}
	}
	return nil
}

func (s Step) validate(where string) error {
	kinds := 0
	if s.Run != "" {
		kinds++
	}
	if s.Copy != nil {
		kinds++
		if s.Copy.Src == "" || s.Copy.Dst == "" {
			return fmt.Errorf("%s: copy needs src and dst", where)
		}
	}
	if s.Reboot {
		kinds++
	}
	if kinds != 1 {
		return fmt.Errorf("%s: a step is exactly one of run, copy, or reboot", where)
	}
	if s.Timeout != "" {
		if _, err := time.ParseDuration(s.Timeout); err != nil {
			return fmt.Errorf("%s: timeout: %w", where, err)
		}
	}
	return validateWhen(where, s.When)
}

var argPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expand substitutes ${NAME} for declared args only. Anything else — $env:PATH
// in PowerShell, $HOME or ${HOME} in sh — is left for the guest's shell, so
// scripts are written exactly as they would be run by hand.
func expand(s string, args map[string]string) string {
	return argPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1]
		if value, ok := args[name]; ok {
			return value
		}
		return match
	})
}

// resolveContext makes a context-relative path absolute and refuses one that
// leaves the context, so a spec cannot read the builder's whole disk.
func resolveContext(context, path string) (string, error) {
	if filepath.IsAbs(path) || filepath.VolumeName(path) != "" {
		return "", fmt.Errorf("%q must be relative to the build context", path)
	}
	full := filepath.Join(context, path)
	rel, err := filepath.Rel(context, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q is outside the build context", path)
	}
	return full, nil
}
