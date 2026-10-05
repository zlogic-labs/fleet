package catalog

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
)

// Refresher keeps an endpoint set current, and keeps each endpoint's load
// sample fresh.
//
// The two jobs are in one loop because they read the same engine and a load
// sample attached to the wrong endpoint generation is worse than no sample at
// all: it would show a queue depth belonging to a deployment that no longer
// exists.
type Refresher struct {
	Client *Client
	// Discover polls the control plane for deployments. Set by Run when a
	// control plane address is configured.
	//
	// Off when none is, which is not an error: a gateway with three
	// hand-written upstreams has everything it needs to route and nothing to
	// ask. The scrape loop runs either way, so endpoint metrics are published
	// by a gateway that never discovers anything.
	Discover bool
	Profiles *engine.Profiles
	Log      *slog.Logger
	// Interval is how often to refresh. Short enough that a scale is visible
	// while someone is watching for it, long enough that a control plane
	// restart is not turned into a request storm.
	Interval time.Duration
	// ScrapeEvery skips most scrapes. Metrics are expensive relative to the
	// deployment list, and at the defaults -- a 15-second Interval with
	// ScrapeEvery of 4 -- a load sample is at most a minute old, which is what
	// engine.StaleAfter is sized against.
	ScrapeEvery int

	// Apply receives each new endpoint set. The gateway wires this to the
	// routing picker's Replace, so the set the request path reads and the set
	// this loop computes are the same object. It is a field rather than an
	// argument to Run because Build constructs both halves and Run only
	// starts them — two constructors both making a picker is how one of them
	// silently ends up unused.
	Apply func([]engine.Endpoint)
	// Report is told each published endpoint set, and is how /metrics sees the
	// same queue depths the router reads. Publishing from anywhere else would
	// be a second opinion about the same engines.
	Report func([]engine.Endpoint)

	mu        sync.RWMutex
	endpoints []engine.Endpoint
	// Merge is the config-declared fleet, kept alongside whatever discovery
	// finds. A deployment the operator reports and an upstream someone typed
	// into a config file are both real, and neither should displace the other.
	Merge []engine.Endpoint
}

// Set replaces the endpoint set, and is what the request path reads.
//
// Returning a copy matters: the routing package and the status handler both
// hold the slice across a loop, and handing out the live slice would let a
// refresh mutate a slice someone is ranging over.
func (r *Refresher) Set(eps []engine.Endpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endpoints = append([]engine.Endpoint(nil), eps...)
}

func (r *Refresher) Endpoints() []engine.Endpoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]engine.Endpoint(nil), r.endpoints...)
}

// Run refreshes until ctx is cancelled.
//
// A failed read logs and keeps the previous set. Clearing the endpoint set on a
// control-plane blip would turn a reporting outage into a total inference
// outage, and the failure mode is deeply asymmetric: stale endpoints route
// traffic to an engine that may have gone away, missing endpoints refuse
// traffic to an engine that is fine.
func (r *Refresher) Run(ctx context.Context) error {
	if r.Interval <= 0 {
		r.Interval = 15 * time.Second
	}
	if r.ScrapeEvery <= 0 {
		r.ScrapeEvery = 4
	}

	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		if !r.Discover {
			// No control plane to ask. The endpoint set stays as it was
			// declared, and the load samples below still get refreshed, which
			// is the half of this loop that matters for a hand-configured
			// fleet: the engines are real either way.
			r.scrapeIfDue(ctx, i)
			sleep(ctx, r.Interval)
			continue
		}

		deps, err := r.Client.Deployments(ctx)
		switch {
		case err != nil:
			r.Log.Error("could not read deployments; keeping the current endpoint set", "err", err)
		default:
			eps := r.merged(Endpoints(deps))
			r.Set(eps)
			if r.Apply != nil {
				r.Apply(eps)
			}
			r.Log.Info("endpoint set refreshed",
				"deployments", len(deps), "routable", len(eps))
		}

		// Reported on every pass, not only after a successful discovery. A
		// gateway with no control plane configured never discovers anything,
		// and a report inside the success branch meant it published no endpoint
		// metrics at all -- which reads to a dashboard as a fleet with no
		// endpoints rather than as a gateway that is not looking.
		if r.Report != nil {
			r.Report(r.Endpoints())
		}

		r.scrapeIfDue(ctx, i)
		sleep(ctx, r.Interval)
	}
}

// scrapeIfDue scrapes on every ScrapeEvery-th pass, so a metrics sweep does
// not run on the ticks that only refresh the deployment list.
func (r *Refresher) scrapeIfDue(ctx context.Context, i int) {
	if i%r.ScrapeEvery != 0 {
		return
	}
	r.scrapeAll(ctx)
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// merged combines discovered and static endpoints, letting discovery win an
// id collision.
//
// The collision is a real one: a deployment that has been adopted still exists
// in the config file for a while, and two endpoints with one id would make the
// picker non-deterministic for that key. Preferring the discovered entry keeps
// the replica count current, which is the number the config file got wrong.
func (r *Refresher) merged(discovered []engine.Endpoint) []engine.Endpoint {
	if len(r.Merge) == 0 {
		return discovered
	}
	seen := make(map[string]bool, len(discovered))
	out := append([]engine.Endpoint(nil), discovered...)
	for _, ep := range discovered {
		seen[ep.ID] = true
	}
	for _, ep := range r.Merge {
		if !seen[ep.ID] {
			out = append(out, ep)
		}
	}
	return out
}

// scrapeAll refreshes every endpoint's load sample.
//
// Endpoints are scraped concurrently because a fleet with a slow engine in it
// must not hold up the rest, and sequential scrapes across ten endpoints at
// five seconds each is a minute of stale routing data.
func (r *Refresher) scrapeAll(ctx context.Context) {
	eps := r.Endpoints()
	if len(eps) == 0 {
		return
	}
	scraper := engine.NewScrapeClient(0)

	var wg sync.WaitGroup
	updated := make([]engine.Endpoint, len(eps))
	for i, ep := range eps {
		wg.Add(1)
		go func(i int, ep engine.Endpoint) {
			defer wg.Done()
			profile := r.profileFor(ep)
			if !profile.Metrics.Available() {
				return
			}
			if load, _, err := scraper.Scrape(ctx, ep, profile); err == nil {
				ep.Load = load
			} else {
				// A stale sample is left stale rather than zeroed, so the
				// status handler's Stale check still fires and the console can
				// say the engine stopped reporting.
				r.Log.Debug("scrape failed", "endpoint", ep.ID, "err", err)
				return
			}
			updated[i] = ep
		}(i, ep)
	}
	wg.Wait()

	for i, ep := range updated {
		if ep.ID != "" {
			eps[i] = ep
		}
	}
	r.Set(eps)
	// The picker holds its own copy of the set, so a load sample that is not
	// pushed there is a sample no routing decision ever sees. /fleet/status
	// would show a queue depth and the router would behave as if it were idle.
	if r.Apply != nil {
		r.Apply(eps)
	}
	// No Report here: the loop in Run publishes on every tick, including this
	// one, and it publishes the set this call just installed. Reporting from
	// both places meant every scrape tick published twice in a row -- once with
	// the previous tick's load and once with this one's -- which is twice the
	// work for a gauge whose second write is the one that counts.
}

// profileFor picks the engine profile for an endpoint, from the label the
// operator reported.
//
// Falling back rather than refusing is deliberate: an endpoint whose engine
// name is unknown still probes against the default profile, which assumes less
// rather than more, and refusing to route to it would strand a deployment the
// operator can plainly see running.
func (r *Refresher) profileFor(ep engine.Endpoint) engine.Profile {
	if r.Profiles == nil {
		return engine.DefaultProfile()
	}
	name := ep.Labels["engine"]
	if name == "" {
		return engine.DefaultProfile()
	}
	return r.Profiles.For(name)
}
