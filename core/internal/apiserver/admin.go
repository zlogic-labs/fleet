package apiserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	sqlstore "github.com/zlogic-labs/fleet/core/internal/store/postgres"
	"github.com/zlogic-labs/fleet/core/pkg/errs"
	"github.com/zlogic-labs/fleet/core/pkg/openai"
)

const maxBodyBytes = 1 << 20

// decode reads a JSON body, refusing anything oversized before it is parsed
// rather than after.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(dst)
	if err != nil {
		writeError(w, errs.InvalidArgument("body: %v", err))
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, err error) {
	openai.WriteError(w, err)
}

// failInternal answers 500 and records the cause for the log. The caller never
// sees it, which is the point — but it has to reach the operator somewhere.
func failInternal(w http.ResponseWriter, err error) {
	if f, ok := w.(interface{ Fail(error) }); ok {
		f.Fail(err)
	}
	writeError(w, errs.Internal(err))
}

// writeStoreErr maps a store error onto a status. A missing row is a 404 and
// everything else is a 500, because the store's other failures are faults
// rather than the caller's mistake and answering 400 would send an operator
// looking for a bad request that does not exist.
func writeStoreErr(w http.ResponseWriter, err error) {
	if sqlstore.NotFound(err) {
		writeError(w, errs.NotFound("%s", cleanErr(err)))
		return
	}
	if sqlstore.Invalid(err) {
		writeError(w, errs.InvalidArgument("%s", cleanErr(err)))
		return
	}
	if sqlstore.Conflict(err) {
		writeError(w, errs.New(errs.KindConflict, "already_exists", "%s", cleanErr(err)))
		return
	}
	failInternal(w, err)
}

// requireDB guards the routes that only exist when a database is configured.
// Tenants, keys and budgets are rows; without a store there is nothing to talk
// about, and a 400 saying so is more honest than an empty list that reads as
// "this platform has no tenants".
func (s *Server) requireDB(w http.ResponseWriter) bool {
	if s.db != nil {
		return true
	}
	writeError(w, errs.InvalidArgument("no database configured; set FLEET_DATABASE_URL"))
	return false
}

// cleanErr strips the driver's prefix so the message names the row, not the
// package.
func cleanErr(err error) string {
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		return msg[i+2:]
	}
	return msg
}

// splitProject breaks "tenant/name" apart. A project id without a tenant is
// rejected rather than defaulted: guessing a tenant would attach a project's
// spending to whoever happened to be created first.
func splitProject(id string) (tenant, project string) {
	t, p, ok := strings.Cut(id, "/")
	if !ok || t == "" || p == "" {
		return "", ""
	}
	t, err1 := url.PathUnescape(t)
	p, err2 := url.PathUnescape(p)
	if err1 != nil || err2 != nil {
		return "", ""
	}
	return t, p
}
