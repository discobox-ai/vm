// Package converge makes a guest match state files: the packages, files,
// services, and guarded scripts they declare. It runs on the client, with the
// state files and their sources on the client's filesystem, and reaches the
// guest only through what every machine offers, exec and copy, so a guest
// needs nothing installed to be converged and the logic is always the
// client's version. The guest holds the record of what was applied, so any
// client applies only a difference. See docs/state-file.md.
package converge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// State is one state file.
type State struct {
	Packages Packages `yaml:"packages,omitempty"`
	Files    []File   `yaml:"files,omitempty"`
	Services Services `yaml:"services,omitempty"`
	Exec     []Exec   `yaml:"exec,omitempty"`
}

// Packages is the packages to have installed, by provider, in the order the
// state file names the providers.
type Packages []ProviderPackages

// ProviderPackages is one provider's packages, each NAME or NAME plus the
// provider's own way to pin a version (apt's NAME=VERSION, npm's NAME@VERSION).
type ProviderPackages struct {
	Provider string
	Names    []string
}

// UnmarshalYAML reads a mapping of provider to names and keeps its order,
// which a Go map would lose.
func (p *Packages) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("packages is a mapping of provider to package names")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		entry := ProviderPackages{Provider: node.Content[i].Value}
		if err := node.Content[i+1].Decode(&entry.Names); err != nil {
			return fmt.Errorf("packages.%s: %w", entry.Provider, err)
		}
		*p = append(*p, entry)
	}
	return nil
}

// File is a managed path: exactly one of Source, Content, Link, or Directory.
type File struct {
	Path string `yaml:"path"`
	// Source is a file on the client, relative to the state file's
	// directory, or a directory, which is every file under it, each at the
	// same place under Path.
	Source string `yaml:"source,omitempty"`
	// Content is the file's bytes, inline.
	Content *string `yaml:"content,omitempty"`
	// Link makes the path a symbolic link to this target.
	Link string `yaml:"link,omitempty"`
	// Directory makes the path a directory.
	Directory bool `yaml:"directory,omitempty"`
	// Mode is octal, and defaults to 0644, or 0755 for a directory. A file
	// from a source directory with no Mode is 0755 if its source can be
	// executed.
	Mode string `yaml:"mode,omitempty"`
	// Owner is USER or USER:GROUP. A path made without one is root's.
	Owner string `yaml:"owner,omitempty"`
}

// Services is the units to hold in each state.
type Services struct {
	Enabled  []string `yaml:"enabled,omitempty"`
	Disabled []string `yaml:"disabled,omitempty"`
	Masked   []string `yaml:"masked,omitempty"`
}

// Exec is a script that runs once for each Key and Run it has had.
type Exec struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key,omitempty"`
	Run  string `yaml:"run"`
	// Shell is the argv Run is appended to, in place of the guest's default
	// shell.
	Shell []string `yaml:"shell,omitempty"`
}

// The kinds of resource, in the order one state file's are applied: a package
// source and its key are files, so files are first, and a service starts
// last, with everything it reads in place.
const (
	KindFile    = "file"
	KindPackage = "package"
	KindExec    = "exec"
	KindService = "service"
)

// The states a service is held in.
const (
	Enabled  = "enabled"
	Disabled = "disabled"
	Masked   = "masked"
)

// Resource is one thing a guest is to have. ID names it across state files
// and runs, so a later state file that declares it again replaces the earlier
// declaration, and Digest is its declaration and content: a resource is
// applied again when its digest is not the one recorded for its ID.
type Resource struct {
	Kind   string
	ID     string
	Digest string

	// File is a file resource's declaration, and SourcePath its source on
	// the client when it has one.
	File       File
	SourcePath string
	Mode       fs.FileMode
	// Provider and Package are a package resource's.
	Provider, Package string
	// Unit and UnitState are a service resource's.
	Unit, UnitState string
	// Exec is an exec resource's.
	Exec Exec
}

// StateFile is one state file's resources, in the order they are applied:
// files, packages, exec, then services.
type StateFile struct {
	// Name is the file's base name, which orders it among state files and
	// names it in the guest's record.
	Name      string
	Resources []Resource
	// Digest is all of it.
	Digest string
}

// Desired is the state files a run was given, in name order.
type Desired struct {
	Files []StateFile
}

// Load reads state files: each path is one NAME.yaml, or a directory, which
// is every NAME.yaml in it. They are ordered by name wherever they came from,
// so a later one can need what an earlier one installed, and a resource a
// later one declares again is the later one's alone.
func Load(paths ...string) (*Desired, error) {
	var files []string
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if filepath.Ext(path) != ".yaml" {
				return nil, fmt.Errorf("%s is not a state file (NAME.yaml) or a directory of them", path)
			}
			files = append(files, path)
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() && filepath.Ext(entry.Name()) == ".yaml" {
				files = append(files, filepath.Join(path, entry.Name()))
			}
		}
	}
	slices.SortStableFunc(files, func(a, b string) int { return strings.Compare(filepath.Base(a), filepath.Base(b)) })

	desired := &Desired{}
	// Where each resource was declared: its state file, and its place there.
	type declaration struct{ file, index int }
	declared := map[string]declaration{}
	for _, path := range files {
		name := filepath.Base(path)
		if n := len(desired.Files); n > 0 && desired.Files[n-1].Name == name {
			return nil, fmt.Errorf("two state files are named %s", name)
		}
		resources, err := loadFile(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		at := len(desired.Files)
		for i, r := range resources {
			if earlier, ok := declared[r.ID]; ok {
				if earlier.file == at {
					return nil, fmt.Errorf("%s: %s is declared twice", name, r.ID)
				}
				// A later state file's declaration is the resource, as a
				// later image layer's file is the file.
				desired.Files[earlier.file].Resources[earlier.index].ID = ""
			}
			declared[r.ID] = declaration{file: at, index: i}
		}
		desired.Files = append(desired.Files, StateFile{Name: name, Resources: resources})
	}
	for i := range desired.Files {
		file := &desired.Files[i]
		file.Resources = slices.DeleteFunc(file.Resources, func(r Resource) bool { return r.ID == "" })
		h := sha256.New()
		for _, r := range file.Resources {
			fmt.Fprintf(h, "%s\x00%s\x00", r.ID, r.Digest)
		}
		file.Digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	}
	return desired, nil
}

// Digest is every state file given, for a cache key.
func (d *Desired) Digest() string {
	fields := make([]string, 0, 2*len(d.Files))
	for _, file := range d.Files {
		fields = append(fields, file.Name, file.Digest)
	}
	return digest(fields...)
}

// loadFile reads one state file strictly: an unknown key is an error, as it
// is in a build spec.
func loadFile(path string) ([]Resource, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: a state file in the root-only directory the converger is pointed at
	if err != nil {
		return nil, err
	}
	var state State
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&state); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	sources := filepath.Dir(path)
	var out []Resource
	for i, file := range state.Files {
		resources, err := fileResources(file, sources)
		if err != nil {
			return nil, fmt.Errorf("files[%d]: %w", i, err)
		}
		out = append(out, resources...)
	}
	for _, provider := range state.Packages {
		for _, name := range provider.Names {
			if name == "" {
				return nil, fmt.Errorf("packages.%s: an empty package name", provider.Provider)
			}
			out = append(out, Resource{
				Kind:     KindPackage,
				ID:       KindPackage + ":" + provider.Provider + "/" + packageName(name),
				Digest:   digest(provider.Provider, name),
				Provider: provider.Provider,
				Package:  name,
			})
		}
	}
	for i, e := range state.Exec {
		if e.Name == "" || e.Run == "" {
			return nil, fmt.Errorf("exec[%d]: name and run are required", i)
		}
		out = append(out, Resource{
			Kind: KindExec, ID: KindExec + ":" + e.Name, Exec: e,
			Digest: digest(append([]string{e.Key, e.Run}, e.Shell...)...),
		})
	}
	// What is turned off goes first: a socket cannot be started while the
	// service it would start is running on its own.
	for _, group := range []struct {
		state string
		units []string
	}{{Disabled, state.Services.Disabled}, {Masked, state.Services.Masked}, {Enabled, state.Services.Enabled}} {
		for _, unit := range group.units {
			if unit == "" {
				return nil, fmt.Errorf("services.%s: an empty unit name", group.state)
			}
			out = append(out, Resource{
				Kind: KindService, ID: KindService + ":" + unit, Digest: digest(group.state),
				Unit: unit, UnitState: group.state,
			})
		}
	}
	return out, nil
}

// packageName is a package without its version pin, so that two pins of one
// package are the same resource declared twice. An npm scope's leading @ is
// part of the name.
func packageName(spec string) string {
	if i := strings.IndexAny(spec[1:], "=@"); i >= 0 {
		return spec[:i+1]
	}
	return spec
}

// guestAbs reports whether a path is absolute in a guest, which need not be
// this process's OS: a Windows guest's has a drive letter.
func guestAbs(path string) bool {
	if strings.HasPrefix(path, "/") {
		return true
	}
	return len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/') &&
		(path[0] >= 'A' && path[0] <= 'Z' || path[0] >= 'a' && path[0] <= 'z')
}

// fileResources is a declared file's resources: itself, or, when its source
// is a directory, one for every file under that directory. Each of a
// directory's files is a resource of its own, so one that leaves the
// directory is deleted as a file that leaves the state files is.
func fileResources(file File, sources string) ([]Resource, error) {
	if !guestAbs(file.Path) {
		return nil, fmt.Errorf("path %q is not absolute", file.Path)
	}
	kinds := 0
	for _, set := range []bool{file.Source != "", file.Content != nil, file.Link != "", file.Directory} {
		if set {
			kinds++
		}
	}
	if kinds != 1 {
		return nil, fmt.Errorf("%s: a file is exactly one of source, content, link, or directory", file.Path)
	}
	mode := fs.FileMode(0o644)
	if file.Directory {
		mode = 0o755
	}
	if file.Mode != "" {
		parsed, err := strconv.ParseUint(file.Mode, 8, 12)
		if err != nil {
			return nil, fmt.Errorf("%s: mode %q is not octal", file.Path, file.Mode)
		}
		mode = fs.FileMode(parsed)
	}
	if file.Source == "" {
		content := ""
		if file.Content != nil {
			content = digest(*file.Content)
		}
		return []Resource{fileResource(file, mode, "", content)}, nil
	}
	// Anywhere on the client: the state file and what it names are the
	// client's own tree.
	source := filepath.Join(sources, filepath.FromSlash(file.Source))
	info, err := os.Stat(source)
	if err != nil {
		return nil, fmt.Errorf("%s: source: %w", file.Path, err)
	}
	if !info.IsDir() {
		sum, err := digestFile(source)
		if err != nil {
			return nil, fmt.Errorf("%s: source: %w", file.Path, err)
		}
		return []Resource{fileResource(file, mode, source, sum)}, nil
	}
	// The guest's separator, which is not this process's when a Windows
	// guest's state files are read on another OS.
	separator := "/"
	if strings.Contains(file.Path, `\`) {
		separator = `\`
	}
	var out []Resource
	err = filepath.WalkDir(source, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		each := file
		each.Path = strings.TrimRight(file.Path, `/\`) + separator + strings.ReplaceAll(filepath.ToSlash(rel), "/", separator)
		each.Source = file.Source + "/" + filepath.ToSlash(rel)
		if d.Type()&fs.ModeSymlink != 0 {
			each.Source = ""
			if each.Link, err = os.Readlink(path); err != nil {
				return err
			}
			out = append(out, fileResource(each, mode, "", ""))
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		eachMode := mode
		if file.Mode == "" && info.Mode()&0o111 != 0 {
			eachMode = 0o755
		}
		sum, err := digestFile(path)
		if err != nil {
			return err
		}
		out = append(out, fileResource(each, eachMode, path, sum))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s: source: %w", file.Path, err)
	}
	return out, nil
}

func fileResource(file File, mode fs.FileMode, sourcePath, content string) Resource {
	return Resource{
		Kind: KindFile, ID: KindFile + ":" + file.Path, File: file, Mode: mode, SourcePath: sourcePath,
		Digest: digest(file.Path, content, file.Link, strconv.FormatBool(file.Directory), mode.String(), file.Owner),
	}
}

func digestFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a source beside a state file, kept inside its directory by fileResource
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil {
		return "", err
	} else if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// digest hashes a resource's fields, unambiguously.
func digest(fields ...string) string {
	data, err := json.Marshal(fields)
	if err != nil {
		panic(err) // strings; cannot fail
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
