package render

import (
	"path"
	"sort"
	"strings"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
	api "github.com/zlogic-labs/fleet/operator/api/v1alpha1"
)

// flags renders opaque engine options as command-line arguments.
//
// Sorted, for the same reason spec.env is: a map iterated in Go's random order
// produces a different arg list on every reconcile, and a Deployment whose pod
// template churns is restarted by nothing but a diff.
func flags(options map[string]string) []string {
	if len(options) == 0 {
		return nil
	}
	keys := make([]string, 0, len(options))
	for k := range options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		name := strings.TrimLeft(k, "-")
		if name == "" {
			continue
		}
		out = append(out, "--"+name, options[k])
	}
	return out
}

// MainGGUF picks the weights file from a GGUF repository.
//
// The largest .gguf wins, which is the main model in every repository that has
// one. The alternative — the first match — is wrong wherever a repository also
// ships a multimodal projector, because the projector is also a .gguf and is
// the smaller file. A repository with no .gguf at all is not a GGUF
// repository, and returning empty makes the caller say so rather than pointing
// the engine at a directory.
func MainGGUF(files []api.ModelFile) string {
	best, bestSize := "", int64(-1)
	for _, f := range files {
		if !strings.EqualFold(path.Ext(f.Name), ".gguf") {
			continue
		}
		if f.Bytes > bestSize {
			best, bestSize = f.Name, f.Bytes
		}
	}
	return best
}

// firstCandidate is the first health path to try, or "/health".
//
// The fallback rather than an empty string is deliberate: a probe with no path
// targets the root, which most servers answer with 200 for a landing page even
// while the model is still loading. An engine that serves nothing on its root
// has to be declared with a profile, and a profile with no health candidate is
// declaring exactly that.
func firstCandidate(c engine.Candidates) string {
	for _, p := range c {
		if p != "" {
			return p
		}
	}
	return "/health"
}

// weightsMount is where a rendered pod expects the weights.
//
// The reconciler calls Weights first and reports its error, so a renderer
// running here already knows a mount exists; the fallback keeps a renderer
// from inventing a second, different path.
func weightsMount(in Input) string {
	if m := in.Deployment.Spec.WeightsMount; m != "" {
		return m
	}
	return defaultWeightsMount
}

// weightsPath is the absolute path of one weight file inside the pod.
//
// The store prefix is part of it, and omitting it produces a path that looks
// right and is not: the mount is the whole store, so a repository at
// "models/Qwen/Qwen2.5-0.5B-Instruct-GGUF" lands at
// /models/models/Qwen/Qwen2.5-0.5B-Instruct-GGUF, and an engine pointed at
// just the file name reports a missing model rather than a wrong path.
func weightsPath(in Input, file string) string {
	if file == "" {
		return weightsMount(in)
	}
	dir := in.Model.Status.Prefix
	if dir == "" {
		return path.Join(weightsMount(in), file)
	}
	return path.Join(weightsMount(in), dir, file)
}

// weightsDir is the absolute path of a model's directory inside the pod, which
// is what an engine that loads a whole repository rather than one file wants.
func weightsDir(in Input) string {
	return path.Join(weightsMount(in), in.Model.Status.Prefix)
}

func maxInt32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}
