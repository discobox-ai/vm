package linuximage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// AgentRepository is where disco-vm publishes its linux agent, the static
// linux disco-vm a host hands a Linux guest at every boot: one image for
// linux/amd64 and linux/arm64 per module version, tagged with that version
// (v1.2.3, or a pseudo-version for a commit on main), so an embedder pins
// the digest of the agent of the module it links (discobox ADR 26-11-11-963
// §10) and pulls it with PullAgent.
const AgentRepository = "ghcr.io/discobox-ai/disco-vm-agent"

// AgentFile is the agent in an agent image, at its root.
const AgentFile = "disco-vm"

// PullAgent pulls the linux/arch agent of the agent image ref pins by
// digest into cacheDir/agent/<digest>-<arch>/disco-vm, unless it is there
// already, and returns that path, absolute (a relative cacheDir is taken
// from the working directory). The cache is owner-only: the agent runs in
// every guest. A cached agent is used with no network, since a digest names
// one image for good. A reference that is not by digest, an image for
// another platform, and one whose disco-vm is not a linux/arch executable
// are refused, naming the image.
func PullAgent(ctx context.Context, cacheDir, ref, arch string) (string, error) {
	d, err := name.NewDigest(ref)
	if err != nil {
		return "", fmt.Errorf("agent image %q is not pinned by digest: %w", ref, err)
	}
	hash, err := v1.NewHash(d.DigestStr())
	if err != nil {
		return "", fmt.Errorf("agent image %q: %w", ref, err)
	}
	// CheckAgent takes an absolute path alone.
	if cacheDir, err = filepath.Abs(cacheDir); err != nil {
		return "", err
	}
	root := filepath.Join(cacheDir, "agent")
	dir := filepath.Join(root, hash.Algorithm+"-"+hash.Hex+"-"+arch)
	agent := filepath.Join(dir, AgentFile)
	if CheckAgent(agent, arch) == nil {
		return agent, nil
	}
	platform := v1.Platform{OS: "linux", Architecture: arch}
	desc, err := remote.Get(d, remote.WithContext(ctx), remote.WithPlatform(platform), remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return "", fmt.Errorf("agent image %s: %w", ref, err)
	}
	img, err := platformImage(desc, "agent image", ref, arch)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	// MkdirAll leaves a directory that was there as it was.
	if err := os.Chmod(root, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(root, ".pull-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if err := extract(mutate.Extract(img), tmp, []string{AgentFile}, 0o700); err != nil {
		return "", fmt.Errorf("agent image %s: %w", ref, err)
	}
	if _, err := os.Stat(filepath.Join(tmp, AgentFile)); err != nil {
		return "", fmt.Errorf("agent image %s holds no %s", ref, AgentFile)
	}
	if err := CheckAgent(filepath.Join(tmp, AgentFile), arch); err != nil {
		return "", fmt.Errorf("agent image %s: %w", ref, err)
	}
	// A broken pull of the same digest is replaced; a good one, which a
	// concurrent pull may have just returned and a boot be reading, is not.
	if CheckAgent(agent, arch) == nil {
		return agent, nil
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(tmp, dir); err != nil {
		// Another pull of the same digest won.
		if CheckAgent(agent, arch) == nil {
			return agent, nil
		}
		return "", err
	}
	return agent, nil
}
