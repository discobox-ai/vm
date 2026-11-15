package boxd

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// What Check asks boxd of the key's org: its machine limit, and the
// secrets in the scopes the driver's machines are in.

// machineLimit reads the key's org's machine limit at the home endpoint, and
// keeps it for Capabilities.MaxMachines. A credential boxd does not let read
// it (boxd documents a login and an API key, not a token minted inside a
// machine) leaves no cap, and is no error: boxd still enforces the limit.
func (d *Driver) machineLimit(ctx context.Context, account boxdapi.BoxdApiClient) (*boxdapi.GetOrgMachineLimitResponse, error) {
	limit, err := account.GetOrgMachineLimit(ctx, &boxdapi.GetOrgMachineLimitRequest{})
	if code := status.Code(err); code == codes.PermissionDenied || code == codes.Unimplemented {
		d.maxMachines.Store(0)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("boxd: the org's machine limit: %w", err)
	}
	d.maxMachines.Store(int64(limit.GetMaxVms()))
	return limit, nil
}

// atLimit reports, when a create is refused, whether the org has as many
// machines as its limit allows, so the refusal is machine.ErrCapacity. boxd
// does not document the code it refuses with, so the limit is asked again;
// a limit it cannot read is not one reached.
func (d *Driver) atLimit(ctx context.Context) (*boxdapi.GetOrgMachineLimitResponse, bool) {
	account, err := d.api.Account()
	if err != nil {
		return nil, false
	}
	limit, err := d.machineLimit(ctx, account)
	if err != nil || limit.GetMaxVms() == 0 {
		return nil, false
	}
	return limit, limit.GetVmCount() >= limit.GetMaxVms()
}

// reachingScopes are the scopes of a secret that reach a private machine of
// the key's owner, which every machine of the driver's is: the org's "all"
// ones and the owner's "private" ones (docs.boxd.sh/guides/env-secrets).
// "shared" reaches the org's shared machines alone, and "unattached" and
// "private-unattached" only a machine whose spec lists the secret, which the
// driver's never do. An empty scope counts too: boxd ignores a personal
// secret's scope, and does not say what it lists one as, nor an org's set
// with none (its default is "shared"). Missing a secret would leave the
// fence open unseen, so the error says to set the scope explicitly.
var reachingScopes = []string{"all", "private", ""}

// hostBoundSecrets fails when a host-bound secret would reach the driver's
// machines: boxd lets a machine reach its hosts whatever its egress
// allowlist says, so the fence holds only without one. It lists the secrets
// with no org, which boxd resolves as it does a create's, which names none
// either: to the org a key or a machine's token is fenced to, and for a login
// to its own scope. It names each secret, its scope, and its hosts; boxd
// never returns a value. org names the org in the error, when known.
func hostBoundSecrets(ctx context.Context, account boxdapi.BoxdApiClient, org string) error {
	vars, err := account.ListVars(ctx, &boxdapi.ListVarsRequest{Kind: "secret"})
	if err != nil {
		return fmt.Errorf("boxd: list the secrets: %w", err)
	}
	var found []string
	for _, v := range vars.GetVars() {
		if len(v.GetDomains()) == 0 || !slices.Contains(reachingScopes, v.GetScope()) {
			continue
		}
		scope := v.GetScope()
		if scope == "" {
			scope = "none listed; set one explicitly"
		}
		found = append(found, fmt.Sprintf("%s (scope %s; hosts %s)", v.GetName(), scope, strings.Join(v.GetDomains(), ", ")))
	}
	if len(found) == 0 {
		return nil
	}
	where := "this key's org"
	if org != "" {
		where = "org " + org
	}
	return fmt.Errorf("boxd: host-bound secrets of %s reach every machine of this key's, and boxd lets a machine reach their hosts whatever its egress allowlist says: %s; move each to scope shared or unattached, or bind it to no hosts",
		where, strings.Join(found, "; "))
}
