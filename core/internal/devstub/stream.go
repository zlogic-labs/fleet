package devstub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The streaming half of the stub, split out of engine.go so that file is under
// the same size limit as the rest of the tree.
func (e *Engine) writeStream(w http.ResponseWriter, r *http.Request, req chatRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "server_error", "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	base := map[string]any{
		"id": "chatcmpl-devstub", "object": "chat.completion.chunk", "model": req.Model,
	}
	send := func(payload map[string]any) bool {
		if err := writeSSE(w, payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !send(withChoice(base, map[string]any{"role": "assistant"}, nil)) {
		return
	}

	// Honouring include_usage is the whole point: a real engine omits the
	// final usage frame when the client did not ask for it, and a stub that
	// always sent one would hide the billing bug this code exists to prevent.
	completion := 0
	for _, word := range strings.Fields(e.cfg.Reply) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(e.cfg.TokenDelay):
		}
		completion++
		if !send(withChoice(base, map[string]any{"content": word + " "}, nil)) {
			return
		}
	}

	// finish_reason is a sibling of delta, not a field inside it. A console
	// reading choices[0].finish_reason gets null otherwise, and the stub would
	// be teaching its readers a shape no real engine emits.
	if !send(withChoice(base, map[string]any{}, "stop")) {
		return
	}

	if req.StreamOptions != nil && req.StreamOptions.IncludeUsage {
		final := map[string]any{
			"id": "chatcmpl-devstub", "object": "chat.completion.chunk",
			"model": req.Model, "choices": []any{}, "usage": e.usage(completion),
		}
		if !send(final) {
			return
		}
	}
	_ = writeRawSSE(w, "[DONE]")
	flusher.Flush()
}

func writeSSE(w http.ResponseWriter, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return writeRawSSE(w, string(raw))
}

func writeRawSSE(w http.ResponseWriter, data string) error {
	_, err := fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}
