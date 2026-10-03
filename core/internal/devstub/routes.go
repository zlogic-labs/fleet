package devstub

import "net/http"

// The stub's route table, split out of engine.go so that file is under the
// same size limit as the rest of the tree. It lists what the stub answers and
// nothing else, which is the honest description of a test fixture.
func (e *Engine) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeText(w, http.StatusOK, "ok\n")
	})
	mux.HandleFunc("/v1/models", e.models)
	mux.HandleFunc("/v1/chat/completions", e.chat)
	mux.HandleFunc("/v1/embeddings", e.embeddings)
	return mux
}
