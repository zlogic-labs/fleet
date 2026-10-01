package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

func endpoint(t *testing.T, h http.Handler) engine.Endpoint {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return engine.Endpoint{ID: "e1", Model: "demo/model", BaseURL: srv.URL}
}

func TestProbeFindsTheSecondHealthCandidate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// /health deliberately absent, which is the case the candidate list exists
	// for: a fork that renamed its readiness endpoint.
	ad := NewAdapter(NewProbeClient(0, 0))

	cap, err := ad.Probe(context.Background(), endpoint(t, mux), engine.LlamaCPPProfile())
	if err != nil {
		t.Fatalf("probe should fall through to the second candidate: %v", err)
	}
	if cap.Profile != "llama-cpp" {
		t.Errorf("profile = %q, want llama-cpp", cap.Profile)
	}
}

func TestProbeFailsWhenNoHealthCandidateAnswers(t *testing.T) {
	ad := NewAdapter(NewProbeClient(0, 0))

	_, err := ad.Probe(context.Background(),
		endpoint(t, http.NewServeMux()), engine.LlamaCPPProfile())
	if err == nil {
		t.Fatal("an engine serving no health endpoint must not report ready")
	}
	if errs.KindOf(err) != errs.KindUpstream {
		t.Errorf("kind = %v, want upstream: %v", errs.KindOf(err), err)
	}
}

// A 503 means the model is still loading. The next candidate must not be tried,
// because a 404 on a wrong path would otherwise mask a not-ready engine and
// the scheduler would send it traffic before the weights are in memory.
func TestProbeDoesNotMaskNotReadyWithTheNextCandidate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	// /api/health would answer 200 if it were reached. It must not be.
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ad := NewAdapter(NewProbeClient(0, 0))

	_, err := ad.Probe(context.Background(), endpoint(t, mux), engine.LlamaCPPProfile())
	if err == nil {
		t.Fatal("a 503 on the real health path must not be reported as ready")
	}
	if !errs.KindOf(err).Retryable() {
		t.Errorf("a not-ready engine should be retryable, got %v", err)
	}
}

// Absence of /tokenize and /version is the normal case, not a fault. An engine
// that serves chat but no extensions is fully usable, and failing the whole
// probe would take it out of rotation.
func TestProbeRecordsMissingExtensionsWithoutFailing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ad := NewAdapter(NewProbeClient(0, 0))

	cap, err := ad.Probe(context.Background(), endpoint(t, mux), engine.VLLMProfile())
	if err != nil {
		t.Fatalf("missing extensions must not fail the probe: %v", err)
	}
	if cap.Tokenize {
		t.Error("Tokenize = true, but this engine serves no /tokenize")
	}
	if cap.Version != "" {
		t.Errorf("Version = %q, want empty", cap.Version)
	}
	if cap.MetricsAvailable != true {
		t.Error("vLLM declares metrics, so MetricsAvailable should be true")
	}
}

// llama-cpp declares no tokenize candidates, so the probe must not spend a
// round trip looking for one, and must not claim exact prompt counting.
func TestProbeSkipsUndeclaredExtensions(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/tokenize", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("/tokenize must not be probed for an engine that declares none")
		w.WriteHeader(http.StatusOK)
	})
	ad := NewAdapter(NewProbeClient(0, 0))

	cap, err := ad.Probe(context.Background(), endpoint(t, mux), engine.LlamaCPPProfile())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if cap.Tokenize {
		t.Error("Tokenize = true without a probe")
	}
	// llama-server does publish Prometheus metrics, under its own prefix and
	// only when started with --metrics, so autoscaling signals are declared.
	// What it must not claim is a cache-occupancy signal: llamacpp:n_tokens_max
	// is an observed high-water mark of context size, and reading it as usage
	// would make a nearly empty server look full.
	if !cap.MetricsAvailable {
		t.Error("llama-cpp declares queue and running signals, so autoscaling is available")
	}
	if _, ok := engine.LlamaCPPProfile().Metrics.Series[engine.SignalKVCacheUsed]; ok {
		t.Error("llama-cpp must not claim a KV cache occupancy signal")
	}
}

func TestProbeReadsContextWindowFromTokenize(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/tokenize", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"count":1,"max_model_len":32768,"tokens":[1]}`))
	})
	ad := NewAdapter(NewProbeClient(0, 0))

	cap, err := ad.Probe(context.Background(), endpoint(t, mux), engine.VLLMProfile())
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !cap.Tokenize {
		t.Fatal("Tokenize = false, but the engine served it")
	}
	if cap.MaxModelLen != 32768 {
		t.Errorf("MaxModelLen = %d, want 32768", cap.MaxModelLen)
	}
}
