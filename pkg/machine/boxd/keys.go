package boxd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// keyCapabilities is what IssueKey makes: a member key fenced to the
// engine's key's org, which reaches every machine of the caller's in it.
// boxd makes one only for an interactive session (`boxd login`): it refuses
// every API key, an issued one included, any key management (by hand,
// 2026-10-24), which is what keeps an issued key from issuing another.
var keyCapabilities = machine.KeyCapabilities{Mode: machine.KeysIssue, Scope: machine.ScopeOrg}

// keyName is an issued key's name on boxd, dvm-<project>--<name>, so the
// keys of a project are found by name, as its machines are.
func (d *Driver) keyName(name string) string { return d.prefix() + name }

// IssueKey creates a member API key, fenced to the org the engine's key is
// fenced to (boxd resolves an empty org to it), that expires after the TTL.
// A member key acts as the caller within that one org: it reaches every
// machine of the caller's there, not only this project's, which is the
// narrowest boxd makes. An org-kind key reaches only the org's shared fleet,
// which disco-vm's private machines are not in, and needs an org admin.
// Called with an API key, which boxd refuses, it is ErrCannotIssue.
func (d *Driver) IssueKey(ctx context.Context, req machine.IssueRequest) (machine.Key, error) {
	if err := req.Validate(); err != nil {
		return machine.Key{}, err
	}
	name := d.keyName(req.Name)
	api, err := d.api.API()
	if err != nil {
		return machine.Key{}, err
	}
	issued, err := d.IssuedKeys(ctx, req.Name)
	if status.Code(err) == codes.PermissionDenied {
		// boxd refuses key management to an API key, the listing included.
		// A key it does not know at all is Unauthenticated, and stays
		// ErrUnauthorized.
		return machine.Key{}, fmt.Errorf("boxd: issue key %s: boxd refused this key: %s: %w", name, boxdMessage(err), machine.ErrCannotIssue)
	}
	if err != nil {
		return machine.Key{}, err
	}
	for _, k := range issued {
		if k.Expires.IsZero() || time.Now().Before(k.Expires) {
			return machine.Key{}, fmt.Errorf("boxd: a key named %q: %w", req.Name, machine.ErrExists)
		}
		// An expired key of the name works no more: delete it, so keys do
		// not pile up under one name.
		if _, err := api.DeleteApiKey(ctx, &boxdapi.DeleteApiKeyRequest{Id: k.ID}); err != nil && !isNotFound(err) {
			return machine.Key{}, fmt.Errorf("boxd: delete expired API key %s: %w", k.ID, keyError(err))
		}
	}
	secs := math.Ceil(req.TTL.Seconds())
	created, err := api.CreateApiKey(ctx, &boxdapi.CreateApiKeyRequest{Name: name, ExpiresInSecs: uint64(secs), Kind: "member"})
	if status.Code(err) == codes.PermissionDenied {
		return machine.Key{}, fmt.Errorf("boxd: issue key %s: boxd refused this key: %s: %w", name, boxdMessage(err), machine.ErrCannotIssue)
	}
	if err != nil {
		return machine.Key{}, fmt.Errorf("boxd: create API key %s: %w", name, keyError(err))
	}
	expires := unixTime(created.GetExpiresAt(), time.Second)
	if latest := time.Now().Add(time.Duration(secs)*time.Second + time.Minute); expires.IsZero() || expires.After(latest) {
		// boxd made a key that outlives the TTL: take it back rather than
		// hand it on.
		err := fmt.Errorf("boxd: API key %s expires at %v, past its TTL of %s: %w", name, expires, req.TTL, machine.ErrCannotIssue)
		if _, derr := api.DeleteApiKey(context.WithoutCancel(ctx), &boxdapi.DeleteApiKeyRequest{Id: created.GetId()}); derr != nil {
			err = fmt.Errorf("%w; and it is still live, since deleting it failed: revoke key %s by hand: %w", err, created.GetId(), derr)
		}
		return machine.Key{}, err
	}
	return machine.Key{
		Driver: d.Name(), Secret: created.GetApiKey(), ID: created.GetId(), Name: req.Name,
		Issued: true, Scope: keyCapabilities.Scope, Expires: expires,
	}, nil
}

// IssuedKeys lists the caller's keys named for the project and name, or for
// the project under any name.
func (d *Driver) IssuedKeys(ctx context.Context, name string) ([]machine.Key, error) {
	listed, err := d.listKeys(ctx)
	if err != nil {
		return nil, err
	}
	var out []machine.Key
	for _, info := range listed {
		issued, ok := d.issuedName(info)
		if ok && (name == "" || issued == name) {
			out = append(out, machine.Key{
				Driver: d.Name(), ID: info.GetId(), Name: issued, Issued: true,
				Scope: keyCapabilities.Scope, Expires: unixTime(info.GetExpiresAt(), time.Second),
			})
		}
	}
	return out, nil
}

// issuedName is the name a key was issued as, if it is a member key of the
// caller's named for the project. The listing also holds the org-kind keys
// of an org the caller administers, whoever made them, which IssueKey never
// makes.
func (d *Driver) issuedName(info *boxdapi.ApiKeyInfo) (string, bool) {
	if !info.GetMine() || (info.GetKind() != "" && info.GetKind() != "member") {
		return "", false
	}
	name, ok := strings.CutPrefix(info.GetName(), d.prefix())
	return name, ok && machine.ValidateName(name) == nil
}

func (d *Driver) listKeys(ctx context.Context) ([]*boxdapi.ApiKeyInfo, error) {
	api, err := d.api.API()
	if err != nil {
		return nil, err
	}
	listed, err := api.ListApiKeys(ctx, &boxdapi.ListApiKeysRequest{})
	if err != nil {
		return nil, fmt.Errorf("boxd: list API keys: %w", keyError(err))
	}
	return listed.GetKeys(), nil
}

// RevokeKey deletes an issued key, once the listing shows it is one this
// driver issued for the project: DeleteApiKey would take any key of the
// caller's.
func (d *Driver) RevokeKey(ctx context.Context, key machine.Key) error {
	if key.ID == "" {
		return errors.New("boxd: revoke: the key has no ID")
	}
	listed, err := d.listKeys(ctx)
	if err != nil {
		return err
	}
	var found *boxdapi.ApiKeyInfo
	for _, info := range listed {
		if info.GetId() == key.ID {
			found = info
		}
	}
	if found == nil {
		return nil
	}
	if _, ok := d.issuedName(found); !ok {
		return fmt.Errorf("boxd: API key %s (%s) is not one disco-vm issued for project %s", key.ID, found.GetName(), d.project)
	}
	api, err := d.api.API()
	if err != nil {
		return err
	}
	if _, err := api.DeleteApiKey(ctx, &boxdapi.DeleteApiKeyRequest{Id: key.ID}); err != nil && !isNotFound(err) {
		return fmt.Errorf("boxd: delete API key %s: %w", key.ID, keyError(err))
	}
	return nil
}

// keyError marks boxd's refusal of key management to the caller's key as
// machine.ErrUnauthorized (refused has marked one it does not know).
func keyError(err error) error {
	if status.Code(err) == codes.PermissionDenied {
		return fmt.Errorf("%w: %w", err, machine.ErrUnauthorized)
	}
	return err
}

// boxdMessage is boxd's own message in a status err wraps.
func boxdMessage(err error) string {
	var gs interface{ GRPCStatus() *status.Status }
	if errors.As(err, &gs) {
		return gs.GRPCStatus().Message()
	}
	return err.Error()
}
