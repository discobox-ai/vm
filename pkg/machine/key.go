package machine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"
)

// ErrNoKey reports a driver that has no credential of its own to hand on: a
// local hypervisor (hcs, vz) or a daemon reached through a socket (docker).
var ErrNoKey = errors.New("machine: this driver has no key to issue")

// ErrCannotIssue reports a driver whose provider has keys, but none the driver
// can issue that both expires and cannot itself issue keys. It issues none
// rather than one whose holder could outlive every revocation.
var ErrCannotIssue = errors.New("machine: this driver cannot issue a key that expires and cannot issue keys")

// ErrUnauthorized reports a key the provider refused: unknown, revoked,
// expired, or one that may not do what was asked (an issued key issuing
// another).
var ErrUnauthorized = errors.New("machine: the key was refused")

// Scope is what a key reaches.
type Scope string

const (
	// ScopeAccount reaches everything of the account's.
	ScopeAccount Scope = "account"
	// ScopeOrg reaches the account's machines in one organization of the
	// provider's (a boxd key fenced to an org).
	ScopeOrg Scope = "org"
	// ScopeProject reaches one project's images, tags, and instances.
	ScopeProject Scope = "project"
)

// KeyMode is whether a driver issues keys.
type KeyMode string

const (
	// KeysNone is a driver that issues no keys: IssueKey returns ErrNoKey or
	// ErrCannotIssue.
	KeysNone KeyMode = "none"
	// KeysIssue is a driver that issues keys that expire and cannot issue
	// keys.
	KeysIssue KeyMode = "issue"
)

// KeyCapabilities is what a driver says about the keys it issues, before a
// caller makes anything with one.
type KeyCapabilities struct {
	Mode KeyMode `json:"mode"`
	// Scope is the narrowest scope an issued key reaches, when Mode is
	// KeysIssue.
	Scope Scope `json:"scope,omitempty"`
}

// Key is a credential for a driver's provider: the caller's own, or one the
// driver issued (IssueKey). Its Secret is opaque: disco-vm never logs it or
// keeps it, and a Key prints without it.
type Key struct {
	// Driver is the driver the key is for.
	Driver string `json:"driver"`
	// Secret is the credential itself. It is omitted from a key IssuedKeys
	// lists, since a provider shows a key only once.
	Secret string `json:"secret,omitempty"`
	// ID is the provider's ID for a key the driver issued, empty for the
	// caller's own.
	ID string `json:"id,omitempty"`
	// Name is what an issued key was issued as (IssueRequest.Name).
	Name string `json:"name,omitempty"`
	// Issued reports a key the driver issued.
	Issued bool `json:"issued"`
	// Scope is what the key reaches, as the driver says; empty where it
	// cannot tell (the caller's own key).
	Scope Scope `json:"scope,omitempty"`
	// Expires is when the key stops working; zero is never.
	Expires time.Time `json:"expires,omitzero"`
}

// String names the key without its secret, so a key that reaches a log or an
// error message carries nothing that works.
func (k Key) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s key", k.Driver)
	if k.ID != "" {
		fmt.Fprintf(&b, " %s", k.ID)
	}
	if k.Name != "" {
		fmt.Fprintf(&b, " %q", k.Name)
	}
	if k.Scope != "" {
		fmt.Fprintf(&b, " (%s)", k.Scope)
	}
	if !k.Expires.IsZero() {
		fmt.Fprintf(&b, " expiring %s", k.Expires.UTC().Format(time.RFC3339))
	}
	return b.String()
}

// GoString is String, so %#v leaves the secret out too.
func (k Key) GoString() string { return k.String() }

// LogValue is String, so slog leaves the secret out too.
func (k Key) LogValue() slog.Value { return slog.StringValue(k.String()) }

// KeySource is the key a driver calls its provider with. The driver asks it
// on every call, so a key replaced where the source reads it is used from the
// next call, with no new driver. A driver given one never reads the
// environment for a key; a driver given none has no key of the caller's.
type KeySource func(ctx context.Context) (Key, error)

// StaticKey is a source that always answers key.
func StaticKey(key Key) KeySource {
	return func(context.Context) (Key, error) { return key, nil }
}

// IssueRequest is a key a driver issues for another host to run its project's
// machines with.
type IssueRequest struct {
	// Name is what the key is issued as, unique to the caller: IssuedKeys
	// finds it by this, so a caller that lost its record of a key can still
	// revoke it.
	Name string
	// TTL is how long the key lasts. It must be positive: an issued key
	// always expires.
	TTL time.Duration
}

// Validate checks a request: a name ValidateName accepts, and a positive TTL.
func (r IssueRequest) Validate() error {
	if err := ValidateName(r.Name); err != nil {
		return fmt.Errorf("machine: key name: %w", err)
	}
	if r.TTL <= 0 {
		return errors.New("machine: an issued key must expire: give it a positive TTL")
	}
	return nil
}

// KeyFromEnv is the source of a driver's key in the environment, for the
// command line: the first of the variables the driver registered that is
// set. It is nil, no key, when the driver registered none or none is set.
func KeyFromEnv(driver string) KeySource {
	registryMu.Lock()
	vars := keyEnv[driver]
	registryMu.Unlock()
	for _, name := range vars {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return StaticKey(Key{Driver: driver, Secret: v})
		}
	}
	return nil
}

// WithoutKeys is env, as os.Environ gives it, with every variable a driver
// reads its key from (KeyFromEnv) left out: the environment of a process the
// driver starts (a shim, a fake guest), so no key reaches it.
func WithoutKeys(env []string) []string {
	registryMu.Lock()
	defer registryMu.Unlock()
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		for _, vars := range keyEnv {
			if slices.Contains(vars, name) {
				return true
			}
		}
		return false
	})
}
