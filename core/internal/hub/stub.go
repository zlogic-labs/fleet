package hub

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Stub is an in-process Hub with synthetic repositories.
//
// It exists so that the pull path can be exercised on a laptop with no
// network and no disk budget: a real 7B repository is 15 GiB, and nobody
// should need that to check that progress reporting, cancellation and the
// registry transition all work. Its files are a few kilobytes of plausible
// names and shapes, and the engine will not load them — this is a plumbing
// test, not an inference test.
type Stub struct {
	// Repos is the synthetic catalogue, keyed by "owner/name".
	Repos map[string]StubRepo
	// PerFileDelay is the pause between files, so a pull is visibly in flight
	// rather than instantaneous.
	PerFileDelay time.Duration

	mu     sync.Mutex
	calls  int
	failOn string
}

type StubRepo struct {
	// Files maps a repository-relative path to its synthetic size. The content
	// is generated, not stored, so the catalogue costs nothing.
	Files map[string]int64
	// FailWith, if non-empty, makes Open return this error for the named file.
	FailWith string
	// Slow, if non-zero, overrides PerFileDelay for this repository, so a
	// pull can be made to last long enough to cancel.
	Slow time.Duration
}

var _ Hub = (*Stub)(nil)

// NewStub returns a catalogue with a couple of plausible repositories.
func NewStub() *Stub {
	return &Stub{
		PerFileDelay: 40 * time.Millisecond,
		Repos: map[string]StubRepo{
			"Qwen/Qwen2.5-1.5B-Instruct": {
				Files: map[string]int64{
					"config.json":            1161,
					"generation_config.json": 132,
					"model.safetensors":      512 * 1024,
					"tokenizer.json":         7101,
					"tokenizer_config.json":  350,
					"vocab.json":             1 << 20,
					"merges.txt":             16 * 1024,
				},
			},
			"Qwen/Qwen2.5-7B-Instruct": {
				Files: map[string]int64{
					"config.json":                      1161,
					"generation_config.json":           132,
					"model-00001-of-00004.safetensors": 2 << 20,
					"model-00002-of-00004.safetensors": 2 << 20,
					"model-00003-of-00004.safetensors": 2 << 20,
					"model-00004-of-00004.safetensors": 1 << 20,
					"model.safetensors.index.json":     2400,
					"tokenizer.json":                   7101,
					"tokenizer_config.json":            350,
					"vocab.json":                       1 << 20,
					"merges.txt":                       16 * 1024,
				},
			},
			// A repository that fails mid-pull, so the error path and the
			// registry's failed state are reachable without unplugging a cable.
			"fleet/broken-model": {
				Files:    map[string]int64{"config.json": 1161, "model.safetensors": 1 << 20},
				FailWith: "corrupt shard: checksum mismatch",
			},
			// A repository slow enough to cancel. Without it, a synthetic pull
			// finishes in under a second and the console's cancel button can
			// never be exercised.
			"fleet/slow-model": {
				Files: map[string]int64{
					"config.json":                      1161,
					"tokenizer.json":                   7101,
					"model-00001-of-00008.safetensors": 1 << 20,
					"model-00002-of-00008.safetensors": 1 << 20,
					"model-00003-of-00008.safetensors": 1 << 20,
					"model-00004-of-00008.safetensors": 1 << 20,
					"model-00005-of-00008.safetensors": 1 << 20,
					"model-00006-of-00008.safetensors": 1 << 20,
					"model-00007-of-00008.safetensors": 1 << 20,
					"model-00008-of-00008.safetensors": 1 << 20,
				},
				Slow: 3 * time.Second,
			},
		},
	}
}

func (s *Stub) Resolve(_ context.Context, owner, name, revision string) (Repo, error) {
	entry, ok := s.Repos[owner+"/"+name]
	if !ok {
		return Repo{}, fmt.Errorf("hub: repository %s/%s not found", owner, name)
	}
	if revision == "" {
		revision = "main"
	}
	// A stub commit hash, so that callers which pin to Resolved see a real
	// hash rather than the branch name they passed in.
	commit := strings.Repeat("0", 39) + "1"

	files := make([]File, 0, len(entry.Files))
	ref := url.QueryEscape(owner + "/" + name + "@" + revision)
	for p, size := range entry.Files {
		files = append(files, File{
			Path: p,
			Size: size,
			LFS:  strings.HasSuffix(p, ".safetensors"),
			URL:  fmt.Sprintf("stub://%s/%s", ref, p),
		})
	}
	sortFiles(files)
	return Repo{
		Owner: owner, Name: name, Revision: revision, Resolved: commit,
		Files: files, TotalBytes: totalOf(files),
	}, nil
}

func (s *Stub) Open(ctx context.Context, file File) (io.ReadCloser, int64, error) {
	// The ref is query-escaped in the URL because it contains slashes, and the
	// file path is not, because it may contain more of them. Splitting on the
	// last slash is therefore the only reading that is right for both.
	rest := strings.TrimPrefix(file.URL, "stub://")
	slash := strings.LastIndex(rest, "/")
	if slash < 0 {
		return nil, 0, fmt.Errorf("hub: malformed stub URL %q", file.URL)
	}
	ref, err := url.QueryUnescape(rest[:slash])
	if err != nil {
		return nil, 0, fmt.Errorf("hub: malformed stub URL %q", file.URL)
	}
	path := rest[slash+1:]

	// The URL carries owner/name@revision; the catalogue is keyed by
	// owner/name, because the stub serves the same synthetic files at every
	// revision.
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		ref = ref[:at]
	}

	s.mu.Lock()
	s.calls++
	entry, ok := s.Repos[ref]
	s.mu.Unlock()
	if !ok {
		return nil, 0, fmt.Errorf("hub: repository %s not found", ref)
	}
	if entry.FailWith != "" {
		// No path prefix here: the caller already wraps this with the file
		// name, and repeating it reads as a different error than it is.
		return nil, 0, fmt.Errorf("stub: %s", entry.FailWith)
	}

	delay := s.PerFileDelay
	if entry.Slow > 0 {
		delay = entry.Slow
	}
	if delay > 0 {
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(delay):
		}
	}

	// Content is generated so the catalogue costs nothing, but it is
	// deterministic: the same path always yields the same bytes, so a test can
	// assert on what was stored.
	body := synthetic(path, file.Size)
	return io.NopCloser(bytes.NewReader(body)), int64(len(body)), nil
}

// Calls reports how many Open calls have been made, which is how a test
// asserts that a retry did not re-download everything.
func (s *Stub) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func synthetic(path string, size int64) []byte {
	if size <= 0 || size > 4<<20 {
		size = 1024
	}
	out := make([]byte, size)
	seed := uint32(1469598103)
	for i := range path {
		seed = (seed ^ uint32(path[i])) * 16777619
	}
	for i := range out {
		seed = seed*1664525 + 1013904223
		out[i] = byte(seed >> 24)
	}
	return out
}
