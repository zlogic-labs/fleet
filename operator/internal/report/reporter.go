// Package report pushes what the operator sees to the Fleet control plane.
//
// It exists because of P7. The control plane must not import client-go, or the
// gateway stops being usable outside a cluster; so the control plane cannot
// read Nodes and Deployments itself. The operator is the only component allowed
// to know what Kubernetes is, which makes it the only component that can
// produce an inventory report. The control plane receives the result over HTTP
// and never learns what produced it.
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/inventory"
)

// Reporter posts inventory to the control plane.
type Reporter struct {
	// URL is the control plane's base address, e.g. http://127.0.0.1:8081.
	URL string
	// Cluster names this operator's report, so two operators pointed at one
	// control plane do not overwrite each other.
	Cluster string
	// Timeout bounds one report. It is short because the next one is a fixed
	// interval away: a control plane that is down must not accumulate a
	// backlog of reports that all fail at once when it comes back.
	Timeout time.Duration
	// Token authenticates to the control plane when it requires it. Empty is
	// the single-installation case.
	Token string

	hc *http.Client
}

// New returns a reporter with sane bounds.
func New(controlPlane, cluster string) *Reporter {
	return &Reporter{
		URL:     controlPlane,
		Cluster: cluster,
		Timeout: 10 * time.Second,
		hc: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// Send posts one report.
//
// Failure is returned rather than logged and swallowed, because the caller is a
// loop: a report that cannot be delivered has to be visible, and the loop
// decides how loudly to complain.
func (r *Reporter) Send(ctx context.Context, rep inventory.Report) error {
	if r.URL == "" {
		return errNoControlPlane
	}
	rep.Cluster.Name = r.Cluster

	body, err := json.Marshal(rep)
	if err != nil {
		return errs.Internal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		r.URL+"/api/v1/inventory", bytes.NewReader(body))
	if err != nil {
		return errs.Internal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}

	resp, err := r.hc.Do(req)
	if err != nil {
		return errs.Unavailable("reporting inventory: %s", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNoContent, resp.StatusCode == http.StatusOK:
		return nil
	default:
		return errs.Upstream(nil, "control plane rejected the inventory report with %d", resp.StatusCode)
	}
}

var errNoControlPlane = fmt.Errorf(
	"no control plane configured; set --report-to or leave reporting off entirely")
