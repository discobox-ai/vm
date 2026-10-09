package boxd

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// Endpoint is the HTTPS URL Prepare pointed at the instance's service.
func (*Driver) Endpoint(_ context.Context, inst machine.InstanceSpec) (string, error) {
	var ref vmRef
	if err := readJSON(filepath.Join(inst.Dir, vmFile), &ref); err != nil {
		return "", fmt.Errorf("boxd: instance %s is not prepared: %w", inst.ID, err)
	}
	return ref.Endpoint, nil
}

// expose points a restored instance's machine at its service, if it has one,
// and records the URL in vm.json for Endpoint.
func (d *Driver) expose(ctx context.Context, api boxdapi.BoxdApiClient, inst machine.InstanceSpec) error {
	if inst.Service == 0 {
		return nil
	}
	path := filepath.Join(inst.Dir, vmFile)
	var ref vmRef
	if err := readJSON(path, &ref); err != nil {
		return err
	}
	url, err := pinProxy(ctx, api, ref.ID, inst.Service)
	if err != nil {
		return err
	}
	ref.Endpoint = url
	return writeJSON(path, ref)
}

// pinProxy points the machine's default proxy at the service port and returns
// its URL, https://<machine>.<zone>. boxd terminates TLS there and forwards
// HTTP to the port. Pinning the port also stops the proxy forwarding to
// whichever common port the guest happens to listen on. Bot protection is
// turned off, since it would put a browser challenge in front of a request
// that wakes the machine, and the service's callers are programs.
func pinProxy(ctx context.Context, api boxdapi.BoxdApiClient, vm string, port uint32) (string, error) {
	if _, err := api.SetBotProtection(ctx, &boxdapi.SetBotProtectionRequest{VmId: vm, Enabled: false}); err != nil {
		return "", fmt.Errorf("boxd: turn off bot protection on %s: %w", vm, err)
	}
	def, err := defaultProxy(ctx, api, vm)
	if err != nil {
		return "", err
	}
	if _, err := api.SetProxyPort(ctx, &boxdapi.SetProxyPortRequest{Name: def.GetName(), Vm: vm, Port: strconv.FormatUint(uint64(port), 10)}); err != nil {
		return "", fmt.Errorf("boxd: point %s's proxy %q at port %d: %w", vm, def.GetName(), port, err)
	}
	if def, err = defaultProxy(ctx, api, vm); err != nil {
		return "", err
	}
	if def.GetPortMode() != "locked" || def.GetEffectivePort() != port || def.GetDomain() == "" {
		return "", fmt.Errorf("boxd: %s's proxy forwards %q to port %d (%s), not locked to %d", vm, def.GetDomain(), def.GetEffectivePort(), def.GetPortMode(), port)
	}
	return "https://" + def.GetDomain(), nil
}

// defaultProxy is the proxy boxd gives every machine at <machine>.<zone>.
func defaultProxy(ctx context.Context, api boxdapi.BoxdApiClient, vm string) (*boxdapi.ProxyInfo, error) {
	list, err := api.ListProxies(ctx, &boxdapi.ListProxiesRequest{Vm: vm})
	if err != nil {
		return nil, fmt.Errorf("boxd: proxies of %s: %w", vm, err)
	}
	var names []string
	for _, p := range list.GetProxies() {
		if p.GetIsDefault() {
			return p, nil
		}
		names = append(names, p.GetName())
	}
	return nil, fmt.Errorf("boxd: %s has no default proxy (proxies: %v)", vm, names)
}
