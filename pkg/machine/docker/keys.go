package docker

import (
	"context"

	"github.com/discobox-ai/vm/pkg/machine"
)

// IssueKey has nothing to issue: the daemon is reached through its socket,
// with no credential of disco-vm's to hand on.
func (*Driver) IssueKey(context.Context, machine.IssueRequest) (machine.Key, error) {
	return machine.Key{}, machine.ErrNoKey
}

// IssuedKeys is none: the driver issues no keys.
func (*Driver) IssuedKeys(context.Context, string) ([]machine.Key, error) { return nil, nil }

// RevokeKey has no key to revoke.
func (*Driver) RevokeKey(context.Context, machine.Key) error { return machine.ErrNoKey }
