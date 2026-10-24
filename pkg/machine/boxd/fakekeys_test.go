package boxd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/vm/internal/fsutil"
	"github.com/discobox-ai/vm/pkg/machine/boxd/internal/boxdapi"
)

// fakeSession is the bearer of an interactive session (`boxd login`), the
// only credential boxd lets manage API keys: it is a JWT already, so the
// driver sends it as it is.
const fakeSession = "session-of-the-owner"

// refusedKeyManagement is boxd's own answer to key management with an API
// key (by hand, 2026-10-24).
const refusedKeyManagement = "this operation requires an interactive session (`boxd login`) — API keys are fenced to one org and cannot manage the account"

// fakeKey is an API key, as the fake API keeps it.
type fakeKey struct {
	id, name, secret, kind string
	expires                time.Time
}

func (k *fakeKey) live() bool { return k.expires.IsZero() || time.Now().Before(k.expires) }

// keys holds the fake API's API keys and the JWTs exchanged for them.
type keys struct {
	byID map[string]*fakeKey
	jwts map[string]string // JWT to key ID
}

// serveTokens is boxd's token exchange: a live API key for a JWT.
func (f *fakeAPI) serveTokens(w http.ResponseWriter, r *http.Request) {
	var in struct {
		APIKey string `json:"api_key"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&in) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys.byID {
		if k.secret == in.APIKey && k.live() {
			jwt := "jwt_" + fsutil.RandomHex(8)
			f.keys.jwts[jwt] = k.id
			if err := json.NewEncoder(w).Encode(struct {
				Token     string `json:"token"`
				ExpiresAt int64  `json:"expires_at"`
			}{jwt, time.Now().Add(time.Hour).Unix()}); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
			return
		}
	}
	http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
}

// caller is the key a call is made with: "" for the session, or an API key's
// ID. A bearer that is neither, or a key revoked or expired, is refused.
func (f *fakeAPI) caller(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	bearer, _ := strings.CutPrefix(strings.Join(md.Get("authorization"), ""), "Bearer ")
	if bearer == fakeSession {
		return "", nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.keys.jwts[bearer]; ok {
		if k, ok := f.keys.byID[id]; ok && k.live() {
			return id, nil
		}
	}
	return "", status.Error(codes.Unauthenticated, "invalid or expired token")
}

func (f *fakeAPI) unaryAuth(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	id, err := f.caller(ctx)
	if err != nil {
		return nil, err
	}
	return handler(context.WithValue(ctx, callerKey{}, id), req)
}

func (f *fakeAPI) streamAuth(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if _, err := f.caller(ss.Context()); err != nil {
		return err
	}
	return handler(srv, ss)
}

type callerKey struct{}

// session refuses key management to anything but the session.
func session(ctx context.Context) error {
	if id, _ := ctx.Value(callerKey{}).(string); id != "" {
		return status.Error(codes.PermissionDenied, refusedKeyManagement)
	}
	return nil
}

//nolint:revive // boxd's RPC name
func (f *fakeAPI) CreateApiKey(ctx context.Context, req *boxdapi.CreateApiKeyRequest) (*boxdapi.CreateApiKeyResponse, error) {
	if err := session(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := &fakeKey{id: "key_" + fsutil.RandomHex(4), name: req.GetName(), secret: "bxd_" + fsutil.RandomHex(16), kind: req.GetKind()}
	if k.kind == "" {
		k.kind = "member"
	}
	if secs := req.GetExpiresInSecs(); secs != 0 {
		k.expires = time.Now().Add(time.Duration(secs) * time.Second)
	}
	f.keys.byID[k.id] = k
	return &boxdapi.CreateApiKeyResponse{Id: k.id, ApiKey: k.secret, ExpiresAt: unixOrZero(k.expires)}, nil
}

//nolint:revive // boxd's RPC name
func (f *fakeAPI) ListApiKeys(ctx context.Context, _ *boxdapi.ListApiKeysRequest) (*boxdapi.ListApiKeysResponse, error) {
	if err := session(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := &boxdapi.ListApiKeysResponse{}
	for _, k := range f.keys.byID {
		out.Keys = append(out.Keys, &boxdapi.ApiKeyInfo{
			Id: k.id, Name: k.name, KeyPrefix: k.secret[:8], ExpiresAt: unixOrZero(k.expires), Kind: k.kind, Mine: true, Org: "fake-org",
		})
	}
	return out, nil
}

//nolint:revive // boxd's RPC name
func (f *fakeAPI) DeleteApiKey(ctx context.Context, req *boxdapi.DeleteApiKeyRequest) (*boxdapi.DeleteApiKeyResponse, error) {
	if err := session(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.keys.byID[req.GetId()]; !ok {
		return nil, status.Errorf(codes.NotFound, "no API key %q", req.GetId())
	}
	delete(f.keys.byID, req.GetId())
	return &boxdapi.DeleteApiKeyResponse{}, nil
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// tokenServer serves the fake API's token exchange.
func (f *fakeAPI) tokenServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(f.serveTokens))
}
