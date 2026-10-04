package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

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
	// MaxConcurrent sizes the connection pool. It is not the download
	// concurrency bound, which lives in the puller and is what the operator
	// sets with -file-concurrency; this only decides how many idle connections
	// are kept warm for that many workers.
	//
	// The comment here used to claim to bound simultaneous downloads, and
	// nothing consulted it. The operator's flag help still says "file
	// downloads are bounded", which was true of the puller and misleading
	// next to this field. The bound is a puller concern because it is a
	// semaphore over in-flight work; this is a pool size.
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
// The Hub's tree API answers with a JSON array of entries, each carrying a type
// of "file" or "directory". This used to decode it as the {"siblings": [...]}
// object that the model-info endpoint returns, which fails against every real
// repository. Nothing caught it because the stub hub implements Hub directly and
// never speaks HTTP, so this decoding path had no test at all -- every local run
// bypassed the exact code that production depends on.
//
// Recursive listings include directory entries, and a directory has no size. Left
// in, they become zero-byte downloads that fail partway through a pull.
func (h *HTTP) listFiles(ctx context.Context, owner, name, revision string) ([]File, error) {
	var tree []struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
		LFS  struct {
			Size int64 `json:"size"`
		} `json:"lfs"`
	}
	endpoint := fmt.Sprintf("/api/models/%s/%s/tree/%s?recursive=true",
		url.PathEscape(owner), url.PathEscape(name), url.PathEscape(revision))
	if err := h.getJSON(ctx, endpoint, &tree); err != nil {
		return nil, err
	}

	base := h.base()
	files := make([]File, 0, len(tree))
	for _, s := range tree {
		if s.Type != "" && s.Type != "file" {
			continue
		}
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
