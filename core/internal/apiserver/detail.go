package apiserver

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/zlogic-labs/fleet/core/internal/detail/clickhouse"
)

// detailConn is the control plane's connection to the reporting replica.
//
// Lazily opened and retried, unlike the gateway's, and the difference is not an
// oversight. The gateway mirrors into the replica, so a replica it cannot reach
// at startup is a gateway whose reports quietly rot — refusing to start is the
// honest answer there. The control plane only reads the replica, and it is also
// the process that manages models, tenants and keys; making the whole management
// API refuse to start because a diagnostic's database is down would trade a
// working product for a broken one.
//
// Retrying on each request rather than once at startup matters on the deployment
// this was written against: both Fleet processes are systemd units and the
// replica is a pod, so the control plane is regularly up before the database it
// wants to read.
type detailConn struct {
	cfg clickhouse.Config
	log *slog.Logger

	mu      sync.Mutex
	store   *clickhouse.Store
	reason  string
	nextTry time.Time
	logged  bool
}

// detailRetryEvery bounds how often an unreachable replica is re-dialled.
//
// Without it the console's thirty-second poll would each spend the dial timeout
// waiting for a server that is not there, and the page would be slower than the
// outage. With it, one attempt a minute and the other fifty-nine seconds answer
// immediately from the last failure.
const detailRetryEvery = time.Minute

// detailOpenTimeout bounds one attempt to reach the replica.
const detailOpenTimeout = 5 * time.Second

// get returns the replica, or the reason there is none.
//
// The reason is a sentence for the console rather than an error: no replica is a
// supported deployment, and "nothing configured" and "configured and not
// answering" are different problems with the same shape in a boolean.
func (d *detailConn) get() (*clickhouse.Store, string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.store != nil {
		return d.store, ""
	}
	if d.cfg.URL == "" {
		return nil, "no detail store is configured; set FLEET_CLICKHOUSE_URL on the control plane " +
			"to compare the reports with the books"
	}
	if time.Now().Before(d.nextTry) {
		return nil, d.reason
	}

	ctx, cancel := context.WithTimeout(context.Background(), detailOpenTimeout)
	defer cancel()
	store, err := clickhouse.Open(ctx, d.cfg)
	if err != nil {
		d.reason = "the detail store could not be reached: " + err.Error()
		d.nextTry = time.Now().Add(detailRetryEvery)
		if !d.logged {
			// Once, not once per attempt: a replica that is down for a week
			// would otherwise fill the log with the same line and bury whatever
			// put it down.
			d.log.Warn("the detail store is unreachable; the reconciliation will say so until it answers",
				"url", d.cfg.URL, "err", err)
			d.logged = true
		}
		return nil, d.reason
	}

	d.store = store
	d.reason = ""
	d.log.Info("comparing reports against the detail store",
		"database", d.cfg.Database, "url", d.cfg.URL)
	return store, ""
}

func (d *detailConn) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.store != nil {
		_ = d.store.Close()
		d.store = nil
	}
}
