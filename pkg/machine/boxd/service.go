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
// default proxy, which Create pinned to it unless its ingress URL was asked
// off: its ingress URL, the same across stop and start. A machine whose
// default proxy is closed (locked to closedPort), or that has none, has no
// ingress URL. A machine on networks is reached by its peers there directly,
// at its name on boxd's internal DNS, <machine>.boxd, on every port: its pool
// URL; one on none, by its ingress URL, if it has one.
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
	if def != nil && !closed(def) {
		if def.GetDomain() == "" {
			return machine.Service{}, fmt.Errorf("boxd: %s's proxy has no domain", d.machineName(inst.Name))
		}
		service.IngressURL = "https://" + def.GetDomain()
	}
	service.PoolURL = service.IngressURL
	if len(inst.Networks) > 0 {
		service.PoolURL = "http://" + net.JoinHostPort(d.machineName(inst.Name)+".boxd", strconv.FormatUint(uint64(guest.ServicePort), 10))
	}
	return service, nil
}

// closedPort is the port a closed default proxy is locked to. boxd will not
// delete a machine's default proxy (DeleteProxy answers FailedPrecondition
// for it, and NotFound for its empty name), but it locks one to port 0, and
// answers 502 there: no guest can listen on TCP port 0, so nothing the
// guest's root runs answers at <machine>.<zone>, across stop and start.
const closedPort = 0

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
	def, err := lockProxy(ctx, api, vm, port)
	if err != nil {
		return "", err
	}
	if def.GetDomain() == "" {
		return "", fmt.Errorf("boxd: %s's proxy has no domain", vm)
	}
	return "https://" + def.GetDomain(), nil
}

// closeProxies leaves the machine reached from the internet at no URL, since
// whatever answers there is the guest root's choice, not the service's: it
// removes every proxy but the default one, which boxd keeps, closes that by
// locking it to closedPort, and reads back that it is the only one and is
// closed. An isolated machine has no in-VM CLI to open or add one again.
// Once they are read back closed, and not before, bot protection is turned
// off, as pinProxy does: port 0 is what keeps the guest unreached, and boxd's
// browser challenge in front of it would answer 503 in place of the closed
// proxy's 502, hiding that it is closed. Until then the guest, which runs,
// may still be reached through a proxy, and the challenge stays in front.
func closeProxies(ctx context.Context, api boxdapi.BoxdApiClient, vm string) error {
	list, err := api.ListProxies(ctx, &boxdapi.ListProxiesRequest{Vm: vm})
	if err != nil {
		return fmt.Errorf("boxd: proxies of %s: %w", vm, err)
	}
	var def *boxdapi.ProxyInfo
	for _, p := range list.GetProxies() {
		if p.GetIsDefault() {
			def = p
			continue
		}
		if _, err := api.DeleteProxy(ctx, &boxdapi.DeleteProxyRequest{Name: p.GetName(), Vm: vm}); err != nil {
			return fmt.Errorf("boxd: remove %s's proxy %q (%s): %w", vm, p.GetName(), p.GetDomain(), err)
		}
	}
	if def == nil {
		return fmt.Errorf("boxd: %s has no default proxy", vm)
	}
	if err := setProxyPort(ctx, api, vm, def, closedPort); err != nil {
		return err
	}
	if list, err = api.ListProxies(ctx, &boxdapi.ListProxiesRequest{Vm: vm}); err != nil {
		return fmt.Errorf("boxd: read back %s's proxies: %w", vm, err)
	}
	var open []string
	for _, p := range list.GetProxies() {
		if !p.GetIsDefault() || !closed(p) {
			open = append(open, fmt.Sprintf("%s (port %d, %s)", p.GetDomain(), p.GetEffectivePort(), p.GetPortMode()))
		}
	}
	if len(open) > 0 {
		return fmt.Errorf("boxd: %s still has proxies open at %v once they were closed", vm, open)
	}
	if _, err := api.SetBotProtection(ctx, &boxdapi.SetBotProtectionRequest{VmId: vm, Enabled: false}); err != nil {
		return fmt.Errorf("boxd: turn off bot protection on %s: %w", vm, err)
	}
	return nil
}

// lockProxy locks the machine's default proxy to port, and reads it back
// locked there.
func lockProxy(ctx context.Context, api boxdapi.BoxdApiClient, vm string, port uint32) (*boxdapi.ProxyInfo, error) {
	def, err := defaultProxy(ctx, api, vm)
	if err != nil {
		return nil, err
	}
	if def == nil {
		return nil, fmt.Errorf("boxd: %s has no default proxy", vm)
	}
	if err := setProxyPort(ctx, api, vm, def, port); err != nil {
		return nil, err
	}
	if def, err = defaultProxy(ctx, api, vm); err != nil {
		return nil, err
	}
	if def == nil || def.GetPortMode() != "locked" || def.GetEffectivePort() != port {
		return nil, fmt.Errorf("boxd: %s's proxy forwards %q to port %d (%s), not locked to %d", vm, def.GetDomain(), def.GetEffectivePort(), def.GetPortMode(), port)
	}
	return def, nil
}

// setProxyPort locks the machine's default proxy def to port. Its name is
// empty, which SetProxyPort takes as the default proxy.
func setProxyPort(ctx context.Context, api boxdapi.BoxdApiClient, vm string, def *boxdapi.ProxyInfo, port uint32) error {
	if _, err := api.SetProxyPort(ctx, &boxdapi.SetProxyPortRequest{Name: def.GetName(), Vm: vm, Port: strconv.FormatUint(uint64(port), 10)}); err != nil {
		return fmt.Errorf("boxd: lock %s's proxy %s to port %d: %w", vm, def.GetDomain(), port, err)
	}
	return nil
}

// closed is whether a proxy is locked to closedPort.
func closed(p *boxdapi.ProxyInfo) bool {
	return p.GetPortMode() == "locked" && p.GetEffectivePort() == closedPort
}

// defaultProxy is the proxy boxd gives every machine at <machine>.<zone>, nil
// for an ID boxd lists none for.
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
