package handler

import (
	"github.com/zlogic-labs/fleet/core/internal/gateway/transport"
	"github.com/zlogic-labs/fleet/core/pkg/billing"
	"github.com/zlogic-labs/fleet/core/pkg/engine"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

// What a request is charged for, when the engine did not say.
//
// P6's fallback used to bill max_tokens, and that is the wrong direction for
// every number involved. A client that asked for 4096 and received forty was
// charged for 4096, so the error grew with the size of the request the client
// had bothered to bound — the well-behaved callers were the ones paying most.
// What Fleet does instead is count the text it forwarded, which is a
// measurement of the payload rather than a belief about the engine's internals.

// measure decides what this request is charged for.
//
// Three cases, in order of how much they can be trusted:
//
//  1. The engine reported usage. That figure is used unchanged, because the
//     prompt cache the engine counted and the one the gateway would count are
//     not the same cache.
//  2. It reported nothing, and there is an answer. Fleet counts the text it
//     forwarded, with the tokenizer that already counted the prompt.
//  3. It reported nothing and there is no answer either — a stream cut before
//     any frame, a response with no text at all. The reservation is the only
//     figure left, so it is used, and the row says so.
//
// The prompt is counted by the gateway in every case. That is not a second
// guess at the engine's accounting: the engine's number is used for the whole
// request when there is one, and the two are never mixed within a row.
func (s *settler) measure(ep engine.Endpoint, result transport.Result, promptTokens, completion int) (openai.Usage, billing.Source) {
	if result.UsageKnown && result.Usage != nil {
		return *result.Usage, billing.SourceEngine
	}
	prompt := promptTokens
	if result.Text != "" && s.Tokens != nil {
		n := s.Tokens.Resolve(ep.Model, "").Count(result.Text)
		if n > 0 {
			return s.counted(ep, result, prompt, n), billing.SourceCounted
		}
	}
	// No text at all. The tokens may still have been spent, so the row exists
	// and is charged, at the reservation.
	return openai.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
	}, billing.SourceReserved
}

// counted reports a capped answer as a floor rather than as a measurement.
//
// The tap keeps 256 KiB, so an answer longer than that is counted in part. It
// is still far closer than the reservation, and the direction is right — it
// under-bills rather than billing a length nobody read — but it must not read
// as exact, which is why both the row and the log say so.
func (s *settler) counted(ep engine.Endpoint, result transport.Result, prompt, n int) openai.Usage {
	if result.TextTruncated {
		s.Log.Warn("the answer was longer than the tap keeps; the token count is a floor",
			"model", ep.Model, "endpoint", ep.ID, "counted", n)
	}
	return openai.Usage{
		PromptTokens:     prompt,
		CompletionTokens: n,
		TotalTokens:      prompt + n,
	}
}

// engineTimings converts what the tap read into what the ledger stores.
//
// The two types are separate on purpose. transport describes an observation;
// billing describes a row, and a row outlives the request that produced it. A
// nil in, a nil out: an engine that published no timings leaves the column
// null rather than zero, because zero queue time is a claim about a deployment
// that nobody made.
func engineTimings(result transport.Result) *billing.EngineTimings {
	if result.Engine == nil {
		return nil
	}
	return &billing.EngineTimings{
		QueueMS:  result.Engine.QueueMS,
		TTFTMS:   result.Engine.TTFTMS,
		DecodeMS: result.Engine.DecodeMS,
	}
}

// queuedMS is the engine's queue figure for the console, or nil.
//
// Nil is the answer for every engine that does not publish one, which is most
// of them: llama-server in particular reports a span that covers queueing and
// prompt evaluation together, and calling that a queue time would draw a line
// on the chart that no measurement supports.
func queuedMS(result transport.Result) *float64 {
	if result.Engine == nil {
		return nil
	}
	return result.Engine.QueueMS
}
