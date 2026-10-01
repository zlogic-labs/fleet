package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/inventory"
)

// The one thing this package exists to get right: a deployment that is not
// serving must never become a route. Everything here is about that boundary,
// because the failure it prevents is silent — traffic aimed at a Service with
// no Pods behind it returns 503, which looks like a busy engine.
func TestOnlyAvailableDeploymentsWithAnAddressBecomeEndpoints(t *testing.T) {
	deps := []inventory.Deployment{
		{
			Name: "qwen-7b", Namespace: "fleet", Model: "Qwen/Qwen2.5-7B",
			State: inventory.DeployAvailable, Ready: 2,
			Selector: "Qwen/Qwen2.5-7B", Address: "qwen-7b.fleet.svc.cluster.local",
		},
		{
			Name: "qwen-7b-pending", Namespace: "fleet", Model: "Qwen/Qwen2.5-7B",
			State: inventory.DeployScheduling, Ready: 0,
			Address: "qwen-7b-pending.fleet.svc.cluster.local",
		},
		{
			Name: "llama-70b", Namespace: "fleet", Model: "meta/llama-70b",
			State: inventory.DeployInsufficientCapacity, Ready: 0,
			Address: "llama-70b.fleet.svc.cluster.local",
		},
		{
			// Available but with no address: the operator has not finished
			// rendering the Service. Routing to "http://" would fail every
			// request in a way that looks like an engine outage.
			Name: "half-done", Namespace: "fleet", Model: "meta/llama-8b",
			State: inventory.DeployAvailable, Ready: 1,
		},
	}

	eps := Endpoints(deps)
	if len(eps) != 1 {
		t.Fatalf("got %d endpoints, want only the available one with an address: %+v", len(eps), eps)
	}
	got := eps[0]
	if got.Model != "Qwen/Qwen2.5-7B" {
		t.Errorf("model = %q, want the selector rather than the model ref", got.Model)
	}
	if got.Replicas != 2 {
		t.Errorf("replicas = %d, want the ready count", got.Replicas)
	}
	// The id is the namespaced name, so a scale keeps the same endpoint and a
	// client's prefix affinity survives the rollout.
	if got.ID != "fleet/qwen-7b" {
		t.Errorf("id = %q, want the namespaced name", got.ID)
	}
	if got.BaseURL != "http://qwen-7b.fleet.svc.cluster.local" {
		t.Errorf("baseURL = %q", got.BaseURL)
	}
}

// A static upstream must not be displaced by discovery, and must not collide
// with it either: two endpoints sharing an id make routing non-deterministic
// for every key that hashes to them.
func TestMergedKeepsStaticEndpointsAndLetsDiscoveryWin(t *testing.T) {
	r := &Refresher{Merge: []engine.Endpoint{
		{ID: "fleet/qwen-7b", Model: "Qwen/Qwen2.5-7B", BaseURL: "http://stale", Replicas: 1},
		{ID: "laptop", Model: "local/llama", BaseURL: "http://127.0.0.1:8081", Replicas: 1},
	}}

	discovered := []engine.Endpoint{
		{ID: "fleet/qwen-7b", Model: "Qwen/Qwen2.5-7B", BaseURL: "http://current", Replicas: 3},
	}
	out := r.merged(discovered)

	if len(out) != 2 {
		t.Fatalf("got %d endpoints, want the discovered one plus the laptop", len(out))
	}
	if out[0].Replicas != 3 {
		t.Errorf("replicas = %d, want the discovered replica count to win", out[0].Replicas)
	}
	if out[1].ID != "laptop" {
		t.Errorf("second endpoint = %q, want the static laptop upstream kept", out[1].ID)
	}
}

func TestDeploymentsDecodeFromTheControlPlane(t *testing.T) {
	body, _ := json.Marshal([]inventory.Deployment{{
		Name: "qwen-0.5b", Namespace: "fleet", Model: "Qwen/Qwen2.5-0.5B",
		State: inventory.DeployAvailable, Ready: 1, Engine: "llama-cpp",
		Selector: "Qwen/Qwen2.5-0.5B", Address: "qwen-0-5b.fleet.svc.cluster.local",
	}})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/deployments" {
			t.Errorf("path = %q, want the deployments endpoint", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	deps, err := NewClient(srv.URL, "").Deployments(context.Background())
	if err != nil {
		t.Fatalf("deployments: %v", err)
	}
	if len(deps) != 1 || deps[0].Engine != "llama-cpp" {
		t.Fatalf("deps = %+v", deps)
	}
	eps := Endpoints(deps)
	if len(eps) != 1 || eps[0].Labels["engine"] != "llama-cpp" {
		t.Errorf("the engine name must survive so the right profile is used: %+v", eps)
	}
}

// A control plane that is down must produce an error, not an empty list: the
// caller keeps its endpoints on error and would happily wipe them on nil.
func TestDeploymentsReportsAFailureRatherThanAnEmptyFleet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := NewClient(srv.URL, "").Deployments(context.Background()); err == nil {
		t.Fatal("a 500 from the control plane must not look like zero deployments")
	}
}

func TestNoControlPlaneConfiguredIsNotAnError(t *testing.T) {
	deps, err := NewClient("", "").Deployments(context.Background())
	if err != nil {
		t.Fatalf("an unconfigured control plane must be silent: %v", err)
	}
	if deps != nil {
		t.Errorf("deps = %v, want nil", deps)
	}
}
