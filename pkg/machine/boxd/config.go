package boxd

import (
	"encoding/json"
	"errors"
	"net"

	"github.com/discobox-ai/vm/pkg/machine"
)

const (
	// AddrEnv is where the command line reads Config.Addr from.
	AddrEnv = "BOXD_GRPC_ADDR"
	// TokenURLEnv is where the command line reads Config.TokenURL from.
	TokenURLEnv = "BOXD_TOKEN_URL" //nolint:gosec // G101: an environment variable's name, not a credential
	// RegionEnv is where the command line reads Config.Region from.
	RegionEnv = "BOXD_REGION"

	defaultAddr     = "boxd.sh:9443"
	defaultTokenURL = "https://app.boxd.sh/api/v1/auth/token" //nolint:gosec // G101: an endpoint URL, not a credential
)

// errNoRegions refuses a region until the driver calls one (vm#68).
var errNoRegions = errors.New("boxd: configuration: region: this disco-vm calls boxd's home region alone; regions are vm#68's")

// Config is the boxd driver's configuration (machine.Config.DriverConfig,
// ADR 26-11-11-215): the cluster it calls.
type Config struct {
	// Addr is the cluster's home gRPC API, HOST:PORT, always over TLS.
	// Empty is boxd.sh's.
	Addr string `json:"addr,omitempty"`
	// TokenURL is where an API key is exchanged for a token. Empty is
	// boxd.sh's.
	TokenURL string `json:"tokenUrl,omitempty"`
	// Region picks the endpoint the home endpoint names for it, for the
	// calls that are the region's. Empty is the home region; another is
	// refused until the driver calls one (vm#68).
	Region string `json:"region,omitempty"`
}

// Definition describes the boxd driver and its configuration.
func Definition() machine.Definition {
	return machine.Definition{
		Description: "Linux VMs as boxd microVMs, in the cloud, from any host.",
		Base: machine.ConfigField{Label: "Base", Type: machine.ConfigString, Env: machine.BaseEnv,
			Description: "The boxd image a fresh VM is made from; empty is boxd's default image."},
		ConfigFields: []machine.ConfigField{
			{Key: "addr", Label: "gRPC Address", Type: machine.ConfigString, Env: AddrEnv, Default: defaultAddr, Advanced: true, Privileged: true,
				Description: "The boxd cluster's home gRPC API, as HOST:PORT, always over TLS; it is sent the key."},
			{Key: "tokenUrl", Label: "Token URL", Type: machine.ConfigString, Env: TokenURLEnv, Default: defaultTokenURL, Advanced: true, Privileged: true,
				Description: "Where the cluster exchanges an API key for a token; it is sent the key, so it is https, or http to a loopback address."},
			{Key: "region", Label: "Region", Type: machine.ConfigString, Env: RegionEnv,
				Description: "The boxd region the driver's machines and snapshots are in. Only the home region (empty) works yet: another is refused until the driver calls a region's endpoint (vm#68)."},
		},
	}
}

func decode(config json.RawMessage) (Config, error) {
	cfg, err := machine.DecodeConfig[Config]("boxd", config)
	if err != nil {
		return cfg, err
	}
	if cfg.Addr == "" {
		cfg.Addr = defaultAddr
	}
	if cfg.TokenURL == "" {
		cfg.TokenURL = defaultTokenURL
	}
	if host, port, err := net.SplitHostPort(cfg.Addr); err != nil || host == "" || port == "" {
		return cfg, errors.New("boxd: configuration: addr: not HOST:PORT")
	}
	if err := machine.CheckKeyURL("boxd", "tokenUrl", cfg.TokenURL); err != nil {
		return cfg, err
	}
	if cfg.Region != "" {
		return cfg, errNoRegions
	}
	return cfg, nil
}

// Validate checks the boxd driver's configuration.
func Validate(config json.RawMessage) error {
	_, err := decode(config)
	return err
}
