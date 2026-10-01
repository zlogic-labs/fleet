// Package gateway wires the OpenAI-compatible surface: middleware chain, route
// table, and the handlers that turn a client request into a proxied one.
package gateway

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/zlogic-labs/fleet/core/pkg/authn"
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

type LimitConfig struct {
	RPM int `yaml:"requests_per_minute"`
	TPM int `yaml:"tokens_per_minute"`
}

// AuthConfig is how callers identify themselves.
//
// Keys is a list of "tenant/keyid" or "tenant/keyid|rpm=10,tpm=1000". It is
// a list in configuration rather than a database table because v1 has no key
// management API, and an operator who needs a working gateway today should not
// first have to stand up Postgres to get one.
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
// Environment last means a container can be reconfigured without rewriting a
// ConfigMap, which is the common case for image promotion.
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

func applyEnv(cfg *Config) {
	setString(&cfg.Listen, "FLEET_LISTEN")
	setString(&cfg.Edition, "FLEET_EDITION")
	setInt(&cfg.MaxBodyMB, "FLEET_MAX_BODY_MB")
	setInt(&cfg.RateLimits.RPM, "FLEET_RATE_RPM")
	setInt(&cfg.RateLimits.TPM, "FLEET_RATE_TPM")
	setInt(&cfg.DefaultMaxTokens, "FLEET_DEFAULT_MAX_TOKENS")
	setDuration(&cfg.Timeouts.Dial, "FLEET_TIMEOUT_DIAL")
	setDuration(&cfg.Timeouts.ResponseHdrs, "FLEET_TIMEOUT_RESPONSE_HEADERS")
	setString(&cfg.ControlPlane.URL, "FLEET_CONTROL_PLANE_URL")
	setString(&cfg.ControlPlane.Token, "FLEET_CONTROL_PLANE_TOKEN")
	setDuration(&cfg.ControlPlane.Every, "FLEET_CONTROL_PLANE_REFRESH")

	if v := os.Getenv("FLEET_AUTH_REQUIRED"); v != "" {
		cfg.Auth.Required = v == "1" || strings.EqualFold(v, "true")
	}
	// Semicolon-separated, because a key list has no natural comma-free form
	// and a comma would have to be escaped inside a value nobody reads.
	if raw := os.Getenv("FLEET_API_KEYS"); raw != "" {
		for _, spec := range strings.Split(raw, ";") {
			if s := strings.TrimSpace(spec); s != "" {
				cfg.Auth.Keys = append(cfg.Auth.Keys, s)
			}
		}
	}

	// A single upstream may be supplied inline, which is the shape of a local
	// llama.cpp or a vLLM pod that has not been adopted yet.
	if raw := os.Getenv("FLEET_UPSTREAMS"); raw != "" {
		for _, spec := range strings.Split(raw, ";") {
			if ep, err := parseUpstream(spec); err == nil && ep.BaseURL != "" {
				cfg.Upstreams = append(cfg.Upstreams, ep)
			}
		}
	}
}

// parseUpstream reads "model=...,url=...,id=...,key=...,engine=...,replicas=...".
func parseUpstream(spec string) (UpstreamConfig, error) {
	var ep UpstreamConfig
	for _, kv := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		switch k {
		case "id":
			ep.ID = v
		case "model":
			ep.Model = v
		case "url":
			ep.BaseURL = v
		case "engine":
			ep.Engine = v
		case "key":
			ep.APIKey = v
		case "replicas":
			// Not optional: without it the endpoint reports zero replicas,
			// which the console shows next to a healthy engine and the Fleet
			// page reads as an engine with nothing serving it.
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				ep.Replicas = n
			}
		}
	}
	return ep, nil
}

func (c Config) validate() error {
	if c.Listen == "" {
		return fmt.Errorf("listen address is empty")
	}
	if c.MaxBodyMB <= 0 {
		return fmt.Errorf("max_body_mb must be positive, got %d", c.MaxBodyMB)
	}
	if c.ControlPlane.Every < 0 {
		return fmt.Errorf("control_plane.refresh_every must not be negative")
	}
	// Required with no keys is a deployment that refuses every request, which
	// looks like an outage rather than a misconfiguration. Caught here so it
	// is a startup error naming the fix, not a 401 storm.
	if c.Auth.Required && len(c.Auth.Keys) == 0 {
		return fmt.Errorf("auth.required is set but auth.keys is empty: every request would be rejected")
	}
	// A configured key list with required off means the keys are being parsed
	// and then ignored, which is the shape of a security setting someone
	// believes is on.
	if !c.Auth.Required && len(c.Auth.Keys) > 0 {
		return fmt.Errorf("auth.keys is set but auth.required is false: the keys would be ignored")
	}
	// The index, not the spec. A config with twenty keys and one typo is
	// found by position; quoting the offending string leaves the operator
	// counting lines.
	for i, spec := range c.Auth.Keys {
		if _, err := authn.ParsePrincipal(spec); err != nil {
			return fmt.Errorf("auth.keys[%d] %q: %w", i, spec, err)
		}
	}
	seen := make(map[string]bool, len(c.Upstreams))
	for i, up := range c.Upstreams {
		if up.BaseURL == "" {
			return fmt.Errorf("upstreams[%d]: base_url is required", i)
		}
		if up.Model == "" {
			return fmt.Errorf("upstreams[%d]: model is required", i)
		}
		if up.ID == "" {
			// Deriving the id from the URL keeps single-upstream configs to
			// two lines, which is how most people first run this.
			up.ID = up.Model + "@" + up.BaseURL
		}
		if seen[up.ID] {
			return fmt.Errorf("upstreams[%d]: duplicate id %q", i, up.ID)
		}
		seen[up.ID] = true
		if up.Engine == "" {
			up.Engine = "openai-compatible"
		}
		if up.Replicas <= 0 {
			up.Replicas = 1
		}
		if up.AffinityPrefixRunes <= 0 {
			up.AffinityPrefixRunes = 512
		}
	}
	return nil
}

func setString(dst *string, key string) {
	if v := os.Getenv(key); v != "" {
		*dst = v
	}
}

func setInt(dst *int, key string) {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func setDuration(dst *time.Duration, key string) {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			*dst = d
		}
	}
}
