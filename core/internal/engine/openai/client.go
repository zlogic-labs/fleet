// Package openai implements engine.Adapter against the OpenAI-compatible HTTP
// surface that vLLM, SGLang, llama.cpp, TGI and our own future runtimes all
// expose. It is the default adapter: a deployment with no engine-specific
// adapter registered still probes successfully (P1).
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zlogic/fleet/core/pkg/errs"
)

// errNotSupported means the endpoint is up but does not implement the probed
// capability. It is a fact to record in Capability, not a failure.
var errNotSupported = errors.New("capability not supported by this endpoint")

// DefaultProbeTimeout bounds a single capability check. Probes run on a
// control-plane loop, so a slow engine must not stall the loop.
const DefaultProbeTimeout = 5 * time.Second

// ProbeClient performs capability checks. It is deliberately separate from the
// proxying client: probing wants aggressive timeouts and a small pool, while
// proxying wants long-lived idle connections and full-duplex streaming.
// Sharing one http.Client between the two is how a gateway acquires a
// pathological tail latency.
type ProbeClient struct {
	hc *http.Client
}

func NewProbeClient(timeout time.Duration, maxIdleConns int) *ProbeClient {
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	if maxIdleConns <= 0 {
		maxIdleConns = 64
	}
	return &ProbeClient{
		hc: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        maxIdleConns,
				MaxIdleConnsPerHost: maxIdleConns / 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// Do issues a JSON request. A 404 or 405 becomes errNotSupported, because
// every capability in this package is optional by construction.
func (c *ProbeClient) Do(ctx context.Context, method, rawURL string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return errs.Internal(err)
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return errs.Internal(err)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return errs.Unavailable("probing %s: %s", rawURL, err)
	}
	defer func() {
		// Draining lets the connection return to the pool; a 5 KiB cap is
		// enough to reuse it and small enough not to buffer a large error.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 5<<10))
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusMethodNotAllowed:
		return errNotSupported
	case resp.StatusCode >= 400:
		return statusError(resp)
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return errs.Upstream(err, "decoding response from %s", rawURL)
	}
	return nil
}

// statusError preserves the upstream status because engines signal real
// conditions through it: 400 is a bad request, 503 is a model still loading.
func statusError(resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	detail := strings.TrimSpace(string(snippet))

	switch resp.StatusCode {
	case http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return errs.Upstream(nil, "endpoint reported %d: %s", resp.StatusCode, detail)
	case http.StatusBadRequest:
		return errs.InvalidArgument("endpoint rejected the probe: %s", detail)
	default:
		return errs.Upstream(nil, "endpoint reported %d", resp.StatusCode)
	}
}

// resolve joins a base URL with a path, tolerating a trailing slash on the
// base. Endpoints come from Kubernetes Services, so the shape is not ours to
// control.
func resolve(base, path string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", errs.Internal(err)
	}
	return strings.TrimRight(b.String(), "/") + path, nil
}
