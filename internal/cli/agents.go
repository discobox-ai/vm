package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/agent"
)

// executable is this disco-vm's own path, and stderr where a warning goes;
// a test points them elsewhere.
var (
	executable           = os.Executable
	stderr     io.Writer = os.Stderr
)

// withAgents fills each field of the named driver's that names an agent
// (ConfigField.AgentOS) and that config leaves out with a disco-vm for that
// OS on this host's architecture (agentFor), which the driver then hands its
// guests. A field it finds none for stays absent, for the driver to refuse
// naming it, and only where it needs an agent. The shim is handed the
// result, so it resolves nothing itself.
func (g *globals) withAgents(name string, config json.RawMessage) (json.RawMessage, error) {
	def, err := machine.DefinitionOf(name)
	if err != nil {
		return config, nil //nolint:nilerr // an unknown driver is machine.New's to refuse
	}
	var fields map[string]any
	if len(bytes.TrimSpace(config)) > 0 {
		if err := json.Unmarshal(config, &fields); err != nil {
			return nil, fmt.Errorf("%s: configuration: not a JSON object", name)
		}
	}
	if fields == nil {
		// No configuration, or JSON's null, which decodes to no map.
		fields = map[string]any{}
	}
	changed := false
	for _, f := range def.Current() {
		if _, ok := fields[f.Key]; ok || f.AgentOS == "" {
			continue
		}
		if path := g.agentFor(f.AgentOS); path != "" {
			fields[f.Key], changed = path, true
		}
	}
	if !changed {
		return config, nil
	}
	return json.Marshal(fields)
}

// agentFor is the disco-vm for goos on this host's architecture that an
// absent agent field defaults to (ADR 26-11-14-352 §2): this executable, if
// it is one; else the one it carries (§1), written out once into an
// owner-only cache under the command line's directory, named by its
// SHA-256; else none (""). It goes by what the files are, never by the
// host's OS. One it carries but cannot write out (a full disk, a directory
// it cannot write) is none, with a warning: every command reads the
// configuration, and most need no agent, so only a boot that does is
// refused, by its driver (§2, step 3).
func (g *globals) agentFor(goos machine.OS) string {
	if path, ok := g.agents[goos]; ok {
		return path
	}
	path, err := g.findAgent(goos)
	if err != nil {
		fmt.Fprintf(stderr, "disco-vm: warning: no %s agent: %v\n", goos, err)
		path = ""
	}
	if g.agents == nil {
		g.agents = map[machine.OS]string{}
	}
	g.agents[goos] = path
	return path
}

func (g *globals) findAgent(goos machine.OS) (string, error) {
	exe, err := executable()
	if err != nil {
		return "", fmt.Errorf("this executable: %w", err)
	}
	if agent.Check(exe, string(goos), runtime.GOARCH) == nil {
		return exe, nil
	}
	return agent.Unpack(filepath.Join(g.dir(), "cache", "cli"), g.carried, string(goos), runtime.GOARCH)
}

// agentDefault says, for help, what an absent agent field for goos
// defaults to, writing nothing out: this disco-vm, the one it carries, or
// nothing ("").
func (g *globals) agentDefault(goos machine.OS) string {
	if exe, err := executable(); err == nil && agent.Check(exe, string(goos), runtime.GOARCH) == nil {
		return "this disco-vm"
	}
	if agent.Carries(g.carried, string(goos), runtime.GOARCH) {
		return fmt.Sprintf("the %s/%s disco-vm this one carries", goos, runtime.GOARCH)
	}
	return ""
}
