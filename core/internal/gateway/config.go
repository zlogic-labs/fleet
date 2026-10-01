// Package gateway wires the OpenAI-compatible surface: middleware chain, route
// table, and the handlers that turn a client request into a proxied one.
package gateway

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole of a single-node deployment's configuration.
//
// Endpoints are declared statically at first because a control plane that
// writes them arrives with the operator module. The shape here is already the
// one the CRD reconciles into, so swapping the loader for a database read is
// not a change to any caller.
type Config struct {
	Listen     string           `yaml:"listen"`
	Edition    string           `yaml:"edition"`
	Upstreams  []UpstreamConfig `yaml:"upstreams"`
	Timeouts   TimeoutConfig    `yaml:"timeouts"`
	MaxBodyMB  int              `yaml:"max_body_mb"`
	RateLimits LimitConfig      `yaml:"rate_limits"`
	Auth       AuthConfig       `yaml:"auth"`
	// DefaultMaxTokens is what one request may generate when it does not say.
	// It is a server ceiling rather than a client default because a client
	// that omits max_tokens has asked for "as much as possible", and the
	// reservation has to be an upper bound (P5).
	DefaultMaxTokens int `yaml:"default_max_tokens"`
	// ControlPlane is the address of fleet-apiserver. When set, the gateway
	// learns its endpoints from the operator's inventory reports instead of
	// only from Upstreams, and the two are merged: static upstreams keep
	// working for a llama.cpp on a laptop with no cluster at all.
	ControlPlane ControlPlaneConfig `yaml:"control_plane"`
}

type ControlPlaneConfig struct {
	URL   string        `yaml:"url"`
	Token string        `yaml:"token"`
	Every time.Duration `yaml:"refresh_every"`
}

type UpstreamConfig struct {
	ID       string `yaml:"id"`
	Model    string `yaml:"model"`
	BaseURL  string `yaml:"base_url"`
	Engine   string `yaml:"engine"`
	Replicas int    `yaml:"replicas"`
	// APIKey authenticates to the upstream. It is the engine's key, not a
	// tenant's; tenants authenticate to the gateway.
	APIKey string `yaml:"api_key"`
	// AffinityPrefixRunes bounds the prompt prefix that participates in the
	// routing hash. Long enough to cover a system prompt, short enough that
	// every request does not hash a whole transcript.
	AffinityPrefixRunes int `yaml:"affinity_prefix_runes"`
}

type TimeoutConfig struct {
	Dial         time.Duration `yaml:"dial"`
	ResponseHdrs time.Duration `yaml:"response_headers"`
	// Total bounds a whole request including generation. Zero means no limit,
	// which is the right default: a long completion is not a failure.
	Total time.Duration `yaml:"total"`
}

// LimitConfig declares rate limits per tenant and per project.
//
// The two levels are separate fields rather than one list with a type column,
// because they answer different questions and fail differently. A tenant entry
// is the envelope: the most that tenant may spend across everything it does. A
// project entry divides that envelope. Merging them into one list would make
// "is this the ceiling or a slice of it" a question only the name could answer.
type LimitConfig struct {
	// RPM and TPM are the server defaults, applied to any tenant with no entry
	// of its own.
	//
	// They stay at the top level rather than moving under a `defaults:` key so
	// an existing deployment that sets them keeps working unchanged. Zero means
	// unlimited, which is also what an anonymous caller gets.
	RPM int `yaml:"requests_per_minute"`
	TPM int `yaml:"tokens_per_minute"`

	// Tenants are the envelopes, as "name" or "name|rpm=…,tpm=…".
	Tenants []string `yaml:"tenants"`
	// Projects are the partitions, as "tenant/project" with the same suffix.
	//
	// A project limit above its tenant's envelope is rejected at startup: it
	// could never bind, so an operator who wrote it believes they have divided
	// a budget they have not divided.
	Projects []string `yaml:"projects"`
}

// AuthConfig is how callers identify themselves.
//
// Keys is a list of "tenant/project/keyid". It is a list in configuration
// rather than a database table because v1 has no key management API, and an
// operator who needs a working gateway today should not first have to stand up
// Postgres to get one.
//
// A key names a project and carries no limits. Limits belong to the scopes in
// LimitConfig, so lowering a budget is a configuration change rather than a key
// rotation — which matters because a rotated key invalidates whatever the
// caller was using when they were cut off.
type AuthConfig struct {
	// Required turns authentication on. Off by default so that
	// `./scripts/dev.sh` works with no keys at all, and stated explicitly so
	// that "authentication is disabled" is a decision rather than an omission.
	Required bool     `yaml:"required"`
	Keys     []string `yaml:"keys"`
}

// Default is what a bare `fleet-gateway` with no config file runs as.
func Default() Config {
	return Config{
		Listen:    ":8080",
		Edition:   "community",
		MaxBodyMB: 32,
		// 4096 is the OpenAI default for gpt-3.5-era models and a sane
		// ceiling for a serving gateway: a request with no max_tokens should
		// not be able to occupy a replica's whole KV cache for minutes.
		DefaultMaxTokens: 4096,
		Timeouts: TimeoutConfig{
			Dial:         5 * time.Second,
			ResponseHdrs: 60 * time.Second,
			Total:        0,
		},
	}
}

// Load reads YAML from path, then applies FLEET_* environment overrides.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config %s: %w", path, err)
		}
		// Decoding onto the defaults leaves unset fields alone, which is what
		// lets a short config file stay short.
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	applyEnv(&cfg)
	return cfg, cfg.validate()
}
