package agent

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// A disco-vm built for a host that is not Linux carries the linux agent of
// its own build (ADR 26-11-14-352 §1), in a directory of two files per
// platform: <goos>-<arch>.gz, the agent gzipped, and <goos>-<arch>.sha256,
// its SHA-256 in hex, which names it in the cache, so a command finds it
// written out without reading the rest. Carry writes them; Unpack writes the
// agent out.

func carriedName(goos, arch string) string { return goos + "-" + arch }

// Carry writes the goos/arch agent at path (relative to the working
// directory, or absolute) into dir as a disco-vm carries it, refusing one
// that is not a goos/arch executable (Check).
func Carry(dir, path, goos, arch string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := Check(path, goos, arch); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: the build's source tree
		return err
	}
	name := filepath.Join(dir, carriedName(goos, arch))
	f, err := os.Create(name + ".gz")
	if err != nil {
		return err
	}
	zw, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		return errors.Join(err, f.Close())
	}
	_, err = zw.Write(data)
	if err = errors.Join(err, zw.Close(), f.Close()); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	return os.WriteFile(name+".sha256", []byte(hex.EncodeToString(sum[:])+"\n"), 0o644) //nolint:gosec // G306: the build's source tree
}

// Carries reports whether carried holds a goos/arch agent. A nil carried
// holds none.
func Carries(carried fs.FS, goos, arch string) bool {
	if carried == nil {
		return false
	}
	_, err := fs.Stat(carried, carriedName(goos, arch)+".gz")
	return err == nil
}

// Unpack writes the goos/arch agent carried holds into
// cacheDir/agent/sha256-<its SHA-256>-<goos>-<arch>/disco-vm, beside the
// agents Pull pulls, unless it is there already, and returns that path,
// absolute; or "" when carried holds none. The cache is owner-only, as
// Pull's is. An agent that is not its recorded SHA-256, or not a goos/arch
// executable (Check), is refused.
func Unpack(cacheDir string, carried fs.FS, goos, arch string) (string, error) {
	if !Carries(carried, goos, arch) {
		return "", nil
	}
	name := carriedName(goos, arch)
	recorded, err := fs.ReadFile(carried, name+".sha256")
	if err != nil {
		return "", fmt.Errorf("the carried agent: %w", err)
	}
	digest := strings.TrimSpace(string(recorded))
	if b, err := hex.DecodeString(digest); err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("the carried agent: %s.sha256 is not a SHA-256", name)
	}
	entry := "sha256-" + digest + "-" + goos + "-" + arch
	agent, err := cached(cacheDir, entry)
	if err != nil {
		return "", err
	}
	if Check(agent, goos, arch) == nil {
		return agent, nil
	}
	agent, err = place(cacheDir, entry, goos, arch, func(dst string) error {
		return unzip(carried, name+".gz", dst, digest)
	})
	if err != nil {
		return "", fmt.Errorf("the carried agent: %w", err)
	}
	return agent, nil
}

// unzip writes the gzipped file name of carried to dst, owner-only, and
// refuses it unless it is digest.
func unzip(carried fs.FS, name, dst, digest string) error {
	f, err := carried.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o700)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), zr) //nolint:gosec // G110: the agent is this binary's own, as large as it is
	if err = errors.Join(err, out.Close()); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("%s is sha256:%s, not the sha256:%s recorded", name, got, digest)
	}
	return nil
}
