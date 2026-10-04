package apiserver

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/zlogic-labs/fleet/core/internal/blobstore"
	"github.com/zlogic-labs/fleet/core/internal/registry"
	"github.com/zlogic-labs/fleet/core/pkg/weights"
)

// These are the three failures a repository check can report, and they used to
// be one. Every caller of this endpoint is an operator deciding whether to
// download something, retry something, or fix a request, and a single 400 for
// all three makes "you have not pulled this yet" read as a client bug.

// repoServer starts a control plane over a registry the test can seed.
func repoServer(t *testing.T, store *registry.Memory) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	blobs, err := blobstore.NewFS(dir)
	if err != nil {
		t.Fatalf("blobstore: %v", err)
	}
	cfg := Config{Listen: "127.0.0.1:0", Blobs: blobs}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := NewServer(cfg, store, log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() { srv.Close(); s.Close() })
	return srv
}

func seedModel(t *testing.T, store *registry.Memory, mdl registry.Model) {
	t.Helper()
	if _, err := store.UpsertModel(context.Background(), mdl); err != nil {
		t.Fatalf("seed %s: %v", mdl.Name, err)
	}
}

func repoStatus(t *testing.T, srv *httptest.Server, path string) int {
	t.Helper()
	code, body := call(t, srv, "GET", path, probeToken, "")
	if code != 200 {
		t.Logf("  %s -> %d %s", path, code, body)
	}
	return code
}

func TestAnUnregisteredRepositoryIsNotFound(t *testing.T) {
	srv := repoServer(t, registry.NewMemory())
	if got := repoStatus(t, srv, "/api/v1/repositories/acme/nothing?engine=llama-cpp"); got != 404 {
		t.Fatalf("got %d, want 404", got)
	}
}

func TestARepositoryWhoseWeightsAreMissingIsAConflict(t *testing.T) {
	store := registry.NewMemory()
	seedModel(t, store, registry.Model{
		Name:   "acme/pending",
		State:  registry.StatePending,
		Format: weights.GGUF,
		Prefix: "models/acme/pending",
	})
	srv := repoServer(t, store)

	// 409 and not 404: the model is registered and will be usable once its pull
	// finishes, so this is a state to wait out rather than a name that is wrong.
	if got := repoStatus(t, srv, "/api/v1/repositories/acme/pending?engine=llama-cpp"); got != 409 {
		t.Fatalf("got %d, want 409", got)
	}
}

func TestTheWrongEngineForAStoredFormatIsInvalid(t *testing.T) {
	store := registry.NewMemory()
	seedModel(t, store, registry.Model{
		Name:   "acme/tiny",
		State:  registry.StateReady,
		Format: weights.GGUF,
		Prefix: "models/acme/tiny",
	})
	srv := repoServer(t, store)

	// Ready, so this reaches the engine check that the pending case never gets
	// to: vLLM cannot read GGUF.
	if got := repoStatus(t, srv, "/api/v1/repositories/acme/tiny?engine=vllm"); got != 400 {
		t.Fatalf("got %d, want 400", got)
	}
}

func TestAReadyStoredRepositoryIsUsable(t *testing.T) {
	store := registry.NewMemory()
	seedModel(t, store, registry.Model{
		Name:   "acme/ok",
		State:  registry.StateReady,
		Format: weights.GGUF,
		Prefix: "models/acme/ok",
	})
	srv := repoServer(t, store)

	if got := repoStatus(t, srv, "/api/v1/repositories/acme/ok?engine=llama-cpp"); got != 200 {
		t.Fatalf("got %d, want 200", got)
	}
}
