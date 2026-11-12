package machine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The types a ConfigField's value has in a driver's configuration, as JSON
// spells them. A secret is not one: it is a SecretField, given apart from
// the configuration (Config.Keys).
const (
	ConfigString  = "string"
	ConfigBoolean = "boolean"
	ConfigNumber  = "number"
)

// BaseEnv is where the command line reads Config.Base from, for every
// driver; Definition.Base describes what a driver takes there.
const BaseEnv = "DISCO_VM_BASE"

// ConfigField describes one field of a driver's configuration
// (Config.DriverConfig): a key of its JSON object (ADR 26-11-11-215). Its
// shape is discobox's sandbox.ProviderConfigField, so a provider that
// embeds the engine can list a driver's fields as its own, plus Default,
// Env, Privileged, and Retired.
type ConfigField struct {
	// Key is the field's key in the JSON object, in lowerCamelCase.
	Key string `json:"key"`
	// Label names the field for a person.
	Label string `json:"label"`
	// Type is ConfigString, ConfigBoolean, or ConfigNumber.
	Type string `json:"type"`
	// Description says what the field sets, and what the driver does
	// without it where Default is empty.
	Description string `json:"description,omitempty"`
	// Placeholder is an example of a value, for a form.
	Placeholder string `json:"placeholder,omitempty"`
	// Required fields must be set; Validate refuses a configuration
	// without one.
	Required bool `json:"required,omitempty"`
	// Advanced fields are for tests and unusual hosts.
	Advanced bool `json:"advanced,omitempty"`
	// Default is the value the driver uses when the field is absent, as
	// text; empty where the driver works it out, which Description says.
	Default string `json:"default,omitempty"`
	// Env is the environment variable the command line reads the field
	// from (ConfigFromEnv). Empty is none. A driver never reads it itself.
	Env string `json:"env,omitempty"`
	// Privileged fields name a host resource (a socket, a file, a
	// directory), or an endpoint the driver's key or a guest's data is
	// sent to. An embedder lets no one set one who could not be handed the
	// key and the host.
	Privileged bool `json:"privileged,omitempty"`
	// Retired fields are accepted for good and listed nowhere a person
	// reads: decoded into ReplacedBy's field, or, with none, ignored with a
	// warning. One given beside its replacement is refused.
	Retired bool `json:"retired,omitempty"`
	// ReplacedBy is the key a Retired field is decoded into.
	ReplacedBy string `json:"replacedBy,omitempty"`
}

// Definition describes a registered driver: what it runs, whether and what
// it takes as its base, the fields of its configuration, which are its own
// and no other driver's, and its secrets.
type Definition struct {
	Description string `json:"description,omitempty"`
	// Base describes Config.Base for this driver, as a field: what it
	// takes, its default, and whether it is Privileged (a host directory
	// that is booted). Its Key is empty and its Env BaseEnv: it is not
	// part of the driver's configuration. Nil is a driver that takes no
	// base (hcs), and refuses a fresh VM.
	Base         *ConfigField  `json:"base,omitempty"`
	ConfigFields []ConfigField `json:"configFields,omitempty"`
	// Secrets are the secrets the driver takes, apart from its
	// configuration (ADR 26-11-12-461): today at most one, its key, which
	// Config.Keys carries.
	Secrets []SecretField `json:"secrets,omitempty"`
}

// TakesBase reports whether the driver makes a fresh VM from a base
// (Config.Base); one that does not refuses a fresh VM, whatever it is given.
func (d Definition) TakesBase() bool { return d.Base != nil }

// Secret is the definition's secret of the key.
func (d Definition) Secret(key string) (SecretField, bool) {
	i := slices.IndexFunc(d.Secrets, func(s SecretField) bool { return s.Key == key })
	if i < 0 {
		return SecretField{}, false
	}
	return d.Secrets[i], true
}

// Field is the definition's field of the key, retired or not.
func (d Definition) Field(key string) (ConfigField, bool) {
	i := slices.IndexFunc(d.ConfigFields, func(f ConfigField) bool { return f.Key == key })
	if i < 0 {
		return ConfigField{}, false
	}
	return d.ConfigFields[i], true
}

// Current is the definition's fields that are not retired, in the order it
// declares them: what a person is shown.
func (d Definition) Current() []ConfigField {
	return slices.DeleteFunc(slices.Clone(d.ConfigFields), func(f ConfigField) bool { return f.Retired })
}

// Keys are the keys of the definition's current fields, in order.
func (d Definition) Keys() []string {
	var keys []string
	for _, f := range d.Current() {
		keys = append(keys, f.Key)
	}
	return keys
}

// check refuses a definition a driver should never register: a field with
// no key, a key twice, a type that is not one of the three, a retired field
// whose replacement is missing, retired, of another type, or more
// privileged than it, a base described with a key, or a secret malformed,
// more than one, or with a field's key.
func (d Definition) check() error {
	seen := map[string]bool{}
	for _, f := range d.ConfigFields {
		if f.Key == "" {
			return errors.New("a field has no key")
		}
		if seen[f.Key] {
			return fmt.Errorf("field %q twice", f.Key)
		}
		seen[f.Key] = true
		switch f.Type {
		case ConfigString, ConfigBoolean, ConfigNumber:
		default:
			return fmt.Errorf("field %q has type %q", f.Key, f.Type)
		}
		if f.ReplacedBy != "" && !f.Retired {
			return fmt.Errorf("field %q is replaced but not retired", f.Key)
		}
	}
	for _, f := range d.ConfigFields {
		if f.ReplacedBy == "" {
			continue
		}
		r, ok := d.Field(f.ReplacedBy)
		switch {
		case !ok || r.Retired:
			return fmt.Errorf("retired field %q is replaced by %q, which is not a current field", f.Key, f.ReplacedBy)
		case r.Type != f.Type:
			return fmt.Errorf("retired field %q is not of its replacement's type", f.Key)
		case r.Privileged != f.Privileged:
			return fmt.Errorf("retired field %q is not as privileged as its replacement", f.Key)
		}
	}
	if d.Base != nil && d.Base.Key != "" {
		return errors.New("the base is not a field of the configuration: it has no key")
	}
	if len(d.Secrets) > 1 {
		return errors.New("more than one secret: a driver's secret is its key, which Config.Keys carries")
	}
	for _, s := range d.Secrets {
		if seen[s.Key] {
			return fmt.Errorf("secret %q is a field's key", s.Key)
		}
		if err := s.check(d.ConfigFields); err != nil {
			return err
		}
	}
	return nil
}

// Validator checks a driver's configuration, as Normalize leaves it: that
// it decodes, and that the driver can run with what it says. It touches
// nothing outside the process, and its errors name a field, never a value.
type Validator func(config json.RawMessage) error

// DecodeConfig decodes a driver's configuration into its own type, refusing
// a key the type does not have, so a misspelled field is an error rather
// than ignored. An empty configuration is T's zero value. Its errors name a
// field or an offset, never a value.
func DecodeConfig[T any](driver string, config json.RawMessage) (T, error) {
	var out T
	if len(bytes.TrimSpace(config)) == 0 {
		return out, nil
	}
	dec := json.NewDecoder(bytes.NewReader(config))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, configError(driver, err)
	}
	if dec.More() {
		return out, fmt.Errorf("%s: configuration: more than one JSON value", driver)
	}
	return out, nil
}

// configError is a decoding error that names where, never what: a syntax
// error's own message quotes the character it stopped at.
func configError(driver string, err error) error {
	if se, ok := errors.AsType[*json.SyntaxError](err); ok {
		return fmt.Errorf("%s: configuration: not JSON (at byte %d)", driver, se.Offset)
	}
	if te, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		return fmt.Errorf("%s: configuration: %s: not a %s", driver, te.Field, te.Type)
	}
	return fmt.Errorf("%s: configuration: %w", driver, err)
}

// Validate checks config against the named driver's definition and its
// Validator, as New does before it constructs.
func Validate(driver string, config json.RawMessage) error {
	_, err := Normalize(driver, config)
	return err
}

// Normalize checks config against the named driver's definition and its
// Validator, and returns it as the driver is given it: an object of its
// current fields, every retired one decoded into its replacement or
// dropped. It refuses a key the definition does not list, a retired key
// beside its replacement, and a required field missing. An empty
// configuration is nil.
func Normalize(driver string, config json.RawMessage) (json.RawMessage, error) {
	reg, err := lookup(driver)
	if err != nil {
		return nil, err
	}
	def := reg.Definition
	fields := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(config)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(config))
		if err := dec.Decode(&fields); err != nil {
			return nil, configError(driver, err)
		}
		if fields == nil || dec.More() {
			return nil, fmt.Errorf("%s: configuration: not one JSON object", driver)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		f, ok := def.Field(key)
		if _, secret := def.Secret(key); secret {
			return nil, fmt.Errorf("%s: configuration: %s is a secret, given apart from the configuration as the driver's key, never in it", driver, key)
		}
		if !ok {
			return nil, unknownField(driver, key, def)
		}
		if !f.Retired {
			continue
		}
		v := fields[key]
		delete(fields, key)
		if f.ReplacedBy == "" {
			slog.Warn("a retired field of the driver's configuration is ignored", "driver", driver, "field", key)
			continue
		}
		if _, both := fields[f.ReplacedBy]; both {
			return nil, fmt.Errorf("%s: configuration: %s is retired for %s, and both are given", driver, key, f.ReplacedBy)
		}
		fields[f.ReplacedBy] = v
	}
	for _, f := range def.Current() {
		if _, ok := fields[f.Key]; f.Required && !ok {
			return nil, fmt.Errorf("%s: configuration: %s is required", driver, f.Key)
		}
	}
	var out json.RawMessage
	if len(fields) > 0 {
		if out, err = json.Marshal(fields); err != nil {
			return nil, err
		}
	}
	if err := reg.Validate(out); err != nil {
		return nil, err
	}
	return out, nil
}

func unknownField(driver, key string, def Definition) error {
	if len(def.Current()) == 0 {
		return fmt.Errorf("%s: configuration: unknown field %q: the driver takes none", driver, key)
	}
	return fmt.Errorf("%s: configuration: unknown field %q (the driver takes %s)", driver, key, strings.Join(def.Keys(), ", "))
}

// CanonicalConfig is config as one string for comparing two: its keys
// sorted, its spacing gone, and none the same as an empty object.
func CanonicalConfig(config json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(config)) == 0 {
		return "{}", nil
	}
	var v any
	if err := json.Unmarshal(config, &v); err != nil {
		return "", errors.New("configuration: not JSON")
	}
	out, err := json.Marshal(v)
	return string(out), err
}

// ConfigFromValues is the named driver's configuration from values given as
// text, by key, as a flag or an environment variable gives them: each
// converted to its field's type. A key the driver does not define is
// refused. No values is no configuration (nil).
func ConfigFromValues(driver string, values map[string]string) (json.RawMessage, error) {
	if len(values) == 0 {
		return nil, nil
	}
	def, err := DefinitionOf(driver)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(values))
	for key, text := range values {
		f, ok := def.Field(key)
		if !ok {
			return nil, unknownField(driver, key, def)
		}
		v, err := f.Parse(text)
		if err != nil {
			return nil, fmt.Errorf("%s: configuration: %s: not a %s", driver, key, f.Type)
		}
		out[key] = v
	}
	return json.Marshal(out)
}

// Parse converts text to the field's type.
func (f ConfigField) Parse(text string) (any, error) {
	switch f.Type {
	case ConfigBoolean:
		return strconv.ParseBool(strings.TrimSpace(text))
	case ConfigNumber:
		return strconv.ParseFloat(strings.TrimSpace(text), 64)
	default:
		return text, nil
	}
}

// ConfigFromEnv is the named driver's fields that the environment sets, by
// key, as text, for the command line: each field's Env, retired or not,
// where it is set and not empty. A driver never reads the environment for
// its configuration; it is given it.
func ConfigFromEnv(driver string) map[string]string {
	def, err := DefinitionOf(driver)
	if err != nil {
		return nil
	}
	values := map[string]string{}
	for _, f := range def.ConfigFields {
		if f.Env == "" {
			continue
		}
		if v := os.Getenv(f.Env); v != "" {
			values[f.Key] = v
		}
	}
	return values
}

// ConfigEnvDisagrees names the environment variables of the named driver's
// fields that are set and say otherwise than config, which the command line
// warns of when it is given a whole configuration and reads none of them.
// A field config leaves out says its Default.
func ConfigEnvDisagrees(driver string, config json.RawMessage) []string {
	def, err := DefinitionOf(driver)
	if err != nil {
		return nil
	}
	given := map[string]any{}
	if len(bytes.TrimSpace(config)) > 0 && json.Unmarshal(config, &given) != nil {
		return nil
	}
	var names []string
	for _, f := range def.ConfigFields {
		env := os.Getenv(f.Env)
		if f.Env == "" || env == "" {
			continue
		}
		want, err := f.Parse(env)
		if err != nil {
			names = append(names, f.Env)
			continue
		}
		key := f.Key
		if f.ReplacedBy != "" {
			key = f.ReplacedBy
		}
		got, ok := given[key]
		if !ok {
			if f.Default == "" {
				names = append(names, f.Env)
				continue
			}
			got, _ = f.Parse(f.Default)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			names = append(names, f.Env)
		}
	}
	return names
}

// CheckKeyURL refuses a URL a driver sends its key to, by its field's name
// and never its value: one that is not https, or http to a loopback
// address for a test; or one with userinfo, a query, or a fragment.
func CheckKeyURL(driver, field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%s: configuration: %s: not a URL", driver, field)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s: configuration: %s: a URL the key is sent to has no userinfo, query, or fragment", driver, field)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if host := u.Hostname(); host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
			return nil
		}
	}
	return fmt.Errorf("%s: configuration: %s: a URL the key is sent to is https, or http to a loopback address", driver, field)
}

// CheckFile refuses a path a driver is about to read and hand a guest (an
// agent) that is not an absolute path to a regular file. Its errors name
// the path, which the configuration gave, and nothing in the file.
func CheckFile(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s is not an absolute path", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	return nil
}
