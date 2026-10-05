package apiserver

import (
	"encoding/json"
	"net/http"
)

// ── plumbing ───────────────────────────────────────────────────

var errNoStore = &noStoreError{}

type noStoreError struct{}

func (*noStoreError) Error() string {
	return "no object store configured; the control plane has nowhere to put weights"
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}
