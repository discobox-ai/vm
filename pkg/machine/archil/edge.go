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

// mountHosts are the hosts Archil adds to a fenced sandbox's allow-list for
// the disks it mounts (seen 2026-11-26: a sandbox made with a mount under a
// deny-by-default policy held them after the asked list, and a policy set
// without them read back with them). They are Archil's, never the fence's:
// a fence is read without them, a policy set keeps them where Archil put
// them, and a fence that names one is refused.
var mountHosts = []string{"mount.*.archil.com", "data.*.archil.com"}

// tokenHeader carries a port token.
const tokenHeader = "X-Archil-Token" //nolint:gosec // G101: a header's name, not a credential

// tokenLife is how long a port token Service mints lasts. A holder renews
// well before it ends, by asking Service again.
const tokenLife = 24 * time.Hour

// checkAllow refuses a fence that names a host Archil adds for its mounts,
// which a fence is read without, and one that closes DNS and allows a
// resolver it denies: the resolver's port 53 would stay closed, and the
// allow mean nothing.
func checkAllow(e *machine.Egress) error {
	if e == nil {
		return nil
	}
	for _, a := range e.Allow {
		if slices.Contains(mountHosts, a) {
			return fmt.Errorf("archil: %s is Archil's own, allowed for a VM's data volume, and a fence cannot name it", a)
		}
		if e.CloseDNS && slices.Contains(resolvers, a) {
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

// target is an allow rule's host or network: the rule itself, or its
// target.
func target(a any) (string, bool) {
	switch v := a.(type) {
	case string:
		return v, true
	case map[string]any:
		t, ok := v["target"].(string)
		return t, ok
	}
	return "", false
}

// isMountHost reports whether an allow rule is one Archil adds for its
// mounts.
func isMountHost(a any) bool {
	t, ok := target(a)
	return ok && slices.Contains(mountHosts, t)
}

// fenceOf is the fence a policy holds, without the hosts Archil adds for its
// mounts: nil when egress is not denied by default. It closes DNS where it
// denies every resolver, or allows nothing.
func fenceOf(p egressPolicy) *machine.Egress {
	if p.Egress == nil || p.Egress.Default != "deny" {
		return nil
	}
	e := &machine.Egress{}
	for _, a := range p.Egress.Allow {
		if t, ok := target(a); ok && !isMountHost(a) {
			e.Allow = append(e.Allow, t)
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
// fence and ingress: the fence's own, with the hosts Archil added to had for
// its mounts kept after it, or, with none, had with any fence a fork carried
// removed and the provider's default kept; marked ingressOff when ingress is
// off.
func wantPolicy(had egressPolicy, fence *machine.Egress, off bool) egressPolicy {
	var p egressPolicy
	switch {
	case fence != nil:
		p = *policy(fence)
		if had.Egress != nil {
			for _, a := range had.Egress.Allow {
				if isMountHost(a) {
					p.Egress.Allow = append(p.Egress.Allow, a)
				}
			}
		}
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
// holds another (a fork holds its source's; a fresh sandbox, the fence alone,
// and the hosts Archil adds for its mounts), and reads it back holding both:
// the fence as asked, but for those hosts, and nothing else. It returns the policy read back.
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
