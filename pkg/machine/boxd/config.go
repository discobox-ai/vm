package boxd

import (
	"encoding/json"
	"errors"
	"net"
	"regexp"

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

// regionID is the shape of a boxd region's ID: a short lowercase code. Which
// regions there are is boxd's, and is asked only once the driver calls one.
var regionID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Config is the boxd driver's configuration (machine.Config.DriverConfig,
// ADR 26-11-11-215): the cluster it calls.
type Config struct {
	// Addr is the cluster's home gRPC API, HOST:PORT, always over TLS.
	// Empty is boxd.sh's.
	Addr string `json:"addr,omitempty"`
	// TokenURL is where an API key is exchanged for a token. Empty is
	// boxd.sh's.
	TokenURL string `json:"tokenUrl,omitempty"`
	// Region picks the endpoint the home endpoint names for it
	// (ListRegions), for the calls that are the region's: every call on a
	// machine or a snapshot. Empty is the home endpoint's own. A region boxd
	// does not list, or one the key's org has not enabled, fails Check and
	// every such call: the list is boxd's, and is read only once it is used.
	Region string `json:"region,omitempty"`
}

// Definition describes the boxd driver and its configuration.
func Definition() machine.Definition {
	return machine.Definition{
		Description: "Linux VMs as boxd microVMs, in the cloud, from any host.",
		Base: &machine.ConfigField{Label: "Base", Type: machine.ConfigString, Env: machine.BaseEnv,
			Description: "The boxd image a fresh VM is made from; empty is boxd's default image."},
		ConfigFields: []machine.ConfigField{
			{Key: "addr", Label: "gRPC Address", Type: machine.ConfigString, Env: AddrEnv, Default: defaultAddr, Advanced: true, Privileged: true,
				Description: "The boxd cluster's home gRPC API, as HOST:PORT, always over TLS; it is sent the key."},
			{Key: "tokenUrl", Label: "Token URL", Type: machine.ConfigString, Env: TokenURLEnv, Default: defaultTokenURL, Advanced: true, Privileged: true,
				Description: "Where the cluster exchanges an API key for a token; it is sent the key, so it is https, or http to a loopback address."},
			{Key: "region", Label: "Region", Type: machine.ConfigString, Env: RegionEnv,
				Description: "The boxd region the driver's machines and snapshots are in, such as fra or nyc; the driver lists and makes only that region's. Empty is the home endpoint's own region."},
		},
		Secrets: []machine.SecretField{{
			Key: "apiKey", Label: "API Key", Type: machine.ConfigSecret, Env: []string{APIKeyEnv, TokenEnv}, Placeholder: "bxd_...",
			Description: "A boxd API key (bxd_...), from `boxd auth keys create`, which the driver exchanges for a token at tokenUrl; or a token, which it sends as it is.",
			Sends: []machine.SecretSend{
				// An API key goes only to the token exchange (client.token).
				{Prefix: apiKeyPrefix, Endpoint: "tokenUrl", Protocol: machine.SendHTTPS, Method: "POST", JSONField: "api_key"},
				// A token is every call's bearer (client.authorize), at the
				// home endpoint and the region's, which ListRegions names.
				{Endpoint: "addr", Listed: true, Protocol: machine.SendGRPC, Method: "POST", Path: "/" + serviceName + "/",
					Header: "authorization", ValuePrefix: "Bearer "},
			},
		}},
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
	if cfg.Region != "" && !regionID.MatchString(cfg.Region) {
		return cfg, errors.New("boxd: configuration: region: not a boxd region ID, such as fra or nyc")
	}
	return cfg, nil
}

// Validate checks the boxd driver's configuration.
func Validate(config json.RawMessage) error {
	_, err := decode(config)
	return err
}
