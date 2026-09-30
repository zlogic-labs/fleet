// Package render turns a FleetDeployment into the Kubernetes objects that
// would serve it.
//
// The reconciler never names an engine. It asks the registry for a renderer and
// hands over the object it found; which renderer that is comes from a
// registration, not a branch. An engine Fleet has never heard of fails as
// "no renderer", which is a true statement, rather than silently rendering a
// vLLM Pod for something that wanted llama.cpp.
package render

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/weights"
	api "github.com/zlogic-labs/fleet/operator/api/v1alpha1"
)

// Input is everything a renderer is allowed to look at.
//
// The model is passed in rather than read from the API because the reconciler
// already fetched it, and a second read would be a second chance to render
// against a model that has since been deleted.
type Input struct {
	Deployment api.FleetDeployment
	Model      api.FleetModel
	Profile    engine.Profile
}

// Renderer produces the objects for one engine family.
type Renderer interface {
	// Name is the engine family this renderer claims.
	Name() string
	// Image is the engine's container image.
	Image() string
	// Command and Args are the process to run. Args are rendered per
	// deployment, so they take the input.
	Command(in Input) ([]string, []string)
	// Accelerators is the default extended resource to request per replica,
	// as a resource name and quantity. An empty name requests none, which
	// is how a CPU-only engine says so.
	Accelerators(in Input) (name string, count int32)
	// HealthPath is the first candidate to use for the readiness probe.
	HealthPath(in Input) string
}

// Registry resolves a renderer by engine name.
type Registry struct {
	mu        sync.RWMutex
	renderers map[string]Renderer
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{renderers: map[string]Renderer{}}
}

// Register adds a renderer, replacing any previous one for the same name.
func (r *Registry) Register(rd Renderer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renderers[strings.ToLower(rd.Name())] = rd
}

// For returns the renderer for an engine name.
//
// Matching allows a family prefix the same way engine.Profiles does, so that
// "llama.cpp" and "vllm-0.9" both land somewhere. A miss is a NotFound error
// naming what is available, because the most likely cause is a typo in a CRD
// and the fix is to read the list.
func (r *Registry) For(name string) (Renderer, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	r.mu.RLock()
	defer r.mu.RUnlock()
	if rd, ok := r.renderers[key]; ok {
		return rd, nil
	}
	for n, rd := range r.renderers {
		if strings.HasPrefix(key, n) {
			return rd, nil
		}
	}
	return nil, errs.NotFound("no renderer for engine %q; this build can render: %s",
		name, strings.Join(r.Names(), ", "))
}

// Names lists the registered engine families, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.renderers))
	for n := range r.renderers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Result is what a render produced: objects to apply, plus the facts a status
// needs that only the renderer could know.
type Result struct {
	Objects []Object
	// Selector is the model name clients address this deployment by.
	Selector string
	// Address is the in-cluster endpoint.
	Address string
	// Name is the rendered workload's name, which is not the
	// FleetDeployment's: a Service cannot contain the dots a model name
	// usually has. Reading the status back means using this rather than
	// assuming the two are the same.
	Name string
	// GPUPerReplica is what one replica asks the scheduler for.
	GPUPerReplica int32
	// Format is what the model actually turned out to be, carried so a
	// status can report the facts rather than re-deriving them.
	Format weights.Format
}

// Object is one rendered Kubernetes object.
//
// The mutation is carried as a closure rather than as a typed value so that
// neither the reconciler nor this package has to switch on object kind: the
// thing that knows what a Service looks like is the function that built it.
type Object struct {
	Kind string
	Name string
	// Apply copies the desired state onto an existing or empty object. It
	// never touches the resourceVersion, so a reconcile that changes nothing
	// writes nothing.
	Apply func(dst client.Object) error
}

// selectorFor is the model name a deployment is addressed by.
//
// It is the FleetModel's own name rather than anything the engine reports,
// because the engine's answer is a file path unless a flag was passed and a
// file path is not something a client can be asked to send.
func selectorFor(m api.FleetModel) string {
	return m.Name
}

// addressFor is the in-cluster DNS name of a deployment's Service.
//
// A Service name, never a Pod IP: Pod IPs change on every rescheduling, and an
// address that changes under the gateway is indistinguishable from an outage.
func addressFor(name, namespace string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", name, namespace)
}

// objectMeta is the metadata every rendered object carries.
func objectMeta(in Input, component string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      in.Deployment.Name,
		Namespace: in.Deployment.Namespace,
		Labels: map[string]string{
			"app.kubernetes.io/name":       in.Deployment.Name,
			"app.kubernetes.io/managed-by": "fleet-operator",
			"fleet.zlogic.com/component":   component,
			"fleet.zlogic.com/deployment":  in.Deployment.Name,
		},
	}
}

// dnsName is a Kubernetes object name derived from a FleetDeployment name.
//
// A Service is named by an RFC 1035 label, which admits no dots, and a model
// called "qwen-0.5b" has one — so the deployment's own name cannot be reused
// for the Service it fronts. Replacing the invalid characters is not enough on
// its own, because "qwen-0.5b" and "qwen-0-5b" then both become
// "qwen-0-5b" and two deployments would fight over one Service. A short digest
// of the original keeps them apart, and keeps the name stable across
// reconciles, which is what lets the gateway hold on to the address.
func dnsName(name string) string {
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:4])

	lower := strings.ToLower(name)
	var b strings.Builder
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "fleet"
	}
	// A Service name is 63 characters at most, and the digest has to fit
	// inside that budget rather than pushing the name over.
	if room := 63 - len(suffix); len(out) > room {
		out = strings.Trim(out[:room], "-")
	}
	// A Service name must start with a letter. Prefixing rather than trimming
	// keeps the digest at the end, where truncation cannot reach it.
	if c := out[0]; c < 'a' || c > 'z' {
		out = "x" + out
	}
	return out + suffix
}
