package release

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeGitHub is the part of GitHub's REST API a release uses: releases, their
// assets, and a repository's contents.
type fakeGitHub struct {
	t        *testing.T
	mu       sync.Mutex
	srv      *httptest.Server
	nextID   int64
	releases []*ghRelease
	data     map[int64][]byte // asset ID to its bytes
	contents map[string][]byte
	puts     int
	token    string
	// makeLatest is what each release's publish said of make_latest.
	makeLatest map[string]string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{t: t, data: map[int64][]byte{}, contents: map[string][]byte{}, token: "secret", makeLatest: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) client() *GitHub {
	return &GitHub{API: f.srv.URL, Token: f.token}
}

func (f *fakeGitHub) id() int64 {
	f.nextID++
	return f.nextID
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		http.Error(w, "bad credentials", http.StatusUnauthorized)
		return
	}
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			f.t.Error(err)
		}
	}
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && p == "/repos/o/r/releases":
		out := make([]*ghRelease, 0, len(f.releases))
		for _, rel := range slices.Backward(f.releases) {
			out = append(out, rel)
		}
		reply(out)
	case r.Method == http.MethodPost && p == "/repos/o/r/releases":
		var in struct {
			TagName    string `json:"tag_name"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		id := f.id()
		rel := &ghRelease{ID: id, TagName: in.TagName, Draft: in.Draft, Prerelease: in.Prerelease,
			UploadURL: fmt.Sprintf("%s/uploads/%d{?name,label}", f.srv.URL, id), HTMLURL: "https://example.test/" + in.TagName}
		f.releases = append(f.releases, rel)
		w.WriteHeader(http.StatusCreated)
		reply(rel)
	case r.Method == http.MethodPatch && strings.HasPrefix(p, "/repos/o/r/releases/"):
		rel := f.release(strings.TrimPrefix(p, "/repos/o/r/releases/"))
		if rel == nil {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Draft      *bool  `json:"draft"`
			Prerelease *bool  `json:"prerelease"`
			MakeLatest string `json:"make_latest"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.makeLatest[rel.TagName] = in.MakeLatest
		if in.Draft != nil {
			rel.Draft = *in.Draft
		}
		if in.Prerelease != nil {
			rel.Prerelease = *in.Prerelease
		}
		reply(rel)
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/repos/o/r/releases/assets/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(p, "/repos/o/r/releases/assets/"), 10, 64)
		for _, rel := range f.releases {
			for i, a := range rel.Assets {
				if a.ID == id {
					rel.Assets = append(rel.Assets[:i], rel.Assets[i+1:]...)
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
		}
		http.NotFound(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/uploads/"):
		rel := f.release(strings.TrimPrefix(p, "/uploads/"))
		name := r.URL.Query().Get("name")
		for _, a := range rel.Assets {
			if a.Name == name {
				http.Error(w, "already_exists", http.StatusUnprocessableEntity)
				return
			}
		}
		data, _ := io.ReadAll(r.Body)
		id := f.id()
		f.data[id] = data
		a := ghAsset{ID: id, Name: name, URL: fmt.Sprintf("%s/assets/%d", f.srv.URL, id)}
		rel.Assets = append(rel.Assets, a)
		w.WriteHeader(http.StatusCreated)
		reply(a)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/assets/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(p, "/assets/"), 10, 64)
		if r.Header.Get("Accept") != "application/octet-stream" {
			http.Error(w, "want the asset's bytes", http.StatusBadRequest)
			return
		}
		_, _ = w.Write(f.data[id])
	case strings.HasPrefix(p, "/repos/o/tap/contents/"):
		path := strings.TrimPrefix(p, "/repos/o/tap/contents/")
		sha := func(b []byte) string { return fmt.Sprintf("sha-%d", len(b)) }
		old, ok := f.contents[path]
		switch r.Method {
		case http.MethodGet:
			if !ok {
				http.NotFound(w, r)
				return
			}
			enc := base64.StdEncoding.EncodeToString(old)
			var wrapped strings.Builder
			for len(enc) > 60 {
				wrapped.WriteString(enc[:60] + "\n")
				enc = enc[60:]
			}
			wrapped.WriteString(enc)
			reply(map[string]string{"sha": sha(old), "content": wrapped.String()})
		case http.MethodPut:
			var in struct{ Message, Content, SHA string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			if ok && in.SHA != sha(old) || !ok && in.SHA != "" {
				http.Error(w, "sha does not match", http.StatusConflict)
				return
			}
			data, _ := base64.StdEncoding.DecodeString(in.Content)
			f.contents[path] = data
			f.puts++
			reply(map[string]any{})
		}
	default:
		http.Error(w, "the fake does not serve "+r.Method+" "+p, http.StatusNotImplemented)
	}
}

func (f *fakeGitHub) release(id string) *ghRelease {
	for _, rel := range f.releases {
		if strconv.FormatInt(rel.ID, 10) == id {
			return rel
		}
	}
	return nil
}

func (f *fakeGitHub) asset(tag, name string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, rel := range f.releases {
		if rel.TagName != tag {
			continue
		}
		for _, a := range rel.Assets {
			if a.Name == name {
				return f.data[a.ID]
			}
		}
	}
	return nil
}

// releaseDir is a build/release a release:build and assets would leave.
func releaseDir(t *testing.T, tag string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range Binaries {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+" "+tag), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := Write(dir, tag); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestPublishMakesARelease(t *testing.T) {
	ctx := context.Background()
	for _, tag := range []string{"v1.0.0", "v1.1.0-rc.1"} {
		f := newFakeGitHub(t)
		dir := releaseDir(t, tag)
		if err := f.client().Publish(ctx, "o/r", tag, dir); err != nil {
			t.Fatal(err)
		}
		if len(f.releases) != 1 {
			t.Fatalf("%s: made %d releases", tag, len(f.releases))
		}
		rel := f.releases[0]
		if rel.Draft || rel.Prerelease != Prerelease(tag) {
			t.Errorf("%s: draft %v, prerelease %v", tag, rel.Draft, rel.Prerelease)
		}
		for _, name := range Assets() {
			want, _ := os.ReadFile(filepath.Join(dir, name))
			if got := f.asset(tag, name); string(got) != string(want) {
				t.Errorf("%s: %s uploaded as %q", tag, name, got)
			}
		}
	}
}

func TestPublishFinishesADraftAndLeavesAPublishedRelease(t *testing.T) {
	ctx := context.Background()
	f := newFakeGitHub(t)
	// A run that failed part way: a draft holding one stale file.
	f.releases = append(f.releases, &ghRelease{ID: 100, TagName: "v1.0.0", Draft: true,
		UploadURL: f.srv.URL + "/uploads/100{?name,label}",
		Assets:    []ghAsset{{ID: 101, Name: ChecksumsName}}})
	f.data[101] = []byte("stale")
	f.nextID = 200
	dir := releaseDir(t, "v1.0.0")
	if err := f.client().Publish(ctx, "o/r", "v1.0.0", dir); err != nil {
		t.Fatal(err)
	}
	if len(f.releases) != 1 || f.releases[0].Draft {
		t.Fatalf("the draft was not finished: %d releases", len(f.releases))
	}
	want, _ := os.ReadFile(filepath.Join(dir, ChecksumsName))
	if got := f.asset("v1.0.0", ChecksumsName); string(got) != string(want) {
		t.Errorf("checksums.txt is %q", got)
	}

	// Published now: another run's files never replace it.
	other := releaseDir(t, "v1.0.0")
	if err := os.WriteFile(filepath.Join(other, Binaries[0]), []byte("rebuilt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.client().Publish(ctx, "o/r", "v1.0.0", other); err != nil {
		t.Fatal(err)
	}
	if got := f.asset("v1.0.0", Binaries[0]); string(got) == "rebuilt" {
		t.Error("a published binary was replaced")
	}
}

func TestPublishNeedsEveryFile(t *testing.T) {
	f := newFakeGitHub(t)
	dir := releaseDir(t, "v1.0.0")
	if err := os.Remove(filepath.Join(dir, PowerShellName)); err != nil {
		t.Fatal(err)
	}
	if err := f.client().Publish(context.Background(), "o/r", "v1.0.0", dir); err == nil {
		t.Fatal("published a release without install.ps1")
	}
	if len(f.releases) != 0 {
		t.Error("made a release before it had every file")
	}
}

func TestTheTapIsToldWhatTheReleaseUploaded(t *testing.T) {
	ctx := context.Background()
	f := newFakeGitHub(t)
	dir := releaseDir(t, "v1.0.0")
	gh := f.client()
	if err := gh.Publish(ctx, "o/r", "v1.0.0", dir); err != nil {
		t.Fatal(err)
	}
	digests, err := gh.Checksums(ctx, "o/r", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	local, err := Hash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(digests) != fmt.Sprint(local) {
		t.Fatalf("read %v, uploaded %v", digests, local)
	}
	formula, err := Formula("o/r", "v1.0.0", digests)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := gh.PutFile(ctx, "o/tap", FormulaPath, "disco-vm v1.0.0", formula); err != nil {
			t.Fatal(err)
		}
	}
	if f.puts != 1 || string(f.contents[FormulaPath]) != string(formula) {
		t.Errorf("committed %d times, formula %q", f.puts, f.contents[FormulaPath])
	}
	// The next release replaces it.
	formula2, err := Formula("o/r", "v1.0.1", digests)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := gh.PutFile(ctx, "o/tap", FormulaPath, "disco-vm v1.0.1", formula2); err != nil || !changed {
		t.Fatalf("changed %v: %v", changed, err)
	}
	if string(f.contents[FormulaPath]) != string(formula2) {
		t.Error("the formula was not replaced")
	}

	if _, err := gh.Checksums(ctx, "o/r", "v9.9.9"); err == nil {
		t.Error("read the checksums of a release that does not exist")
	}
}

func TestAFixOnAnOlderLineIsNotTheLatestRelease(t *testing.T) {
	ctx := context.Background()
	f := newFakeGitHub(t)
	gh := f.client()
	for _, tag := range []string{"v1.0.0", "v2.0.0", "v1.0.1", "v2.1.0-rc.1"} {
		if err := gh.Publish(ctx, "o/r", tag, releaseDir(t, tag)); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{"v1.0.0": "true", "v2.0.0": "true", "v1.0.1": "false", "v2.1.0-rc.1": "false"}
	for tag, latest := range want {
		if f.makeLatest[tag] != latest {
			t.Errorf("%s published with make_latest %q, want %q", tag, f.makeLatest[tag], latest)
		}
	}
	for tag, newer := range map[string]string{"v1.0.1": "v2.0.0", "v2.0.0": "", "v1.0.0": "v2.0.0"} {
		got, err := gh.NewerRelease(ctx, "o/r", tag)
		if err != nil {
			t.Fatal(err)
		}
		if got != newer {
			t.Errorf("NewerRelease(%s) = %q, want %q (a prerelease is never newer)", tag, got, newer)
		}
	}
}
