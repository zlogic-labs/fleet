package transport

import (
	"bytes"
	"encoding/json"
)

// Counting the answer when the engine will not.
//
// P6 used to read "only the engine's usage is trusted" and the fallback for a
// missing one was to bill the reservation. That is defensible as a direction —
// bill something rather than nothing — and wrong as a number: a client that
// asks for max_tokens=4096 and receives forty tokens was charged for 4096, so
// the shorter the answer the worse the error, on precisely the engines least
// able to report.
//
// So the gateway counts what it forwarded. Not the client's claim, and not the
// engine's accounting: the bytes that crossed the proxy, tokenised by the same
// tokenizer that already counts the prompt. That is a measurement of the
// payload rather than a belief about what happened inside the engine, and it is
// wrong by a percent or two instead of by a factor of a hundred.

// textCap bounds the answer the tap will hold for counting.
//
// Text, not frames: the point is to keep the characters and discard
// everything else. 256 KiB is beyond the completion limit of every model worth
// running, so in practice the cap never bites and the ones it protects are the
// ones where an undercount is still far better than a 4096-token guess.
const textCap = 1 << 18

// Any of these means the frame carries characters the tenant will be billed
// for. Checking for the substring first is what keeps the JSON parser off the
// frames that carry nothing: the opening role delta, the [DONE] sentinel, a
// keep-alive comment, and every frame of a stream that only ever calls tools.
var textMarkers = [][]byte{
	[]byte(`"content"`),
	[]byte(`"reasoning_content"`),
	[]byte(`"arguments"`),
}

// textChunk is the smallest shape that carries an answer, in either a streaming
// delta or a whole message. Declared here rather than reusing openai.ChatChunk
// because that one carries the usage pointer the tap is already reading through
// a different gate, and because Delta.Content is a plain string while a
// non-streamed message content may be the multipart form.
//
// reasoning_content is included because a reasoning model emits it as
// completion tokens and the engine's own usage counts it: a gateway that
// counted only content would under-report the model that costs the most to run.
type textChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
}

func (t *Tap) considerText(payload []byte) {
	if t.textFull {
		return
	}
	hit := false
	for _, m := range textMarkers {
		if bytes.Contains(payload, m) {
			hit = true
			break
		}
	}
	if !hit {
		return
	}
	var chunk textChunk
	if json.Unmarshal(payload, &chunk) != nil {
		return
	}
	t.appendText(chunk)
}

// appendText adds one frame's characters, in the order a client would read
// them: the answer, the reasoning that produced it, then any tool-call
// arguments.
func (t *Tap) appendText(chunk textChunk) {
	for _, c := range chunk.Choices {
		t.add(c.Delta.Content)
		t.add(c.Delta.ReasoningContent)
		t.add(c.Message.Content)
		t.add(c.Message.ReasoningContent)
		for _, tc := range c.Delta.ToolCalls {
			t.add(tc.Function.Arguments)
		}
	}
}

// add appends one fragment, and stops at the cap rather than growing without
// bound: a single response must not be able to pin memory proportional to
// whatever length a client asked the model to produce. The fragment that
// crosses the line is cut rather than dropped, so a long answer degrades to a
// slightly short count instead of to nothing.
func (t *Tap) add(s string) {
	if s == "" || t.textFull {
		return
	}
	if room := textCap - len(t.text); len(s) > room {
		t.text = append(t.text, s[:room]...)
		t.textFull = true
		return
	}
	t.text = append(t.text, s...)
}
