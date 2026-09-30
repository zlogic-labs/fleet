package apiserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/zlogic-labs/fleet/core/internal/engine"
	"github.com/zlogic-labs/fleet/core/internal/hub"
	"github.com/zlogic-labs/fleet/core/internal/registry"
	"github.com/zlogic-labs/fleet/core/pkg/weights"
)

// Verify checks that the stored objects satisfy the engine that will load
// them.
//
// A registry entry in Ready state is a claim, and the claim is only worth
// something if a deployment can rely on it. This is what an operator runs when
// a pod fails to load weights and the question is whether the pull lied or the
// mount is wrong.
//
// The engine argument is what makes this check meaningful. Verifying against a
// hardcoded safetensors file list marks every GGUF model as incomplete, and a
// false "incomplete" on a model that is fine sends an operator to re-download
// several gigabytes that were already correct.
func (p *Puller) Verify(ctx context.Context, name, engineName string) (found int, missing []string, err error) {
	mdl, err := p.Store.GetModel(ctx, name)
	if err != nil {
		return 0, nil, err
	}
	if mdl.State != registry.StateReady {
		return 0, nil, fmt.Errorf("model %s is %s, not ready", name, mdl.State)
	}
	if err := engine.Compatible(mdl.Format, engineName); err != nil {
		return 0, nil, fmt.Errorf("%s: %w", name, err)
	}

	objs, err := p.Blobs.List(ctx, mdl.Prefix, 0)
	if err != nil {
		return 0, nil, err
	}
	rel := make([]string, 0, len(objs))
	for _, o := range objs {
		rel = append(rel, strings.TrimPrefix(o.Key, mdl.Prefix+"/"))
	}
	return len(rel), p.Profiles.For(engineName).VerifyFiles(rel), nil
}

// tokenizerFor returns the tokenizer id to record for a model.
//
// A GGUF carries its tokenizer inside the weights, so naming an external one
// is a claim Fleet cannot check and the engine will ignore. Returning the model
// name is the same claim with an extra step. The honest answer is empty: the
// gateway's own resolver finds the closest matching encoding, and the console
// shows that as "embedded".
func tokenizerFor(f weights.Format, fallback string) string {
	if f == weights.GGUF {
		return ""
	}
	return fallback
}

func paths(repo hub.Repo) []string {
	out := make([]string, 0, len(repo.Files))
	for _, f := range repo.Files {
		out = append(out, f.Path)
	}
	return out
}
