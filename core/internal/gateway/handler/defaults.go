package handler

import (
	"github.com/zlogic-labs/fleet/core/internal/gateway/ratelimit"
	"github.com/zlogic-labs/fleet/core/pkg/tokenizer"
)

// One place where a handler's options get their defaults.
//
// Both constructors call it because two constructors that each own a copy is
// two places for one of them to be forgotten, and forgetting one is not an
// error anywhere — it looks exactly like a gateway that received no traffic.
//
// It has already happened twice. The chat handler reached settlement with no
// observer, so a gateway published endpoint and build metrics and no request
// metrics at all. Then the same handler reached settlement with no tokenizer, so
// every request to an engine that reports no usage silently reverted to billing
// the reservation.
func withDefaults(opts ChatOptions, tokens tokenizer.Resolver) ChatOptions {
	if opts.SampleBuffer <= 0 {
		opts.SampleBuffer = 32
	}
	if opts.DefaultMaxTokens <= 0 {
		// The same figure config.go configures, reached from the other
		// direction. It was 1024 here, which is a second and unstated answer
		// to "what may one request cost": in the gateway the config always
		// wins, so it only ever applied to a caller constructing a handler
		// directly — and a library caller is exactly who would read 1024 as
		// Fleet's position on the question.
		//
		// The number lives in one place. If you change it, change it here.
		opts.DefaultMaxTokens = 4096
	}
	if opts.Tokens == nil {
		// Settlement counts an unreported answer with this. A nil here would
		// degrade every such request back to billing the reservation, which is
		// the behaviour this exists to remove.
		opts.Tokens = tokens
	}
	if opts.Limiter == nil {
		// An unlimited limiter rather than a nil check on every request: the
		// handler's hot path should not branch on whether the deployment
		// configured a limit.
		opts.Limiter = ratelimit.NewMemory(nil)
	}
	return opts
}
