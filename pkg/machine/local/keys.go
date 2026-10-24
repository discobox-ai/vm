package local

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine"
)

// keyPrefix starts the secret of every key the driver issues.
const keyPrefix = "dvmk_"

// keyRecord is an issued key's file, keys/<hash>.json, named for the
// SHA-256 of its secret, so a call finds its key's record in one read. It
// keeps the hash, never the secret.
type keyRecord struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Project string    `json:"project"`
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
}

func (d *Driver) keysDir() string { return filepath.Join(d.dir, "keys") }

// issues reports a hypervisor whose driver issues keys (fake). One that does
// not (hcs, vz) has no credential of its own: whoever can read the directory
// is its owner, and a key source is not asked.
func (d *Driver) issues() bool { return d.hv.Capabilities().Keys.Mode == machine.KeysIssue }

// authorize checks the driver's key before a call. With no key source the
// caller is the directory's owner. With one, on a hypervisor that issues
// keys, the key must be one the driver issued for this project that has not
// expired or been revoked.
func (d *Driver) authorize(ctx context.Context) error {
	_, err := d.caller(ctx)
	return err
}

// authorizeInstance is authorize, and refuses an instance whose record is
// another project's: the methods that take an instance reach it by its ID.
func (d *Driver) authorizeInstance(ctx context.Context, inst machine.Instance) error {
	issued, err := d.caller(ctx)
	if err != nil {
		return err
	}
	if !validID(inst.ID) {
		return fmt.Errorf("instance %s: %w", inst.ID, machine.ErrNotFound)
	}
	var rec instanceRecord
	err = fsutil.ReadJSON(filepath.Join(d.instanceDir(inst.ID), instanceFile), &rec)
	switch {
	case errors.Is(err, fs.ErrNotExist) && issued != nil:
		// An instance with no record yet may be any project's, half made.
		return fmt.Errorf("instance %s: %w", inst.ID, machine.ErrNotFound)
	case errors.Is(err, fs.ErrNotExist):
		// The owner removes what a crash left, or what is gone already.
		return nil
	case err != nil:
		return err
	case rec.Project != d.project:
		return fmt.Errorf("instance %s: %w", inst.ID, machine.ErrNotFound)
	}
	return nil
}

// caller is the issued key the call is made with, or nil for the owner.
func (d *Driver) caller(ctx context.Context) (*keyRecord, error) {
	if d.keys == nil || !d.issues() {
		return nil, nil
	}
	key, err := d.keys(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: key: %w", d.hv.Name(), err)
	}
	hash := hashSecret(key.Secret)
	var rec keyRecord
	err = fsutil.ReadJSON(filepath.Join(d.keysDir(), hash+".json"), &rec)
	if err == nil && subtle.ConstantTimeCompare([]byte(rec.Hash), []byte(hash)) == 1 &&
		rec.Project == d.project && time.Now().Before(rec.Expires) {
		return &rec, nil
	}
	return nil, fmt.Errorf("%s: %s: %w", d.hv.Name(), key, machine.ErrUnauthorized)
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// keyRecords reads every issued key's record, of every project. One it
// cannot read is left out: it authorizes nothing, since a call reads its own
// record, and is not this project's to list.
func (d *Driver) keyRecords() ([]keyRecord, error) {
	entries, err := os.ReadDir(d.keysDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []keyRecord
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var rec keyRecord
		if err := fsutil.ReadJSON(filepath.Join(d.keysDir(), entry.Name()), &rec); err == nil && rec.Hash+".json" == entry.Name() {
			out = append(out, rec)
		}
	}
	slices.SortFunc(out, func(a, b keyRecord) int { return a.Created.Compare(b.Created) })
	return out, nil
}

func (d *Driver) key(rec keyRecord) machine.Key {
	return machine.Key{Driver: d.hv.Name(), ID: rec.ID, Name: rec.Name, Issued: true, Scope: machine.ScopeProject, Expires: rec.Expires}
}

// owner refuses a key operation made with an issued key: an issued key
// cannot issue, list, or revoke keys.
func (d *Driver) owner(ctx context.Context) error {
	rec, err := d.caller(ctx)
	if err != nil {
		return err
	}
	if rec != nil {
		return fmt.Errorf("%s: an issued key cannot issue or revoke keys: %w", d.hv.Name(), machine.ErrUnauthorized)
	}
	return nil
}

// IssueKey writes a key bound to the project, which expires after the TTL.
func (d *Driver) IssueKey(ctx context.Context, req machine.IssueRequest) (machine.Key, error) {
	if !d.issues() {
		return machine.Key{}, machine.ErrNoKey
	}
	if err := d.owner(ctx); err != nil {
		return machine.Key{}, err
	}
	if err := req.Validate(); err != nil {
		return machine.Key{}, err
	}
	records, err := d.keyRecords()
	if err != nil {
		return machine.Key{}, err
	}
	now := time.Now().UTC()
	for _, rec := range records {
		switch {
		case rec.Project != d.project:
		case !now.Before(rec.Expires):
			// An expired key works no more: its record goes, and its name
			// is free.
			_ = os.Remove(filepath.Join(d.keysDir(), rec.Hash+".json"))
		case rec.Name == req.Name:
			return machine.Key{}, fmt.Errorf("%s: a key named %q: %w", d.hv.Name(), req.Name, machine.ErrExists)
		}
	}
	secret := keyPrefix + fsutil.RandomHex(32)
	rec := keyRecord{
		ID: fsutil.RandomHex(8), Name: req.Name, Project: d.project, Hash: hashSecret(secret),
		Created: now, Expires: now.Add(req.TTL),
	}
	if err := os.MkdirAll(d.keysDir(), 0o700); err != nil {
		return machine.Key{}, err
	}
	if err := fsutil.WriteJSON(filepath.Join(d.keysDir(), rec.Hash+".json"), rec); err != nil {
		return machine.Key{}, err
	}
	key := d.key(rec)
	key.Secret = secret
	return key, nil
}

// IssuedKeys lists the project's issued keys under a name, or all of them.
func (d *Driver) IssuedKeys(ctx context.Context, name string) ([]machine.Key, error) {
	if !d.issues() {
		return nil, nil
	}
	if err := d.owner(ctx); err != nil {
		return nil, err
	}
	records, err := d.keyRecords()
	if err != nil {
		return nil, err
	}
	var out []machine.Key
	for _, rec := range records {
		if rec.Project == d.project && (name == "" || rec.Name == name) {
			out = append(out, d.key(rec))
		}
	}
	return out, nil
}

// RevokeKey removes an issued key's record, which is what makes it work.
func (d *Driver) RevokeKey(ctx context.Context, key machine.Key) error {
	if !d.issues() {
		return machine.ErrNoKey
	}
	if err := d.owner(ctx); err != nil {
		return err
	}
	records, err := d.keyRecords()
	if err != nil {
		return err
	}
	for _, rec := range records {
		if rec.ID != key.ID {
			continue
		}
		if rec.Project != d.project {
			return fmt.Errorf("%s: key %s is project %s's, not %s's", d.hv.Name(), key.ID, rec.Project, d.project)
		}
		if err := os.Remove(filepath.Join(d.keysDir(), rec.Hash+".json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
