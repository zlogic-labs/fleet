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
	Name            string   `json:"name"`
	Format          string   `json:"format"`
	MinCompute      int      `json:"minCompute"`
	Metrics         bool     `json:"metrics"`
	Tokenize        bool     `json:"tokenize"`
	KnownModels     []string `json:"knownModels"`
	RequiresGPU     bool     `json:"requiresGpu"`
	MinComputeLabel string   `json:"minComputeLabel"`
	Notes           string   `json:"notes"`
}

func (s *Server) listEngines(w http.ResponseWriter, _ *http.Request) {
	out := make([]engineView, 0, 4)
	for _, p := range s.profiles.All() {
		out = append(out, engineView{
			Name:            p.Name,
			Format:          p.Format.String(),
			MinCompute:      p.MinCompute,
			Metrics:         p.Metrics.Available(),
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
