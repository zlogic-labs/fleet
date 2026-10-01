package apiserver

import (
	"net/http"
	"strconv"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
)

// profile itself: the probes and required-file globs are internal detail, and
// what an operator needs is what this engine can run and what it will cost
// them to be wrong.
type engineView struct {
	Name       string `json:"name"`
	Format     string `json:"format"`
	MinCompute int    `json:"minCompute"`
	// Metrics says the engine publishes autoscaling signals.
	Metrics bool `json:"metrics"`
	// KVCacheUsage names the series Fleet reads cache occupancy from, or "".
	// It is exposed separately from Metrics because an engine can publish
	// queue depth without publishing occupancy — llama.cpp does exactly that —
	// and an autoscaler that reads occupancy from an engine that has none sees
	// zero and never scales back down.
	KVCacheUsage string   `json:"kvCacheUsage"`
	Capacity     bool     `json:"capacity"`
	Tokenize     bool     `json:"tokenize"`
	KnownModels  []string `json:"knownModels"`
	RequiresGPU  bool     `json:"requiresGpu"`
	// MinComputeLabel renders 75 as the vendor's name for it, so an operator
	// can match it against a card without doing the translation themselves.
	MinComputeLabel string `json:"minComputeLabel"`
	Notes           string `json:"notes"`
}

func (s *Server) listEngines(w http.ResponseWriter, _ *http.Request) {
	out := make([]engineView, 0, 4)
	for _, p := range s.profiles.All() {
		out = append(out, engineView{
			Name:            p.Name,
			Format:          p.Format.String(),
			MinCompute:      p.MinCompute,
			Metrics:         p.Metrics.Available(),
			KVCacheUsage:    p.Metrics.Series[engine.SignalKVCacheUsed],
			Capacity:        p.Metrics.CapacityAvailable(),
			Tokenize:        len(p.Tokenize) > 0,
			KnownModels:     engine.EnginesFor(p.Format),
			RequiresGPU:     p.MinCompute > 0,
			MinComputeLabel: computeLabel(p.MinCompute),
			Notes:           p.Notes,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// computeLabel renders a compute capability the way the vendor names it, so an
// operator can match it against a card without translating 75 to "Turing".
func computeLabel(cc int) string {
	if cc == 0 {
		return ""
	}
	name := map[int]string{
		50: "Maxwell", 61: "Pascal", 70: "Volta", 75: "Turing",
		80: "Ampere", 86: "Ampere", 89: "Ada", 90: "Hopper", 100: "Blackwell",
	}[cc]
	if name == "" {
		return "compute capability " + strconv.Itoa(cc) + "+"
	}
	return "compute capability " + strconv.Itoa(cc) + " (" + name + ")+"
}
