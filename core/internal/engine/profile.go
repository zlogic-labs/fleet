package engine

import (
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/zlogic-labs/fleet/core/pkg/weights"
)

// Signal is a scheduling signal Fleet wants, named independently of whatever
// the engine calls it.
type Signal string

const (
	// SignalQueueDepth is requests accepted but not yet running. The signal
	// that actually means "add capacity" for an LLM engine.
	SignalQueueDepth Signal = "queue_depth"
	// SignalRunningReqs is requests currently decoding.
	SignalRunningReqs Signal = "running_requests"
	// SignalKVCacheUsed is the engine's prefix-cache occupancy, 0..1. This is
	// the one that predicts throughput loss, because a full KV cache throttles
	// prefill before it throttles anything visible.
	SignalKVCacheUsed Signal = "kv_cache_used"
)

// MetricsSpec maps Fleet's signals onto one engine's Prometheus series.
//
// An empty Path means the engine exposes nothing usable, and that is a
// legitimate state rather than a gap to be filled in later: llama.cpp's server
// does not publish vLLM's metric set, and pretending otherwise would have the
// autoscaler acting on zeros. A deployment whose engine has no metrics scales
// on replica count alone, and the console says so.
type MetricsSpec struct {
	Path   string
	Series map[Signal]string
}

// Available reports whether this engine can drive autoscaling at all.
func (m MetricsSpec) Available() bool { return m.Path != "" && len(m.Series) > 0 }

// Candidates are the paths that might answer one question, best first.
//
// Listing candidates rather than a single path is the direct consequence of
// P4 applied to endpoint names instead of versions. vLLM serves /health;
// llama.cpp's server serves /health too but /props carries the build; neither
// is guaranteed to stay put across releases. Hard-coding one path per engine
// turns a rename into a deployment that never becomes ready, and the failure
// looks like a stuck image pull. Every candidate is optional: a wrong entry
// costs one 404, not a wrong Capability.
type Candidates []string

// Profiles is a set of engine families.
type Profiles struct {
	mu       sync.RWMutex
	byName   map[string]Profile
	fallback Profile
}

// NewProfiles returns a registry holding the given profiles, falling back to
// defaultProfile for any engine not named.
func NewProfiles(list ...Profile) *Profiles {
	p := &Profiles{byName: map[string]Profile{}, fallback: DefaultProfile()}
	for _, prof := range list {
		p.byName[strings.ToLower(prof.Name)] = prof
	}
	return p
}

// For returns the profile for an engine family, or the default.
//
// Never fails and never returns nil: a deployment naming an engine Fleet has
// never heard of still probes, because the OpenAI-compatible surface is the
// contract and the profile only sharpens it. An unknown engine is a console
// note, not a rejected deployment.
func (p *Profiles) For(engine string) Profile {
	if p == nil {
		return DefaultProfile()
	}
	key := strings.ToLower(strings.TrimSpace(engine))
	p.mu.RLock()
	defer p.mu.RUnlock()
	if prof, ok := p.byName[key]; ok {
		return prof
	}
	// A deployment that named "vllm-0.9" or "llama.cpp" should not silently
	// lose its weight-format check, so match a family prefix before giving up.
	for name, prof := range p.byName {
		if strings.HasPrefix(key, name) {
			return prof
		}
	}
	return p.fallback
}

// Register adds or replaces a profile. Used by the operator when it learns of a
// runtime Fleet has not shipped a profile for.
func (p *Profiles) Register(prof Profile) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byName[strings.ToLower(prof.Name)] = prof
}

// All returns every registered profile, sorted by name.
//
// Only the registered ones: the default is what you get when a lookup fails,
// not a member of the set, so including it would suggest an engine that does
// not exist.
func (p *Profiles) All() []Profile {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]Profile, 0, len(p.byName))
	for _, prof := range p.byName {
		out = append(out, prof)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Profile is everything Fleet knows about one engine family that is not
// implied by the OpenAI protocol.
//
// It is data, not code, and that is the whole point: adding an engine is adding
// a literal here, never an if-statement in a reconciler. Every field is
// either a fact to verify by probing or a declaration of absence, never an
// assumption about behaviour.
type Profile struct {
	// Name is the engine family a FleetDeployment names.
	Name string
	// Format is what this engine can load. weights.Unknown means undeclared,
	// and an undeclared engine is refused at admission: there is no such thing
	// as deploying a GGUF to vLLM, and there is no such thing as deploying
	// anything to an engine Fleet knows nothing about.
	Format weights.Format
	// RequiredFiles lists what a pull must contain before the model is
	// reported ready, as groups of alternatives: every group must be
	// satisfied by at least one of its patterns, and a file may satisfy more
	// than one group.
	//
	// The grouping is not decoration. A safetensors repository is a single
	// model.safetensors OR an index plus shards, never both, so a flat list
	// of "all required" patterns reports every correctly-sharded repository
	// as incomplete. A ready model that cannot be loaded is the worst
	// possible state, but so is a ready model reported broken.
	RequiredFiles [][]string
	// Health is the readiness gate. It is the only hard probe, and it accepts
	// the first candidate that answers.
	Health Candidates
	// Version and Tokenize are soft: absent is recorded, never fatal.
	Version  Candidates
	Tokenize Candidates
	// Metrics drives autoscaling. May be absent.
	Metrics MetricsSpec
	// MinCompute is the NVIDIA compute capability this engine needs, 0 when
	// the constraint does not apply. Checked at admission so an impossible
	// combination is refused in seconds rather than pending forever.
	//
	// These are the values the upstream projects document today, and they will
	// drift. They exist to fail fast on the common case, not to be the
	// authority — the authority is whether a model loads, which only a real
	// run can answer.
	MinCompute int
	// Notes is shown in the console. It is where a caveat that does not fit a
	// field goes, rather than into a field that then means two things.
	Notes string
}

// VerifyFiles reports which required groups are unsatisfied by a set of
// repository-relative paths.
//
// It returns the patterns of the unsatisfied groups, not the groups, because
// the message an operator reads should name the file they are missing rather
// than an index.
func (p Profile) VerifyFiles(files []string) (missing []string) {
	have := make(map[string]struct{}, len(files))
	for _, f := range files {
		have[f] = struct{}{}
	}
	for _, group := range p.RequiredFiles {
		if !matchesAny(have, group) {
			missing = append(missing, strings.Join(group, " or "))
		}
	}
	return missing
}

func matchesAny(have map[string]struct{}, patterns []string) bool {
	for _, pattern := range patterns {
		if _, ok := have[pattern]; ok {
			return true
		}
		for f := range have {
			if ok, _ := path.Match(pattern, f); ok {
				return true
			}
		}
	}
	return false
}
