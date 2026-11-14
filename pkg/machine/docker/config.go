package docker

import (
	"encoding/json"
	"errors"
	"path"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/vm/pkg/machine"
)

// HostEnv is where the command line reads Config.Host from, as the docker
// CLI reads it.
const HostEnv = "DOCKER_HOST"

const defaultHost = "unix:///var/run/docker.sock"

// AgentEnv is where the command line reads Config.Agent from.
const AgentEnv = "DISCO_VM_AGENT"

// Config is the docker driver's configuration (machine.Config.DriverConfig,
// ADR 26-11-11-215).
type Config struct {
	// Host is the Docker daemon (HostDaemon): unix://PATH or
	// tcp://HOST:PORT without TLS. Empty is defaultHost.
	Host string `json:"host,omitempty"`
	// Root is where the driver keeps volumes and caches on the daemon's
	// host (ADR 26-11-03-782). Empty is DefaultRoot.
	Root string `json:"root,omitempty"`
	// Agent is the disco-vm binary, built for linux and the daemon's CPU,
	// that Create puts in a base's image, as an absolute path. The command
	// line sets it to itself on a linux host, and to the one it carries on
	// another (ADR 26-11-14-352); the driver falls back to nothing.
	Agent string `json:"agent,omitempty"`
}

// Definition describes the docker driver and its configuration.
func Definition() machine.Definition {
	return machine.Definition{
		Description: "Linux VMs as Docker containers that boot systemd in a user namespace of their own, on a local or remote daemon.",
		Base: &machine.ConfigField{Label: "Base", Type: machine.ConfigString, Env: machine.BaseEnv, Placeholder: "debian:sid",
			Description: "The image a fresh VM is readied from; with none, a fresh VM is refused."},
		ConfigFields: []machine.ConfigField{
			{Key: "host", Label: "Docker Host", Type: machine.ConfigString, Env: HostEnv, Default: defaultHost, Privileged: true,
				Description: "The Docker daemon: unix://PATH, or tcp://HOST:PORT without TLS (the driver sends it no key). Docker contexts, ssh://, and TLS are not read."},
			{Key: "root", Label: "Root", Type: machine.ConfigString, Env: RootEnv, Default: DefaultRoot, Advanced: true, Privileged: true,
				Description: "The absolute directory on the daemon's host where the driver keeps VMs' data volumes and caches, which guests mount."},
			{Key: "agent", Label: "Agent", Type: machine.ConfigString, Env: AgentEnv, Advanced: true, Privileged: true, AgentOS: machine.Linux,
				Description: "The absolute path of the static linux disco-vm, for the daemon's CPU, that Create puts in a base's image; the command line sets it to itself on linux, and to the one it carries on another host."},
		},
	}
}

func decode(config json.RawMessage) (Config, error) {
	cfg, err := machine.DecodeConfig[Config]("docker", config)
	if err != nil {
		return cfg, err
	}
	if cfg.Host == "" {
		cfg.Host = defaultHost
	}
	if cfg.Root == "" {
		cfg.Root = DefaultRoot
	}
	if !strings.HasPrefix(cfg.Host, "unix://") && !strings.HasPrefix(cfg.Host, "tcp://") {
		return cfg, errors.New("docker: configuration: host: only unix:// and tcp:// (without TLS) are supported")
	}
	// The daemon's host is linux, whatever this one is.
	if !path.IsAbs(cfg.Root) {
		return cfg, errors.New("docker: configuration: root: not an absolute path")
	}
	if cfg.Agent != "" && !filepath.IsAbs(cfg.Agent) {
		return cfg, errors.New("docker: configuration: agent: not an absolute path")
	}
	return cfg, nil
}

// Validate checks the docker driver's configuration.
func Validate(config json.RawMessage) error {
	_, err := decode(config)
	return err
}
