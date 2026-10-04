package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zlogic-labs/fleet/core/internal/blobstore"
	"github.com/zlogic-labs/fleet/core/internal/hub"
)

// openStore picks the object backend.
//
// S3 wins when an endpoint is configured; otherwise a directory under --data.
// The fallback is deliberate: an operator evaluating Fleet should be able to
// pull a small model and see the whole path work before standing up MinIO, and
// a single-node install is a legitimate deployment, not a mistake.
func openStore() (blobstore.Store, error) {
	if os.Getenv("FLEET_S3_ENDPOINT") != "" {
		store, err := blobstore.S3FromEnv(context.Background())
		if err != nil {
			return nil, fmt.Errorf("configuring S3: %w", err)
		}
		return store, nil
	}
	dir := envOr("FLEET_DATA_DIR", "./fleet-data")
	abs, err := filepath.Abs(filepath.Join(dir, "weights"))
	if err != nil {
		return nil, err
	}
	return blobstore.NewFS(abs)
}

// pickHub returns the model source and how to describe it in the log.
//
// --dev swaps in the synthetic repository so the control plane can be started
// with no network and no credentials. Everything it returns is labelled as
// synthetic in the log line above, because a downloaded-looking weight that is
// not loadable is otherwise indistinguishable from a real one until someone
// tries to serve it.
func pickHub(f flags) (hub.Hub, string) {
	if f.dev {
		stub := hub.NewStub()
		if f.devDelay > 0 {
			stub.PerFileDelay = f.devDelay
		}
		return stub, "synthetic (--dev)"
	}
	if f.hubToken != "" {
		return hub.NewHTTP(f.hubToken, f.fileConc), f.hubURL
	}
	return hub.NewHTTP("", f.fileConc), f.hubURL
}
