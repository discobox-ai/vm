package boxd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// serviceListen is where a boxd guest's service listens: boxd's proxy reaches
// the machine over its network, at guest.ServicePort.
var serviceListen = "tcp:0.0.0.0:" + strconv.FormatUint(uint64(guest.ServicePort), 10)

// Service is the guest's TCP port behind the HTTPS URL Prepare pointed at it,
// or for an instance of a base, its first Boot. Until that boot there is no
// machine, and so no URL.
func (*Driver) Service(_ context.Context, inst machine.InstanceSpec) (machine.Service, error) {
	var ref vmRef
	if err := readJSON(filepath.Join(inst.Dir, vmFile), &ref); errors.Is(err, fs.ErrNotExist) && inst.Base != nil {
		return machine.Service{}, fmt.Errorf("boxd: instance %s has not started yet: %w", inst.ID, machine.ErrNotRunning)
	} else if err != nil {
		return machine.Service{}, fmt.Errorf("boxd: instance %s is not prepared: %w", inst.ID, err)
	}
	if ref.Endpoint == "" {
		return machine.Service{}, fmt.Errorf("boxd: instance %s was prepared without a service; create it again", inst.ID)
	}
	return machine.Service{Listen: serviceListen, Port: guest.ServicePort, URL: ref.Endpoint}, nil
}

// expose points a restored instance's machine at the service port, and
// records the URL in vm.json for Service.
func (d *Driver) expose(ctx context.Context, api boxdapi.BoxdApiClient, inst machine.InstanceSpec) error {
	path := filepath.Join(inst.Dir, vmFile)
	var ref vmRef
	if err := readJSON(path, &ref); err != nil {
		return err
	}
	url, err := pinProxy(ctx, api, ref.ID, guest.ServicePort)
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
