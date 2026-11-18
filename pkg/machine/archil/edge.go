package archil

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/discobox-ai/vm/pkg/guest"
	"github.com/discobox-ai/vm/pkg/machine"
)

// What the driver holds at Archil's edge, outside the guest (ADR
// 26-11-17-311): the fence, with DNS closed when asked, whether the VM's
// ingress URL is off, and the port tokens that alone reach its service.

// resolvers are the resolvers an Archil guest's resolv.conf names. An
// allow-list leaves port 53 open to them, so a fence that closes DNS denies
// both, and Deny wins over Allow.
var resolvers = []string{"8.8.8.8", "1.1.1.1"}

// ingressOff is the mark, in a sandbox's network policy, of a VM whose
// ingress URL is asked off: a sandbox has no labels, and the driver keeps
// nothing of its own. It is a denied address in TEST-NET-1 (RFC 5737), which
// nothing routes to, so denying it changes no traffic (seen 2026-11-18: an
// unfenced VM with it resolved names and reached example.com over HTTP and
// over HTTPS with the site's own certificate, as one without it did); and an
// address, not a name, since a domain rule has Archil terminate the guest's
// TLS on 443.
const ingressOff = "192.0.2.86"

// tokenHeader carries a port token.
const tokenHeader = "X-Archil-Token" //nolint:gosec // G101: a header's name, not a credential

// tokenLife is how long a port token Service mints lasts. A holder renews
// well before it ends, by asking Service again.
const tokenLife = 24 * time.Hour

// checkAllow refuses a fence that closes DNS and allows a resolver it
// denies: the resolver's port 53 would stay closed, and the allow mean
// nothing.
func checkAllow(e *machine.Egress) error {
	if e == nil || !e.CloseDNS {
		return nil
	}
	for _, a := range e.Allow {
		if slices.Contains(resolvers, a) {
			return fmt.Errorf("archil: a fence that closes DNS denies %s, and cannot allow it", a)
		}
	}
	return nil
}

// policy is the network policy of an egress fence; nil is none.
func policy(e *machine.Egress) *egressPolicy {
	if e == nil {
		return nil
	}
	p := &egressPolicy{Egress: &egressRules{Default: "deny"}}
	for _, a := range e.Allow {
		p.Egress.Allow = append(p.Egress.Allow, a)
	}
	if e.CloseDNS || len(e.Allow) == 0 {
		// Nothing beyond the networks closes DNS too: Archil may leave its
		// resolvers open even under a deny-all default.
		p.Egress.Deny = slices.Clone(resolvers)
	}
	return p
}

// fenceOf is the fence a policy holds: nil when egress is not denied by
// default. It closes DNS where it denies every resolver, or allows nothing.
func fenceOf(p egressPolicy) *machine.Egress {
	if p.Egress == nil || p.Egress.Default != "deny" {
		return nil
	}
	e := &machine.Egress{}
	for _, a := range p.Egress.Allow {
		switch v := a.(type) {
		case string:
			e.Allow = append(e.Allow, v)
		case map[string]any:
			if t, ok := v["target"].(string); ok {
				e.Allow = append(e.Allow, t)
			}
		}
	}
	if len(e.Allow) > 0 {
		e.CloseDNS = true
		for _, r := range resolvers {
			e.CloseDNS = e.CloseDNS && slices.Contains(p.Egress.Deny, r)
		}
	}
	return e
}

// sameFence reports whether two fences hold the same: an empty Allow closes
// DNS whatever CloseDNS says.
func sameFence(a, b *machine.Egress) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return slices.Equal(a.Allow, b.Allow) && (len(a.Allow) == 0 || a.CloseDNS == b.CloseDNS)
}

// ingressOf reports whether a policy leaves the VM's ingress URL on: it
// does not carry the ingressOff mark.
func ingressOf(p egressPolicy) bool {
	return p.Egress == nil || !slices.Contains(p.Egress.Deny, ingressOff)
}

// wantPolicy is the policy a sandbox whose policy is had should hold for a
// fence and ingress: the fence's own, or, with none, had with any fence a
// fork carried removed and the provider's default kept; marked ingressOff
// when ingress is off.
func wantPolicy(had egressPolicy, fence *machine.Egress, off bool) egressPolicy {
	var p egressPolicy
	switch {
	case fence != nil:
		p = *policy(fence)
	case fenceOf(had) == nil && had.Egress != nil:
		rules := *had.Egress
		rules.Deny = slices.DeleteFunc(slices.Clone(rules.Deny), func(d string) bool { return d == ingressOff })
		p.Egress = &rules
	}
	if off {
		if p.Egress == nil {
			p.Egress = &egressRules{Default: "allow"}
		}
		p.Egress.Deny = append(p.Egress.Deny, ingressOff)
	}
	if r := p.Egress; r != nil && r.Default == "allow" && len(r.Allow) == 0 && len(r.Deny) == 0 {
		// Allowing all and denying nothing is no policy.
		p.Egress = nil
	}
	return p
}

// samePolicy compares two policies as JSON would, nil and empty lists alike.
func samePolicy(a, b egressPolicy) bool {
	if a.Egress == nil || b.Egress == nil {
		return a.Egress == nil && b.Egress == nil
	}
	return a.Egress.Default == b.Egress.Default && slices.Equal(a.Egress.Deny, b.Egress.Deny) &&
		slices.EqualFunc(a.Egress.Allow, b.Egress.Allow, func(x, y any) bool { return fmt.Sprint(x) == fmt.Sprint(y) })
}

// setNetwork gives a sandbox the policy of a fence and ingress, where it
// holds another (a fork holds its source's; a fresh sandbox, the fence alone),
// and reads it back holding both. It returns the policy read back.
func (d *Driver) setNetwork(ctx context.Context, id string, fence *machine.Egress, off bool) (egressPolicy, error) {
	had, err := d.api.network(ctx, id)
	if err != nil {
		return had, err
	}
	if want := wantPolicy(had, fence, off); !samePolicy(had, want) {
		if err := d.api.setNetwork(ctx, id, want); err != nil {
			return had, err
		}
	}
	got, err := d.api.network(ctx, id)
	if err != nil {
		return got, err
	}
	if !sameFence(fenceOf(got), fence) {
		return got, fmt.Errorf("fenced to %v, and Archil holds %v", fence, fenceOf(got))
	}
	if ingressOf(got) == off {
		return got, fmt.Errorf("its ingress URL asked off is %t, and Archil holds it %t", off, !ingressOf(got))
	}
	return got, nil
}

// unexposeService makes 7302 public no more, where a VM made before ports
// were left unexposed has it exposed: every Start, Commit, Service, and
// Create or Upgrade from a committed VM (the source and the fork) unexpose it, so such a
// VM is fenced whichever of them it meets first, a VM an anonymous request
// woke and a committed one included.
func (d *Driver) unexposeService(ctx context.Context, id string) error {
	if err := d.api.unexposePort(ctx, id, guest.ServicePort); err != nil {
		return fmt.Errorf("unexpose port %d: %w", guest.ServicePort, err)
	}
	return nil
}

// revokeTokens revokes every port token of a sandbox. A revoke that fails
// fails the call; a token it leaves still expires.
func (d *Driver) revokeTokens(ctx context.Context, id string) error {
	tokens, err := d.api.portTokens(ctx, id)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("list port tokens: %w", err)
	}
	for _, t := range tokens {
		if err := d.api.revokeToken(ctx, id, t.ID); err != nil {
			return fmt.Errorf("revoke port token %s: %w", t.ID, err)
		}
	}
	return nil
}

// Listen is guest.ServicePort on every address of the guest's.
func (*Driver) Listen(context.Context, machine.Instance) (string, error) {
	return fmt.Sprintf("tcp:0.0.0.0:%d", guest.ServicePort), nil
}

// Service mints a port token for guest.ServicePort, which lasts tokenLife,
// and gives the port's hostname, which Archil gives only with a token and
// which lasts across stop and start: the ingress URL, unless asked off, and
// the pool URL either way, since Archil gives sandboxes no path to each
// other. The token is the credential, never kept and never put in the guest.
// A request without it is refused before the guest, and does not start a
// stopped one.
func (d *Driver) Service(ctx context.Context, inst machine.Instance) (machine.Service, error) {
	sb, err := d.sandboxOf(ctx, inst)
	if err != nil {
		return machine.Service{}, fmt.Errorf("archil: %s's service: %w", inst.Name, err)
	}
	// A URL handed out with a credential is one that needs it.
	if err := d.unexposeService(ctx, sb.ID); err != nil {
		return machine.Service{}, fmt.Errorf("archil: %s's service: %w", inst.Name, err)
	}
	p, err := d.api.network(ctx, sb.ID)
	if err != nil {
		return machine.Service{}, fmt.Errorf("archil: %s's service: %w", inst.Name, err)
	}
	asked := time.Now()
	t, err := d.api.mintToken(ctx, sb.ID, guest.ServicePort, tokenLife)
	if err != nil {
		return machine.Service{}, fmt.Errorf("archil: %s's service: mint a port token: %w", inst.Name, err)
	}
	if t.Hostname == "" || t.Token == "" {
		return machine.Service{}, fmt.Errorf("archil: %s's service: a port token came with no hostname or no secret", inst.Name)
	}
	expires := t.Expires
	if expires.IsZero() {
		// Archil names no expiry: the one asked for, counted from before
		// it was, so never later than Archil's.
		expires = asked.Add(tokenLife)
	}
	service := machine.Service{
		PoolURL:    "https://" + t.Hostname,
		Credential: &machine.Credential{Header: tokenHeader, Token: t.Token, Expires: expires},
	}
	if ingressOf(p) {
		service.IngressURL = service.PoolURL
	}
	return service, nil
}
