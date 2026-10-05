package apiserver

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/zlogic-labs/fleet/core/internal/blobstore"
	"github.com/zlogic-labs/fleet/core/internal/registry"
)

// The engine catalogue is what the console shows instead of letting an
// operator submit a combination that will be refused at admission, so the two
// engines that differ must actually differ here: vLLM reports queue time,
// llama-server cannot, and the console needs both facts plus the path it can
// check against the engine's own response.

func engineCatalogue(t *testing.T) map[string]engineView {
	t.Helper()
	blobs, err := blobstore.NewFS(t.TempDir())
	if err != nil {
		t.Fatalf("blobstore: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := NewServer(Config{Listen: "127.0.0.1:0", Blobs: blobs}, registry.NewMemory(), log)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(s.Close)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/engines", nil))

	var out []engineView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding the catalogue: %v", err)
	}
	byName := map[string]engineView{}
	for _, e := range out {
		byName[e.Name] = e
	}
	return byName
}

func TestTheCatalogueSaysWhichEnginesReportAQueueTime(t *testing.T) {
	byName := engineCatalogue(t)

	vllm, ok := byName["vllm"]
	if !ok {
		t.Fatal("vllm is missing from the catalogue")
	}
	if !vllm.QueueTime {
		t.Error("vllm does report queue time and the console says it does not")
	}
	// The container is the part an operator cannot guess, so the path has to
	// include it or it is not checkable against the engine's own response.
	if want := "metrics.queue_time_ms"; vllm.QueueTimeField != want {
		t.Errorf("vllm queue field = %q, want %q", vllm.QueueTimeField, want)
	}

	llama, ok := byName["llama-cpp"]
	if !ok {
		t.Fatal("llama-cpp is missing from the catalogue")
	}
	// prompt_ms is queue plus prompt evaluation. Reporting it as queue time
	// would manufacture a split that does not exist, and an operator acting on
	// it would scale for a queue that is really a long prompt.
	if llama.QueueTime {
		t.Error("llama-cpp reports prompt_ms as queue time, which it is not")
	}
	if llama.QueueTimeField != "" {
		t.Errorf("llama-cpp carries a queue field %q while saying it has none",
			llama.QueueTimeField)
	}
	if llama.RequestTimingsNote == "" {
		t.Error("an engine that cannot separate the two spans needs to say why")
	}
}
