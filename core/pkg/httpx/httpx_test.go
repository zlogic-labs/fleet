package httpx

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// runLog mounts the two middleware and returns the recorded lines plus the
// client-visible response.
func runLog(t *testing.T, handler http.Handler) ([]string, *httptest.ResponseRecorder) {
	t.Helper()
	var lines []string
	log := slog.New(slog.NewTextHandler(writerFunc(func(b []byte) {
		if line := strings.TrimSpace(string(b)); line != "" {
			lines = append(lines, line)
		}
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))

	rec := httptest.NewRecorder()
	RequestLog(log)(handler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	return lines, rec
}

type writerFunc func([]byte)

func (f writerFunc) Write(b []byte) (int, error) { f(b); return len(b), nil }

// The status a handler logs must be the one the client received. A handler
// that answers 200 and then fails wrote both statuses; net/http keeps the
// first, so the log has to as well.
func TestTheLoggedStatusIsTheOneTheClientReceived(t *testing.T) {
	lines, rec := runLog(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("committed"))
		w.WriteHeader(http.StatusInternalServerError)
	}))

	if rec.Code != http.StatusOK {
		t.Fatalf("the client must keep the first status, got %d", rec.Code)
	}
	if !strings.Contains(lines[0], "status=200") {
		t.Fatalf("the log must report what the client saw: %s", lines[0])
	}
	if strings.Contains(lines[0], "status=500") {
		t.Fatalf("a status the client never received was logged: %s", lines[0])
	}
}

// Bytes are counted so a log line can tell a 2-byte refusal from a 2 KiB one.
func TestTheLogCarriesTheResponseSize(t *testing.T) {
	lines, _ := runLog(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("0123456789"))
	}))
	if !strings.Contains(lines[0], "bytes="+strconv.Itoa(10)) {
		t.Fatalf("want bytes=10: %s", lines[0])
	}
}

// Fail is how a handler names a cause it must not put on the wire.
func TestAFailedHandlerNamesItsCauseInTheLog(t *testing.T) {
	cause := errors.New("relation budget_rules does not exist")

	lines, rec := runLog(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r, ok := w.(interface{ Fail(error) })
		if !ok {
			t.Fatal("the writer must expose Fail")
		}
		r.Fail(cause)
		w.WriteHeader(http.StatusInternalServerError)
	}))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
	if body, _ := io.ReadAll(rec.Body); strings.Contains(string(body), "budget_rules") {
		t.Fatalf("the cause must not reach the client: %s", body)
	}
	if !strings.Contains(lines[0], "budget_rules does not exist") {
		t.Fatalf("the cause must reach the log: %s", lines[0])
	}
}

// A streamed answer can commit a 200 and several frames before something
// downstream panics. There is no envelope left to write into that response --
// appending one puts a JSON object in the middle of an SSE stream.
func TestAPanicAfterTheFirstFrameDoesNotCorruptTheStream(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := NewRecorder(rec)

	defer func() {
		if v := recover(); v != nil {
			t.Fatalf("unexpected panic: %v", v)
		}
	}()
	func() {
		defer func() {
			if v := recover(); v != nil {
				if !HeaderWritten(WithRecorder(req, w)) {
					t.Error("a panic with nothing written should be answerable")
				}
				return
			}
			t.Error("the inner handler must panic")
		}()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"delta\":\"hi\"}\n\n"))
		panic("engine closed the connection")
	}()

	body, _ := io.ReadAll(rec.Body)
	if !strings.HasPrefix(string(body), "data: ") {
		t.Fatalf("the stream must start with a frame, got %q", body)
	}
	if strings.Contains(string(body), `"error"`) {
		t.Fatalf("an envelope was appended to an SSE stream: %q", body)
	}
}
