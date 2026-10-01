package engine

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/errs"
)

// maxMetricsBody caps how much of an exposition is read. A vLLM instance under
// load emits tens of thousands of series, and the ones Fleet needs are in the
// first few hundred lines of the cache_config_info gauge; reading the whole
// body on a polling loop is bandwidth spent to discard it.
//
// The cap is not a correctness concern: if the gauge is past this many bytes
// the scrape finds no capacity and reports none, which is the honest outcome.
const maxMetricsBody = 4 << 20

// metricsClient fetches an engine's metrics body.
type metricsClient struct {
	hc *http.Client
}

func newMetricsClient(timeout time.Duration) *metricsClient {
	return &metricsClient{
		hc: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// get fetches path on base and returns the body.
//
// 404 and 501 are both mapped to errNoMetrics and treated as a fact rather than
// a failure: llama-server answers 501 unless it was started with --metrics,
// and an engine that will never serve metrics is a configuration fact the
// console should show, not an error to retry.
func (c *metricsClient) get(ctx context.Context, base, path string) ([]byte, error) {
	if path == "" {
		return nil, errNoMetrics
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, join(base, path), nil)
	if err != nil {
		return nil, errs.Internal(err)
	}
	req.Header.Set("Accept", "text/plain")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, errs.Unavailable("scraping %s: %s", path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusNotImplemented:
		return nil, errNoMetrics
	case resp.StatusCode >= 400:
		return nil, errs.Upstream(nil, "metrics endpoint reported %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxMetricsBody))
}

// join concatenates a base URL and a path without doubling the slash. Endpoints
// come from Kubernetes Service DNS names, so the shape is not ours to control.
func join(base, path string) string {
	return strings.TrimRight(base, "/") + path
}
