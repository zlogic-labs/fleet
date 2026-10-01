// Package catalog keeps the gateway's view of which engines are serving what.
//
// It exists because the endpoint set is not static once an operator is running:
// a deployment scales, a rollout replaces a Service, a cluster moves. The
// gateway has to learn those changes without being restarted, and it has to
// learn them from somewhere that is not Kubernetes — P7 keeps client-go out of
// the gateway, so the source of truth is the control plane's inventory, and
// Kubernetes is one hop further upstream than that.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/inventory"
)

// Client reads deployment state from the control plane.
type Client struct {
	// Base is the control plane's address, e.g. http://127.0.0.1:8081.
	Base string
	// Token authenticates when the control plane requires it.
	Token string
	hc    *http.Client
}

// NewClient returns a client with a short timeout. The read is on a refresh
// loop, and a control plane that takes seconds to answer has already told us
// everything we need to know.
func NewClient(base, token string) *Client {
	return &Client{
		Base:  base,
		Token: token,
		hc:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Deployments lists what the operator has reported.
//
// A failure is returned, never swallowed: the caller has to decide between
// "no deployments" and "could not ask", and those lead to different behaviour
// — one clears the endpoint set, the other keeps serving what it has.
func (c *Client) Deployments(ctx context.Context) ([]inventory.Deployment, error) {
	if c.Base == "" {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.Base+"/api/v1/deployments", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read deployments: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("control plane returned %d for deployments", resp.StatusCode)
	}
	var out []inventory.Deployment
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode deployments: %w", err)
	}
	return out, nil
}

// Endpoints turns reported deployments into routing endpoints.
//
// Only Available deployments are routable, and the distinction is the whole
// point of the operator's phase vocabulary: a deployment that is Scheduling is
// a promise, and sending traffic to it produces errors rather than completions.
// An operator watching the console should see it listed there and absent from
// the gateway.
func Endpoints(deps []inventory.Deployment) []engine.Endpoint {
	out := make([]engine.Endpoint, 0, len(deps))
	for _, d := range deps {
		if d.State != inventory.DeployAvailable || d.Ready < 1 || d.Address == "" {
			continue
		}
		// The selector is the name clients send, and it is the FleetModel's
		// name rather than anything derived from the deployment's own. Falling
		// back to the model ref keeps a route alive for a report that came
		// from an older operator, and costs nothing when it is present.
		model := d.Selector
		if model == "" {
			model = d.Model
		}
		if model == "" {
			continue
		}
		out = append(out, engine.Endpoint{
			// The address is a Service DNS name, which survives every
			// rescheduling. Keying the endpoint on it means a rollout that
			// replaces the Pods behind it is not a different endpoint, so a
			// client's prefix affinity survives the replacement.
			ID:      d.Key(),
			Model:   model,
			BaseURL: "http://" + d.Address,
			// Ready rather than Desired: the endpoints must match what the
			// Service can actually route to, and a replica that has not passed
			// its readiness probe contributes nothing to the address the
			// gateway holds.
			Replicas: d.Ready,
			Labels: map[string]string{
				"engine":     d.Engine,
				"deployment": d.Name,
				"namespace":  d.Namespace,
				"gpu_per":    fmt.Sprintf("%d", d.GPUPer),
			},
		})
	}
	return out
}
