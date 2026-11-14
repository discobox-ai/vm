package release

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// CheckRepo refuses a name that is not owner/repository.
func CheckRepo(repo string) error {
	if !repoPattern.MatchString(repo) {
		return fmt.Errorf("%q is not an owner/repository", repo)
	}
	return nil
}

// GitHub is the part of GitHub's REST API a release uses, as one token.
type GitHub struct {
	// API is the REST API's root, https://api.github.com unless a test or
	// GitHub Enterprise says otherwise.
	API   string
	Token string
	// Log is told each step, for the release job's log.
	Log  io.Writer
	HTTP *http.Client
}

type ghRelease struct {
	ID         int64     `json:"id"`
	TagName    string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	UploadURL  string    `json:"upload_url"`
	HTMLURL    string    `json:"html_url"`
	Assets     []ghAsset `json:"assets"`
}

type ghAsset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

var errNotFound = errors.New("not found")

func (g *GitHub) logf(format string, args ...any) {
	if g.Log != nil {
		fmt.Fprintf(g.Log, format+"\n", args...)
	}
}

func (g *GitHub) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return http.DefaultClient
}

// do sends a request to the API, or to an absolute URL it handed out, and
// decodes a JSON answer into out. A 404 is errNotFound.
func (g *GitHub) do(ctx context.Context, method, path string, body io.Reader, contentType string, size int64, out any) error {
	u := path
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		u = strings.TrimSuffix(g.API, "/") + path
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if size >= 0 && body != nil {
		req.ContentLength = size
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, u, errNotFound)
	}
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %s: %s", method, u, resp.Status, bytes.TrimSpace(msg))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (g *GitHub) doJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	size := int64(-1)
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, size = bytes.NewReader(data), int64(len(data))
	}
	return g.do(ctx, method, path, body, "application/json", size, out)
}

// findRelease returns repo's release of tag, a draft included, which
// GitHub's lookup by tag does not find. Only the newest hundred are looked
// at: a release is published moments after its tag is pushed.
func (g *GitHub) findRelease(ctx context.Context, repo, tag string) (*ghRelease, error) {
	var releases []ghRelease
	if err := g.doJSON(ctx, http.MethodGet, "/repos/"+repo+"/releases?per_page=100", nil, &releases); err != nil {
		return nil, err
	}
	for i := range releases {
		if releases[i].TagName == tag {
			return &releases[i], nil
		}
	}
	return nil, nil
}

// Publish makes repo's GitHub Release of tag from the files in dir that
// Assets names: a prerelease for an -alpha or -rc tag, a full release
// otherwise. It is made as a draft, given every file, and only then
// published, so the newest release never lacks its installer or a binary. A
// run after one that failed part way finds that draft and finishes it. A
// release already published is left as it is, so a binary a user may have
// installed is never replaced under its digest.
func (g *GitHub) Publish(ctx context.Context, repo, tag, dir string) error {
	if err := CheckTag(tag); err != nil {
		return err
	}
	if err := CheckRepo(repo); err != nil {
		return err
	}
	for _, name := range Assets() {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("release %s: %w", tag, err)
		}
	}
	rel, err := g.findRelease(ctx, repo, tag)
	if err != nil {
		return err
	}
	if rel != nil && !rel.Draft {
		g.logf("%s is published already (%s); leaving its files as they are", tag, rel.HTMLURL)
		return nil
	}
	if rel == nil {
		rel = &ghRelease{}
		err := g.doJSON(ctx, http.MethodPost, "/repos/"+repo+"/releases", map[string]any{
			"tag_name":               tag,
			"name":                   tag,
			"draft":                  true,
			"prerelease":             Prerelease(tag),
			"generate_release_notes": true,
		}, rel)
		if err != nil {
			return err
		}
		g.logf("created a draft release of %s", tag)
	} else {
		g.logf("finishing the draft release of %s", tag)
	}

	upload, _, _ := strings.Cut(rel.UploadURL, "{")
	for _, name := range Assets() {
		for _, a := range rel.Assets {
			if a.Name == name {
				if err := g.doJSON(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/releases/assets/%d", repo, a.ID), nil, nil); err != nil {
					return err
				}
			}
		}
		if err := g.uploadAsset(ctx, upload, filepath.Join(dir, name), name); err != nil {
			return err
		}
		g.logf("uploaded %s", name)
	}

	// GitHub makes whichever full release is published last its latest,
	// which both installers install; a fix on an older line must not take
	// that from a newer release.
	makeLatest := "false"
	if !Prerelease(tag) {
		newer, err := g.NewerRelease(ctx, repo, tag)
		if err != nil {
			return err
		}
		if newer == "" {
			makeLatest = "true"
		} else {
			g.logf("%s is newer, so it stays the latest release", newer)
		}
	}
	err = g.doJSON(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/releases/%d", repo, rel.ID), map[string]any{
		"draft":       false,
		"prerelease":  Prerelease(tag),
		"make_latest": makeLatest,
	}, rel)
	if err != nil {
		return err
	}
	g.logf("published %s: %s", tag, rel.HTMLURL)
	return nil
}

// NewerRelease returns the newest of repo's published full releases above
// tag by version, or "" when there is none: tag is then the newest full
// release, the one GitHub's latest and the tap serve.
func (g *GitHub) NewerRelease(ctx context.Context, repo, tag string) (string, error) {
	var releases []ghRelease
	if err := g.doJSON(ctx, http.MethodGet, "/repos/"+repo+"/releases?per_page=100", nil, &releases); err != nil {
		return "", err
	}
	newest := ""
	for _, r := range releases {
		if r.Draft || r.Prerelease || CheckTag(r.TagName) != nil {
			continue
		}
		if semver.Compare(r.TagName, tag) > 0 && (newest == "" || semver.Compare(r.TagName, newest) > 0) {
			newest = r.TagName
		}
	}
	return newest, nil
}

func (g *GitHub) uploadAsset(ctx context.Context, upload, path, name string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return g.do(ctx, http.MethodPost, upload+"?name="+url.QueryEscape(name), f, "application/octet-stream", info.Size(), nil)
}

// Checksums returns the digests repo's published release of tag uploaded,
// read from its checksums.txt, so what the tap is told is what users
// download, whichever run built it.
func (g *GitHub) Checksums(ctx context.Context, repo, tag string) ([]Digest, error) {
	rel, err := g.findRelease(ctx, repo, tag)
	if err != nil {
		return nil, err
	}
	if rel == nil || rel.Draft {
		return nil, fmt.Errorf("%s has no published release of %s", repo, tag)
	}
	for _, a := range rel.Assets {
		if a.Name != ChecksumsName {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/octet-stream")
		if g.Token != "" {
			req.Header.Set("Authorization", "Bearer "+g.Token)
		}
		resp, err := g.client().Do(req)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", a.URL, resp.Status)
		}
		return ParseChecksums(data)
	}
	return nil, fmt.Errorf("the release of %s has no %s", tag, ChecksumsName)
}

// PutFile writes content to path on repo's default branch, with message as
// the commit's, unless the file already holds it. It reports whether it
// committed.
func (g *GitHub) PutFile(ctx context.Context, repo, path, message string, content []byte) (bool, error) {
	if err := CheckRepo(repo); err != nil {
		return false, err
	}
	var current struct {
		SHA     string `json:"sha"`
		Content string `json:"content"`
	}
	err := g.doJSON(ctx, http.MethodGet, "/repos/"+repo+"/contents/"+path, nil, &current)
	switch {
	case errors.Is(err, errNotFound):
	case err != nil:
		return false, err
	default:
		// GitHub wraps the base64 at 60 columns.
		old, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(current.Content, "\n", ""))
		if err == nil && bytes.Equal(old, content) {
			g.logf("%s in %s is current", path, repo)
			return false, nil
		}
	}
	put := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
	}
	if current.SHA != "" {
		put["sha"] = current.SHA
	}
	if err := g.doJSON(ctx, http.MethodPut, "/repos/"+repo+"/contents/"+path, put, nil); err != nil {
		return false, err
	}
	g.logf("committed %s to %s", path, repo)
	return true, nil
}
