package gateway

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// The environment layer.
//
// Separate from config.go because it is the only part of loading that reaches
// outside the process, and it is the part that has to stay right when a
// container is reconfigured without a ConfigMap rewrite. Every override here is
// "if set, it wins" — an unset variable must leave the file's value alone, or a
// deployment could not express "use the file default".

// applyEnv overlays FLEET_* variables onto a config.
//
// Environment last means a container can be reconfigured without rewriting a
// ConfigMap, which is the common case for image promotion.
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
	setString(&cfg.Database.URL, "FLEET_DATABASE_URL")

	if v := os.Getenv("FLEET_DATABASE_MIGRATE"); v != "" {
		cfg.Database.Migrate = v == "1" || strings.EqualFold(v, "true")
	}

	// Semicolon-separated, for the same reason the key list is: neither a
	// tenant name nor a limit suffix contains a comma.
	if raw := os.Getenv("FLEET_RATE_TENANTS"); raw != "" {
		cfg.RateLimits.Tenants = splitList(raw)
	}
	if raw := os.Getenv("FLEET_RATE_PROJECTS"); raw != "" {
		cfg.RateLimits.Projects = splitList(raw)
	}

	if v := os.Getenv("FLEET_AUTH_REQUIRED"); v != "" {
		cfg.Auth.Required = v == "1" || strings.EqualFold(v, "true")
	}
	if raw := os.Getenv("FLEET_API_KEYS"); raw != "" {
		cfg.Auth.Keys = splitList(raw)
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

// parseUpstream reads "model=…,url=…,id=…,key=…,engine=…,replicas=…".
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

// setInt and setDuration silently ignore a value that does not parse.
//
// Quietly, because a typo in a container's environment should not stop the
// process from booting — but also not silently, because a limit that fails to
// parse and is then ignored is a limit nobody believes is in force. The
// zero-value fallback is the server default, which is the safe direction for a
// timeout and the documented one for a rate limit.
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
