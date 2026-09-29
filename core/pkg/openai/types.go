// Package openai models the subset of the OpenAI wire protocol that Fleet
// speaks and inspects. It exists so that the gateway, the usage collector and
// the tokenizer all decode requests the same way, and so that clients using
// the official SDKs see byte-identical responses.
package openai

import (
	"encoding/json"
	"strings"
)

// Usage is the only token accounting the platform trusts for billing.
// P6 in docs/architecture.md: nothing reported by the client is used.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`

	// CachedTokens is priced differently from fresh prompt tokens on most
	// providers, so it is a first-class billing dimension rather than an extra.
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	// ReasoningTokens are billed as output on OpenAI-style pricing.
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
	AudioTokens  int `json:"audio_tokens,omitempty"`
}

type CompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
	AudioTokens     int `json:"audio_tokens,omitempty"`
}

// FreshPromptTokens is the prompt token count excluding the cache hit, which
// is the quantity most price books charge at the full input rate.
func (u Usage) FreshPromptTokens() int {
	cached := 0
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	if fresh := u.PromptTokens - cached; fresh > 0 {
		return fresh
	}
	return 0
}

func (u Usage) CachedPromptTokens() int {
	if u.PromptTokensDetails != nil {
		return u.PromptTokensDetails.CachedTokens
	}
	return 0
}

func (u Usage) ReasoningTokens() int {
	if u.CompletionTokensDetails != nil {
		return u.CompletionTokensDetails.ReasoningTokens
	}
	return 0
}

// Message is a single chat turn. Content is a Content so that both the string
// and the multipart form decode without branching at every call site.
type Message struct {
	Role       string     `json:"role"`
	Content    Content    `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function FunctionSpec `json:"function"`
}

type FunctionSpec struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

// ChatRequest is the subset of /v1/chat/completions that Fleet inspects.
// The struct is intentionally permissive: the raw body is forwarded upstream
// untouched, and only the fields below are read out of it.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream,omitempty"`

	MaxTokens   *int     `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`

	// StreamOptions.IncludeUsage makes the engine emit a final usage-only
	// chunk. We always set it, because without it a streaming response has no
	// token counts at all and the request becomes unbillable.
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`

	Tools []Tool   `json:"tools,omitempty"`
	Stop  []string `json:"stop,omitempty"`
	Seed  *int64   `json:"seed,omitempty"`
	User  string   `json:"user,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason *string `json:"finish_reason"`
}

type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason,omitempty"`
}

type Delta struct {
	Role      string     `json:"role,omitempty"`
	Content   string     `json:"content,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

type EmbeddingRequest struct {
	Model          string `json:"model"`
	Input          any    `json:"input"`
	EncodingFormat string `json:"encoding_format,omitempty"`
	Dimensions     *int   `json:"dimensions,omitempty"`
	User           string `json:"user,omitempty"`
}

type EmbeddingResponse struct {
	Object string          `json:"object"`
	Data   []EmbeddingItem `json:"data"`
	Model  string          `json:"model"`
	Usage  *Usage          `json:"usage,omitempty"`
}

type EmbeddingItem struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// Prefix is the conversation head used as the routing-affinity key. Long
// system prompts and few-shot examples are what actually live in a KV cache,
// so the hash must cover them and nothing about the newest turn.
func (r *ChatRequest) Prefix(runeLen int) string {
	var b strings.Builder
	for _, m := range r.Messages {
		if b.Len() >= runeLen {
			break
		}
		b.WriteString(m.Role)
		b.WriteByte('\n')
		b.WriteString(m.Content.String())
		b.WriteByte('\n')
	}
	s := b.String()
	if len([]rune(s)) <= runeLen {
		return s
	}
	return string([]rune(s)[:runeLen])
}

// ResolveMaxTokens returns the output budget the caller committed to, which is
// the amount the limiter reserves up front.
func (r *ChatRequest) ResolveMaxTokens(defaultLimit int) int {
	if r.MaxTokens != nil && *r.MaxTokens > 0 {
		return *r.MaxTokens
	}
	return defaultLimit
}

// DecodeChatRequest is the single entry point for reading a request body, so
// that a malformed body produces the same error shape everywhere.
func DecodeChatRequest(body []byte) (*ChatRequest, error) {
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return &req, nil
}
