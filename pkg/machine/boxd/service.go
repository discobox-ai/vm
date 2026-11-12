package boxd

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// serviceListen is where a boxd guest's service listens: boxd's proxy reaches
// the machine over its network, at guest.ServicePort.
var serviceListen = "tcp:0.0.0.0:" + strconv.FormatUint(uint64(guest.ServicePort), 10)

// Service is the guest's TCP port behind the HTTPS URL of the machine's
// default proxy, which Create pinned to it for a VM asked to be public. The
// URL is the same across stop and start. A machine with no default proxy has
// no URL. A machine on networks is reached by its peers there directly, at
// its name on boxd's internal DNS, <machine>.boxd, on every port; one on
// none, by its URL, if it has one.
func (d *Driver) Service(ctx context.Context, inst machine.Instance) (machine.Service, error) {
	api, err := d.api.API(ctx)
	if err != nil {
		return machine.Service{}, err
	}
	def, err := defaultProxy(ctx, api, inst.ID)
	if err != nil {
		return machine.Service{}, d.refuseCommitted(ctx, api, inst, err)
	}
	if def == nil {
		// boxd lists no proxies for a machine with none, and may for an ID
		// that is no machine at all (a committed VM's snapshot, a machine
		// gone): ask for the machine itself, so neither is served.
		if _, err := api.GetVm(ctx, &boxdapi.GetVmRequest{VmId: inst.ID}); err != nil {
			if isNotFound(err) {
				err = fmt.Errorf("boxd: VM %s: %w", inst.Name, machine.ErrNotFound)
			}
			return machine.Service{}, d.refuseCommitted(ctx, api, inst, err)
		}
	}
	service := machine.Service{Listen: serviceListen}
	if def != nil {
		if def.GetDomain() == "" {
			return machine.Service{}, fmt.Errorf("boxd: %s's proxy has no domain", d.machineName(inst.Name))
		}
		service.URL = "https://" + def.GetDomain()
	}
	service.PrivateURL = service.URL
	if len(inst.Networks) > 0 {
		service.PrivateURL = "http://" + net.JoinHostPort(d.machineName(inst.Name)+".boxd", strconv.FormatUint(uint64(guest.ServicePort), 10))
	}
	return service, nil
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
	if def == nil {
		return "", fmt.Errorf("boxd: %s has no default proxy", vm)
	}
	if _, err := api.SetProxyPort(ctx, &boxdapi.SetProxyPortRequest{Name: def.GetName(), Vm: vm, Port: strconv.FormatUint(uint64(port), 10)}); err != nil {
		return "", fmt.Errorf("boxd: point %s's proxy %q at port %d: %w", vm, def.GetName(), port, err)
	}
	if def, err = defaultProxy(ctx, api, vm); err != nil {
		return "", err
	}
	if def == nil || def.GetPortMode() != "locked" || def.GetEffectivePort() != port || def.GetDomain() == "" {
		return "", fmt.Errorf("boxd: %s's proxy forwards %q to port %d (%s), not locked to %d", vm, def.GetDomain(), def.GetEffectivePort(), def.GetPortMode(), port)
	}
	return "https://" + def.GetDomain(), nil
}

// dropProxies removes every proxy boxd gave the machine, its default one at
// <machine>.<zone> among them, and reads back that none is left: a VM not
// asked to be public is reached from the internet at no URL, since whatever
// answers there is the guest root's choice, not the service's. An isolated
// machine has no in-VM CLI to make one again.
func dropProxies(ctx context.Context, api boxdapi.BoxdApiClient, vm string) error {
	list, err := api.ListProxies(ctx, &boxdapi.ListProxiesRequest{Vm: vm})
	if err != nil {
		return fmt.Errorf("boxd: proxies of %s: %w", vm, err)
	}
	for _, p := range list.GetProxies() {
		if _, err := api.DeleteProxy(ctx, &boxdapi.DeleteProxyRequest{Name: p.GetName(), Vm: vm}); err != nil {
			return fmt.Errorf("boxd: remove %s's proxy %q (%s): %w", vm, p.GetName(), p.GetDomain(), err)
		}
	}
	if list, err = api.ListProxies(ctx, &boxdapi.ListProxiesRequest{Vm: vm}); err != nil {
		return fmt.Errorf("boxd: read back %s's proxies: %w", vm, err)
	}
	if left := list.GetProxies(); len(left) > 0 {
		var domains []string
		for _, p := range left {
			domains = append(domains, p.GetDomain())
		}
		return fmt.Errorf("boxd: %s still has proxies at %v once they were removed", vm, domains)
	}
	return nil
}

// defaultProxy is the proxy boxd gives every machine at <machine>.<zone>, nil
// once it is removed.
func defaultProxy(ctx context.Context, api boxdapi.BoxdApiClient, vm string) (*boxdapi.ProxyInfo, error) {
	list, err := api.ListProxies(ctx, &boxdapi.ListProxiesRequest{Vm: vm})
	if err != nil {
		return nil, fmt.Errorf("boxd: proxies of %s: %w", vm, err)
	}
	for _, p := range list.GetProxies() {
		if p.GetIsDefault() {
			return p, nil
		}
	}
	return nil, nil
}
