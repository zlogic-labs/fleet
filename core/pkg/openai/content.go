package openai

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ContentPart is one element of a multimodal message. Only the fields we act
// on are modelled.
//
// An image_url or input_audio part decodes to a Type and nothing else: the
// payload is not captured, so Text is empty and the part contributes nothing to
// a token estimate. That is the intended reading rather than an oversight --
// the request is forwarded as the bytes the client sent (ensureIncludeUsage
// edits one byte position and never re-encodes), so the engine still receives
// the image; what is lost is Fleet's own estimate of its prompt, which is why
// an engine's reported usage outranks it (P6) and why the row says which of
// the two it used.
type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Content holds a message body that may arrive either as a plain string or as
// an array of parts, depending on the client and the model. Decoding both
// into one type keeps prefix-affinity routing from special-casing every caller.
type Content struct {
	Text  string
	Parts []ContentPart
}

func (c *Content) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	if trimmed[0] == '"' {
		return json.Unmarshal(trimmed, &c.Text)
	}
	return json.Unmarshal(trimmed, &c.Parts)
}

func (c Content) MarshalJSON() ([]byte, error) {
	if len(c.Parts) == 0 {
		return json.Marshal(c.Text)
	}
	return json.Marshal(c.Parts)
}

// IsZero lets callers skip empty content when building a request from scratch.
func (c Content) IsZero() bool { return c.Text == "" && len(c.Parts) == 0 }

// String flattens the content to text. Non-text parts (images, audio) are
// skipped, which is the correct input for a text tokenizer estimate and for
// the prompt-prefix hash.
func (c Content) String() string {
	if len(c.Parts) == 0 {
		return c.Text
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p.Type == "text" || p.Type == "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
