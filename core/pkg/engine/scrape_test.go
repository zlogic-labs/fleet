package engine

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// vllmExposition is the shape verified against vllm/v1/metrics/loggers.py on
// main at 2eaa3bc: capacity arrives as labels on an info gauge whose sample
// value is always 1, and every unset CacheConfig field is the literal string
// "None" because CacheConfig.metrics_info() stringifies a dict of values.
const vllmExposition = `# HELP vllm:num_requests_waiting Number of requests waiting to be processed.
# TYPE vllm:num_requests_waiting gauge
vllm:num_requests_waiting{model_name="qwen-0.5b"} 3.0
# TYPE vllm:num_requests_running gauge
vllm:num_requests_running{model_name="qwen-0.5b"} 7.0
# TYPE vllm:kv_cache_usage_perc gauge
vllm:kv_cache_usage_perc{model_name="qwen-0.5b"} 0.42
# TYPE vllm:cache_config_info gauge
vllm:cache_config_info{block_size="16",cache_dtype="auto",cpu_offload_gb="None",enable_prefix_caching="False",kv_cache_max_concurrency="37.67",kv_cache_size_tokens="616000",num_gpu_blocks="38500",num_gpu_blocks_override="None"} 1.0
`

func serveMetrics(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func scrapeAgainst(t *testing.T, body string, p Profile) (Load, Capacity) {
	t.Helper()
	srv := serveMetrics(t, body)
	ep := Endpoint{ID: "t", Model: "qwen-0.5b", BaseURL: srv.URL}
	load, cap, err := NewScrapeClient(0).Scrape(context.Background(), ep, p)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	return load, cap
}

func TestScrapeReadsVLLMCapacityAndLoad(t *testing.T) {
	load, cap := scrapeAgainst(t, vllmExposition, VLLMProfile())

	if load.QueueDepth != 3 || load.RunningReqs != 7 {
		t.Errorf("load = %+v, want queue 3 running 7", load)
	}
	if load.KVCacheUsed != 0.42 {
		t.Errorf("KVCacheUsed = %v, want 0.42", load.KVCacheUsed)
	}
	if cap.KVTokens != 616000 {
		t.Errorf("KVTokens = %d, want 616000", cap.KVTokens)
	}
	if cap.MaxConcurrency != 37.67 {
		t.Errorf("MaxConcurrency = %v, want 37.67", cap.MaxConcurrency)
	}
	if cap.At.IsZero() {
		t.Error("capacity must be timestamped, or it cannot be aged out")
	}
	if load.UpdatedAt.IsZero() {
		t.Error("load must be timestamped, or Stale cannot work")
	}
}

// The whole reason the field is named kv_cache_usage_perc and not
// gpu_cache_usage_perc: an absent series reads as zero occupancy, which looks
// like a healthy idle engine rather than a broken signal.
func TestScrapeDoesNotInventZeroForAnAbsentSeries(t *testing.T) {
	load, _ := scrapeAgainst(t,
		`vllm:num_requests_waiting 1.0`+"\n", VLLMProfile())

	if load.KVCacheUsed != 0 {
		t.Errorf("KVCacheUsed = %v, want the zero value with no series present", load.KVCacheUsed)
	}
	// The absence has to remain detectable, or a caller cannot tell an idle
	// engine from an engine that never published the signal.
	if _, ok := VLLMProfile().Metrics.Series[SignalKVCacheUsed]; !ok {
		t.Fatal("the vLLM profile must declare a cache-usage series at all")
	}
}

func TestScrapeLeavesCapacityEmptyWhenTheGaugeIsAbsent(t *testing.T) {
	_, cap := scrapeAgainst(t, `vllm:num_requests_waiting 1.0`+"\n", VLLMProfile())
	if cap.KVTokens != 0 || cap.MaxConcurrency != 0 {
		t.Errorf("capacity = %+v, want zero rather than a fabricated number", cap)
	}
}

// A label that says "None" is how vLLM renders an unset field. Reading it as
// 0 would report a 0-token KV cache, and every capacity calculation downstream
// would then divide by it.
func TestScrapeTreatsTheStringNoneAsAbsent(t *testing.T) {
	_, cap := scrapeAgainst(t,
		`vllm:cache_config_info{kv_cache_max_concurrency="None",kv_cache_size_tokens="None"} 1.0`+"\n",
		VLLMProfile())
	if cap.KVTokens != 0 || cap.MaxConcurrency != 0 {
		t.Errorf("capacity = %+v, want zero when the engine reports None", cap)
	}
}

func TestScrapeReportsNoMetricsRatherThanZeroes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
	}))
	defer srv.Close()

	_, _, err := NewScrapeClient(0).Scrape(context.Background(),
		Endpoint{BaseURL: srv.URL}, LlamaCPPProfile())
	if err == nil {
		t.Fatal("a server started without --metrics must be reported, not treated as an idle engine")
	}
	if !isNoMetrics(err) {
		t.Errorf("err = %v, want a no-metrics verdict the console can explain", err)
	}
}

func isNoMetrics(err error) bool {
	return errors.Is(err, errNoMetrics)
}

// ConcurrencyAt is the number that makes a deployment priceable, so it is worth
// pinning: it is a ratio, and a replica that cannot fit one request at the
// configured context length must report less than one rather than clamping.
func TestConcurrencyAtIsARatioNotAClamp(t *testing.T) {
	c := Capacity{KVTokens: 400_000, MaxConcurrency: 1}

	if got := c.ConcurrencyAt(4096); got != 97.65625 {
		t.Errorf("ConcurrencyAt(4096) = %v, want 97.65625", got)
	}
	if got := c.ConcurrencyAt(1_000_000); got != 0.4 {
		t.Errorf("ConcurrencyAt(1M) = %v, want 0.4, which is a deployment that queues", got)
	}
	if got := (Capacity{}).ConcurrencyAt(4096); got != 0 {
		t.Errorf("ConcurrencyAt on an unreported capacity = %v, want 0", got)
	}
}

func TestLoadStaleRejectsAFrozenReading(t *testing.T) {
	now := time.Now()
	if !(Load{UpdatedAt: now.Add(-3 * time.Minute)}).Stale(now, 2*time.Minute) {
		t.Error("a three-minute-old sample is stale against a two-minute limit")
	}
	if (Load{UpdatedAt: now}).Stale(now, 2*time.Minute) {
		t.Error("a fresh sample is not stale")
	}
	if !(Load{}).Stale(now, 2*time.Minute) {
		t.Error("a zero timestamp is stale, not fresh")
	}
}
