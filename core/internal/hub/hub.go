// Package hub downloads model repositories and uploads them to object storage.
//
// Fleet does not stream weights through a Kubernetes node. A 70B model is
// 140 GiB; pulling it per replica, per node, per restart is what turns a model
// deployment into a data-movement problem. Instead the control plane pulls once
// into object storage, and a deployment mounts that prefix. This package is
// that once.
package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// File is one file in a repository.
type File struct {
	// Path is relative to the repository root, e.g. model-00001-of-00004.safetensors.
	Path string
	Size int64
	// URL is the direct download link, already resolved for the revision.
	URL string
	// LFS is true for files stored in git-lfs, which is every weight file.
	LFS bool
}

// Repo is a repository at a specific revision.
type Repo struct {
	Owner string
	Name  string
	// Revision is a commit hash, a branch, or a tag. Callers that need
	// reproducibility resolve it first.
	Revision string
	Files    []File
	// TotalBytes is the sum of every file's size.
	TotalBytes int64
	// Resolved is the commit hash the revision pointed at.
	Resolved string
}

// Ref renders "owner/name@revision".
func (r Repo) Ref() string { return r.Owner + "/" + r.Name + "@" + r.Resolved }

// Hub is a model repository client.
type Hub interface {
	// Resolve fetches the file list for a repository at a revision. A
	// branch or tag is resolved to a commit hash so that everything
	// downstream can pin to it.
	Resolve(ctx context.Context, owner, name, revision string) (Repo, error)
	// Open streams a file's contents. The caller closes the reader.
	Open(ctx context.Context, file File) (io.ReadCloser, int64, error)
}

// HTTP talks to huggingface.co. It is also what talks to a private Hub, a
// mirror, or a corporate proxy: only BaseURL differs.
type HTTP struct {
	BaseURL string
	// Token authenticates to gated repositories. Empty means anonymous,
	// which works for public models and fails with a clear 401 otherwise.
	Token string
	// Transport is shared across requests so a pull of a large repository
	// reuses connections instead of handshaking per file.
	Transport http.RoundTripper
	// MaxConcurrent bounds simultaneous file downloads. The Hub rate-limits
	// anonymous traffic hard, and a 40-file repository fired in parallel is a
	// reliable way to get a 429 halfway through a 140 GiB pull.
	MaxConcurrent int
}

var _ Hub = (*HTTP)(nil)

const defaultBase = "https://huggingface.co"

func (h *HTTP) base() string {
	if h.BaseURL == "" {
		return defaultBase
	}
	return strings.TrimRight(h.BaseURL, "/")
}

func (h *HTTP) client() *http.Client {
	tr := h.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}
	return &http.Client{Transport: tr, Timeout: 0}
}

func (h *HTTP) transport() http.RoundTripper {
	if h.Transport != nil {
		return h.Transport
	}
	return http.DefaultTransport
}

// apiModel is the shape of the Hub's model-info endpoint.
type apiModel struct {
	Sha      string `json:"sha"`
	Siblings []struct {
		Rfilename string `json:"rfilename"`
		// Size is absent for git-lfs files, which is the common case.
		Size *int64 `json:"size,omitempty"`
	} `json:"siblings"`
}

func (h *HTTP) Resolve(ctx context.Context, owner, name, revision string) (Repo, error) {
	if owner == "" || name == "" {
		return Repo{}, fmt.Errorf("hub: owner and name are required")
	}
	if revision == "" {
		revision = "main"
	}
	// A full commit hash needs no resolution and is the cheapest path, so try
	// it before spending a request on a branch lookup.
	if isCommitHash(revision) {
		if files, err := h.listFiles(ctx, owner, name, revision); err == nil {
			return Repo{
				Owner: owner, Name: name, Revision: revision, Resolved: revision,
				Files: files, TotalBytes: totalOf(files),
			}, nil
		}
	}

	var model apiModel
	if err := h.getJSON(ctx, fmt.Sprintf("/api/models/%s/%s/revision/%s",
		url.PathEscape(owner), url.PathEscape(name), url.PathEscape(revision)), &model); err != nil {
		return Repo{}, err
	}
	if model.Sha == "" {
		return Repo{}, fmt.Errorf("hub: %s/%s has no commit at %q", owner, name, revision)
	}

	files, err := h.listFiles(ctx, owner, name, model.Sha)
	if err != nil {
		return Repo{}, err
	}
	return Repo{
		Owner: owner, Name: name, Revision: revision, Resolved: model.Sha,
		Files: files, TotalBytes: totalOf(files),
	}, nil
}

// listFiles expands the repository at a commit into downloadable files.
//
// Sizes are not in the listing, because the listing is served from a manifest
// that does not record them for git-lfs objects. The Hub's tree API does, so
// that is where the numbers come from: progress reporting without real sizes
// is progress reporting that lies.
func (h *HTTP) listFiles(ctx context.Context, owner, name, revision string) ([]File, error) {
	var tree struct {
		Siblings []struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
			LFS  struct {
				Size int64 `json:"size"`
			} `json:"lfs"`
		} `json:"siblings"`
	}
	endpoint := fmt.Sprintf("/api/models/%s/%s/tree/%s?recursive=true",
		url.PathEscape(owner), url.PathEscape(name), url.PathEscape(revision))
	if err := h.getJSON(ctx, endpoint, &tree); err != nil {
		return nil, err
	}

	base := h.base()
	files := make([]File, 0, len(tree.Siblings))
	for _, s := range tree.Siblings {
		size := s.Size
		lfs := false
		if s.LFS.Size > 0 {
			size = s.LFS.Size
			lfs = true
		}
		files = append(files, File{
			Path: s.Path,
			Size: size,
			LFS:  lfs,
			URL: fmt.Sprintf("%s/%s/%s/resolve/%s/%s",
				base, url.PathEscape(owner), url.PathEscape(name), url.PathEscape(revision), escapePath(s.Path)),
		})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("hub: %s/%s@%s has no files", owner, name, revision)
	}
	return files, nil
}

func (h *HTTP) Open(ctx context.Context, file File) (io.ReadCloser, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, file.URL, nil)
	if err != nil {
		return nil, 0, err
	}
	h.authorize(req)

	res, err := h.client().Do(req)
	if err != nil {
		return nil, 0, err
	}
	if res.StatusCode != http.StatusOK {
		// Draining lets the connection be reused for the next file, which
		// matters when the alternative is a fresh TLS handshake per file.
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		_ = res.Body.Close()
		return nil, 0, statusError(res.StatusCode, file.Path)
	}
	return res.Body, res.ContentLength, nil
}

func (h *HTTP) getJSON(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	h.authorize(req)

	res, err := h.client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("hub: %s %s: %w (%s)", req.Method, path, statusError(res.StatusCode, ""), strings.TrimSpace(string(body)))
	}
	dec := json.NewDecoder(res.Body)
	if err := dec.Decode(into); err != nil {
		return fmt.Errorf("hub: decoding %s: %w", path, err)
	}
	return nil
}

func (h *HTTP) authorize(req *http.Request) {
	if h.Token == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
}

// statusError distinguishes the failures that need different operator action.
// A 401 on a gated repo and a 429 from rate limiting look identical in a log
// and mean opposite things.
func statusError(code int, pathHint string) error {
	where := ""
	if pathHint != "" {
		where = " for " + pathHint
	}
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("hub: access denied%s; the repository may be gated, and a token is required", where)
	case http.StatusNotFound:
		return fmt.Errorf("hub: not found%s; check the repository and revision", where)
	case http.StatusTooManyRequests:
		return fmt.Errorf("hub: rate limited%s; retry later or set a token to raise the limit", where)
	case http.StatusRequestEntityTooLarge:
		return fmt.Errorf("hub: file too large%s", where)
	default:
		return fmt.Errorf("hub: unexpected status %d%s", code, where)
	}
}

// isCommitHash is a length and hex check, not a parse: the Hub is the only
// authority on whether a revision exists, and a wrong guess costs one failed
// request.
func isCommitHash(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// escapePath escapes each segment but keeps the separators, because a resolve
// URL addresses a file inside a repository, not a single flat name.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return strings.Join(parts, "/")
}

func totalOf(files []File) int64 {
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}

// RepoRef is a convenience for callers that only have "owner/name".
func RepoRef(ref, revision string) (owner, name, rev string, err error) {
	parts := strings.Split(strings.Trim(ref, "/"), "/")
	switch len(parts) {
	case 2:
		owner, name, rev = parts[0], parts[1], revision
	case 3:
		owner, name, rev = parts[0], parts[1], parts[2]
	default:
		return "", "", "", fmt.Errorf("hub: %q is not owner/name or owner/name@revision", ref)
	}
	if rev == "" {
		rev = "main"
	}
	if owner == "" || name == "" {
		return "", "", "", fmt.Errorf("hub: %q has an empty owner or name", ref)
	}
	if path.Base(owner) != owner || path.Base(name) != name {
		return "", "", "", fmt.Errorf("hub: %q must be two path segments", ref)
	}
	return owner, name, rev, nil
}

// NewHTTP builds a Hub with a connection pool sized for a pull.
//
// A pull holds a few connections open for minutes, and Go's default idle
// timeout is short relative to a 2 GiB weight file on a slow link.
func NewHTTP(token string, maxConcurrent int) *HTTP {
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConns = maxConcurrent * 2
	tr.MaxIdleConnsPerHost = maxConcurrent * 2
	tr.IdleConnTimeout = 90 * time.Second
	return &HTTP{Token: token, Transport: tr, MaxConcurrent: maxConcurrent}
}
