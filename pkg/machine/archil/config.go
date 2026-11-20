package archil

import (
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/discobox-ai/vm/pkg/machine"
)

const (
	// RegionEnv is where the command line reads Config.Region from.
	RegionEnv = "ARCHIL_REGION"
	// BaseURLEnv is where the command line reads Config.BaseURL from.
	BaseURLEnv = "ARCHIL_BASE_URL"
	// legacyRegion is where the driver called before its region was
	// required, so where a pool made then keeps its VMs.
	legacyRegion = "aws-us-east-1"
)

// Config is the archil driver's configuration (machine.Config.DriverConfig,
// ADR 26-11-11-215).
type Config struct {
	// Region is the region the driver calls: its control plane, its
	// sandboxes, and its disks. It is required, as boxd's is: a pool sees
	// only its region's VMs, so a default that changed would lose sight of
	// VMs still billed in the old one.
	Region string `json:"region,omitempty"`
	// BaseURL is a control plane in place of the region's, for a test. It
	// wins over Region's, and is sent the key.
	BaseURL string `json:"baseUrl,omitempty"`
}

// regions are the regions that run sandboxes.
func regions() []string { return slices.Sorted(maps.Keys(controlPlanes)) }

// Definition describes the archil driver and its configuration.
func Definition() machine.Definition {
	return machine.Definition{
		Description: "Linux VMs as Archil's Firecracker sandboxes, in the cloud, from any host.",
		Base: &machine.ConfigField{Label: "Base", Type: machine.ConfigString, Env: machine.BaseEnv, Format: machine.OCIImage, Placeholder: "debian:sid",
			Description: "The public OCI image a fresh VM runs; empty is Archil's default image."},
		ConfigFields: []machine.ConfigField{
			{Key: "region", Label: "Region", Type: machine.ConfigString, Env: RegionEnv, Required: true, Placeholder: legacyRegion,
				Description: "The Archil region the driver calls, which keeps its sandboxes and disks: one of " + strings.Join(regions(), ", ") + ". " + legacyRegion + " is where a pool made before it was required keeps its VMs."},
			{Key: "baseUrl", Label: "Base URL", Type: machine.ConfigString, Env: BaseURLEnv, Advanced: true, Privileged: true,
				Description: "A control plane's URL in place of the region's, for a test; it is sent the key, so it is https, or http to a loopback address."},
		},
		Secrets: []machine.SecretField{{
			Key: "apiKey", Label: "API Key", Type: machine.ConfigSecret, Env: []string{APIKeyEnv}, Placeholder: "key-...",
			Description: "An Archil API key, from the Archil console.",
			Sends: []machine.SecretSend{
				// Every API call's bearer (api.call), at baseUrl, or else
				// the region's control plane: any of them.
				{Endpoint: "baseUrl", Hosts: controlPlaneHosts(), Protocol: machine.SendHTTPS, Path: "/api/",
					Header: "Authorization", ValuePrefix: "Bearer key-", TrimPrefix: "key-"},
			},
		}},
	}
}

// controlPlaneHosts are the control planes' HOST:PORTs, sorted.
func controlPlaneHosts() []string {
	var hosts []string
	for _, region := range regions() {
		u, err := url.Parse(controlPlanes[region])
		if err != nil {
			panic(err)
		}
		hosts = append(hosts, net.JoinHostPort(u.Hostname(), "443"))
	}
	return hosts
}

func decode(config json.RawMessage) (Config, error) {
	cfg, err := machine.DecodeConfig[Config]("archil", config)
	if err != nil {
		return cfg, err
	}
	if cfg.Region == "" {
		return cfg, fmt.Errorf("archil: configuration: region is required: the Archil region the driver's sandboxes and disks are in, one of %s (%s keeps the VMs of a pool made before it was required)", strings.Join(regions(), ", "), legacyRegion)
	}
	if cfg.BaseURL != "" {
		return cfg, machine.CheckKeyURL("archil", "baseUrl", cfg.BaseURL)
	}
	if _, ok := controlPlanes[cfg.Region]; !ok {
		return cfg, fmt.Errorf("archil: configuration: region: not one that runs sandboxes (%s)", strings.Join(regions(), ", "))
	}
	return cfg, nil
}

// Validate checks the archil driver's configuration.
func Validate(config json.RawMessage) error {
	_, err := decode(config)
	return err
}
