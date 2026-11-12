package machine

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
)

// ConfigSecret is a SecretField's type, as discobox's
// sandbox.ProviderConfigField spells a secret. It is never a ConfigField's:
// a secret is not part of a driver's configuration.
const ConfigSecret = "secret"

// The protocols a driver sends a secret over (SecretSend.Protocol).
const (
	// SendHTTPS is an HTTP/1.1 or HTTP/2 request over TLS, or plain HTTP to
	// a loopback address for a test (CheckKeyURL).
	SendHTTPS = "https"
	// SendGRPC is a gRPC call: an HTTP/2 POST over TLS to
	// /PACKAGE.SERVICE/METHOD, its headers gRPC metadata.
	SendGRPC = "grpc"
)

// SecretField describes a secret a driver takes (ADR 26-11-12-461): its
// key, the credential it calls its provider with. A secret is written and
// never read back: a definition holds where its value comes from and where
// it is sent, never the value; it is given apart from the configuration
// (Config.Keys), and nothing disco-vm prints, logs, returns, or writes to
// disk carries it, only the driver's own calls to its provider. Its shape is
// discobox's sandbox.ProviderConfigField of type secret, plus Env and Sends.
type SecretField struct {
	// Key names the secret, in lowerCamelCase. No configuration field has
	// it: one given in a configuration is refused, naming it.
	Key string `json:"key"`
	// Label names the secret for a person.
	Label string `json:"label"`
	// Type is ConfigSecret.
	Type string `json:"type"`
	// Description says what the secret is and where to get one.
	Description string `json:"description,omitempty"`
	// Placeholder is an example of its shape (bxd_...), for a form; never
	// a working value.
	Placeholder string `json:"placeholder,omitempty"`
	// Env is the environment variables the command line reads it from
	// (KeyFromEnv), in order, and leaves out of every process the driver
	// starts (WithoutKeys). A driver never reads them itself.
	Env []string `json:"env,omitempty"`
	// Sends is every place the driver sends the secret. A secret sent
	// nowhere (fake's, checked in the process) has none. A send the driver
	// makes and its definition does not list is the driver's bug.
	Sends []SecretSend `json:"sends,omitempty"`
}

// SecretSend is one place a driver sends a secret: which hosts, over which
// protocol, by which method to which path, and where in the request. It is
// what a proxy that holds the real secret, and hands the driver a stand-in
// for it, needs to put the real one in its place, and to refuse the
// stand-in anywhere else.
type SecretSend struct {
	// Prefix limits the send to a secret that starts with it. An empty
	// Prefix is a secret no other send of the field's Prefix matches: the
	// sends of one secret are its forms (boxd's API key is exchanged for a
	// token, and a token is sent as it is).
	Prefix string `json:"prefix,omitempty"`
	// Endpoint is the configuration field whose value names the host the
	// secret is sent to: a URL, or HOST:PORT. Unset, it is the field's
	// Default, or else Hosts.
	Endpoint string `json:"endpoint,omitempty"`
	// Hosts are where the secret is sent, each HOST:PORT, when Endpoint is
	// empty or names a field that is unset and has no Default: every host
	// it can be sent to, which may be more than one configuration's.
	Hosts []string `json:"hosts,omitempty"`
	// Listed sends the secret also to hosts the endpoint names at run time
	// (boxd's region endpoints, which ListRegions names), which no
	// definition can list.
	Listed bool `json:"listed,omitempty"`
	// Protocol is SendHTTPS or SendGRPC.
	Protocol string `json:"protocol"`
	// Method is the HTTP method, and empty for any. A gRPC call is a POST.
	Method string `json:"method,omitempty"`
	// Path is the request's path: exact, or a prefix where it ends in "/".
	// Where Endpoint is a URL, it is below the URL's own path, and empty
	// is the URL's path exactly.
	Path string `json:"path,omitempty"`
	// Header is the request header (gRPC: metadata key) whose value
	// carries the secret, as ValuePrefix then the secret without
	// TrimPrefix. Exactly one of Header and JSONField is set.
	Header string `json:"header,omitempty"`
	// ValuePrefix comes before the secret in Header's value ("Bearer ").
	ValuePrefix string `json:"valuePrefix,omitempty"`
	// TrimPrefix is taken off the secret, if it starts with it, before it
	// is put after ValuePrefix (archil's "key-", which ValuePrefix holds).
	TrimPrefix string `json:"trimPrefix,omitempty"`
	// JSONField is the top-level key of a JSON object request body whose
	// string value is the secret.
	JSONField string `json:"jsonField,omitempty"`
}

// check refuses a secret a driver should never register.
func (s SecretField) check(fields []ConfigField) error {
	if s.Key == "" {
		return errors.New("a secret has no key")
	}
	if s.Type != ConfigSecret {
		return fmt.Errorf("secret %q has type %q", s.Key, s.Type)
	}
	if len(s.Env) == 0 {
		return fmt.Errorf("secret %q is read from no environment variable", s.Key)
	}
	for i, send := range s.Sends {
		if err := send.check(fields); err != nil {
			return fmt.Errorf("secret %q, send %d: %w", s.Key, i, err)
		}
	}
	return nil
}

func (s SecretSend) check(fields []ConfigField) error {
	switch s.Protocol {
	case SendHTTPS, SendGRPC:
	default:
		return fmt.Errorf("protocol %q", s.Protocol)
	}
	if (s.Header == "") == (s.JSONField == "") {
		return errors.New("not exactly one of a header and a JSON field")
	}
	if s.JSONField != "" && (s.ValuePrefix != "" || s.TrimPrefix != "") {
		return errors.New("a JSON field's value is the secret alone")
	}
	if s.Path != "" && !strings.HasPrefix(s.Path, "/") {
		return fmt.Errorf("path %q is not absolute", s.Path)
	}
	for _, h := range s.Hosts {
		if _, _, err := net.SplitHostPort(h); err != nil {
			return fmt.Errorf("host %q is not HOST:PORT", h)
		}
	}
	if s.Endpoint == "" {
		if len(s.Hosts) == 0 {
			return errors.New("no endpoint and no hosts")
		}
		return nil
	}
	i := slices.IndexFunc(fields, func(f ConfigField) bool { return f.Key == s.Endpoint })
	switch {
	case i < 0 || fields[i].Retired || fields[i].Type != ConfigString:
		return fmt.Errorf("endpoint %q is not a current text field", s.Endpoint)
	case !fields[i].Privileged:
		return fmt.Errorf("endpoint %q is not privileged, and a secret is sent to it", s.Endpoint)
	case fields[i].Default == "" && len(s.Hosts) == 0:
		return fmt.Errorf("endpoint %q has no default and the send no hosts", s.Endpoint)
	}
	return nil
}

// SecretSends is where the named driver sends its secrets under config:
// each send with Hosts the concrete HOST:PORTs it reaches, Endpoint
// resolved (the field's value, else its Default, else Hosts as listed), and
// Path the full path, below the endpoint URL's. A send that is Listed
// reaches also the hosts its endpoint names at run time. config is
// normalized first, so it is refused as New refuses it.
func SecretSends(driver string, config json.RawMessage) (map[string][]SecretSend, error) {
	config, err := Normalize(driver, config)
	if err != nil {
		return nil, err
	}
	def, err := DefinitionOf(driver)
	if err != nil {
		return nil, err
	}
	values := map[string]any{}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &values); err != nil {
			return nil, configError(driver, err)
		}
	}
	out := map[string][]SecretSend{}
	for _, s := range def.Secrets {
		sends := []SecretSend{}
		for _, send := range s.Sends {
			resolved, err := send.resolve(driver, def, values)
			if err != nil {
				return nil, err
			}
			sends = append(sends, resolved)
		}
		out[s.Key] = sends
	}
	return out, nil
}

// resolve is the send under a configuration's values.
func (s SecretSend) resolve(driver string, def Definition, values map[string]any) (SecretSend, error) {
	if s.Endpoint == "" {
		return s, nil
	}
	f, _ := def.Field(s.Endpoint)
	value, _ := values[s.Endpoint].(string)
	if value == "" {
		value = f.Default
	}
	if value == "" {
		return s, nil
	}
	host, base, err := endpointHost(value)
	if err != nil {
		return s, fmt.Errorf("%s: configuration: %s: %w", driver, s.Endpoint, err)
	}
	s.Hosts = []string{host}
	switch {
	case s.Path == "":
		s.Path = base
	case base != "":
		s.Path = strings.TrimRight(base, "/") + s.Path
	}
	if s.Path == "" {
		s.Path = "/"
	}
	return s, nil
}

// endpointHost is the HOST:PORT and path of an endpoint given as a URL, at
// its scheme's port when it names none, or as HOST:PORT, with no path.
func endpointHost(value string) (host, path string, err error) {
	if !strings.Contains(value, "://") {
		if _, _, err := net.SplitHostPort(value); err != nil {
			return "", "", errors.New("not HOST:PORT")
		}
		return value, "", nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" {
		return "", "", errors.New("not a URL")
	}
	port := u.Port()
	if port == "" {
		if port = map[string]string{"https": "443", "http": "80"}[u.Scheme]; port == "" {
			return "", "", errors.New("names no port")
		}
	}
	return net.JoinHostPort(u.Hostname(), port), u.Path, nil
}
