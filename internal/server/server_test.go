package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vgeyer/paperless-extract/internal/queue"
	"github.com/vgeyer/paperless-extract/internal/server"
)

const secret = "s3cret-webhook-value-0123"

func TestParseDocID(t *testing.T) {
	ok := []struct {
		in   string
		want int
	}{
		{"http://paperless:8000/documents/123/", 123},
		{"https://p.example.com/documents/42", 42},
		{"https://p.example.com/documents/42/details", 42},
		{"https://p.example.com/sub/api/documents/7/?x=1#f", 7},
		{"/documents/5/", 5},
		{"  https://h/documents/3/  ", 3},
		{"https://h/documents/1/documents/9/", 9},
		{"https://h/documents/8/documents/", 8},
		{"https://h//documents//11//", 11},
	}
	for _, c := range ok {
		got, err := server.ParseDocID(c.in)
		if err != nil || got != c.want {
			t.Errorf("ParseDocID(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	bad := []string{
		"", "   ", "https://h/documents/", "https://h/",
		"https://h/documents/abc/", "https://h/documents/+5/", "https://h/documents/-1/",
		"https://h/documents/0/", "https://h/documents/00/",
		"https://h/documents/99999999999999999999/",
		"{{doc_url}}",
		"https://h/docs/5/",
		"https://h/documents/5a/",
		"https://h/?documents=5",
		"https://h/x#/documents/5/",
	}
	for _, in := range bad {
		got, err := server.ParseDocID(in)
		if err == nil {
			t.Errorf("ParseDocID(%q) = %d, nil; want error", in, got)
			continue
		}
		if !errors.Is(err, server.ErrNoDocID) {
			t.Errorf("ParseDocID(%q) error %v does not wrap ErrNoDocID", in, err)
		}
	}
}

type fakeQueue struct {
	mu    sync.Mutex
	err   error
	calls []int
}

func (f *fakeQueue) Enqueue(id int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	return f.err
}

func (f *fakeQueue) Calls() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.calls...)
}

func newHandler(q server.Enqueuer, logger *slog.Logger) http.Handler {
	return server.New(server.Options{Secret: secret, Queue: q, Logger: logger})
}

func do(t *testing.T, h http.Handler, method, path string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("response not JSON: %q (%v)", rec.Body.String(), err)
	}
	return m
}

func authed() map[string]string { return map[string]string{"X-Webhook-Secret": secret} }

const validBody = `{"doc_url":"http://paperless:8000/documents/123/","title":"Rechnung","document_type":"Invoice"}`

func TestWebhookUnauthorized(t *testing.T) {
	cases := map[string]map[string]string{
		"missing":  nil,
		"wrong":    {"X-Webhook-Secret": "wrong-secret-value-0000000"},
		"prefix":   {"X-Webhook-Secret": secret[:len(secret)-1]},
		"longer":   {"X-Webhook-Secret": secret + "x"},
		"empty":    {"X-Webhook-Secret": ""},
		"otherhdr": {"Authorization": secret},
	}
	for name, hdr := range cases {
		t.Run(name, func(t *testing.T) {
			q := &fakeQueue{}
			var logBuf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			h := newHandler(q, logger)
			rec := do(t, h, http.MethodPost, "/webhook", strings.NewReader(validBody), hdr)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			m := decodeJSON(t, rec)
			if m["error"] != "unauthorized" {
				t.Errorf("body = %v", m)
			}
			if len(q.Calls()) != 0 {
				t.Errorf("Enqueue called: %v", q.Calls())
			}
			if v := hdr["X-Webhook-Secret"]; v != "" && strings.Contains(logBuf.String(), v) {
				t.Errorf("log contains received secret: %s", logBuf.String())
			}
			if strings.Contains(logBuf.String(), secret) {
				t.Errorf("log contains configured secret")
			}
		})
	}
}

func TestWebhookUnauthorizedBeforeBodyValidation(t *testing.T) {
	q := &fakeQueue{}
	rec := do(t, newHandler(q, nil), http.MethodPost, "/webhook", strings.NewReader("not json"), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestWebhookBadBody(t *testing.T) {
	bodies := map[string]string{
		"empty":     "",
		"notjson":   "doc_url=http://h/documents/1/",
		"array":     `[{"doc_url":"http://h/documents/1/"}]`,
		"string":    `"http://h/documents/1/"`,
		"number":    `42`,
		"truncated": `{"doc_url":"http://h/documents/1/"`,
		"wrongtype": `{"doc_url":123}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			q := &fakeQueue{}
			rec := do(t, newHandler(q, nil), http.MethodPost, "/webhook", strings.NewReader(body), authed())
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
			}
			m := decodeJSON(t, rec)
			if name != "wrongtype" && m["error"] != "invalid body" {
				t.Errorf("error = %v, want invalid body", m["error"])
			}
			if len(q.Calls()) != 0 {
				t.Errorf("Enqueue called")
			}
		})
	}
}

func TestWebhookOversizedBody(t *testing.T) {
	q := &fakeQueue{}
	pad := strings.Repeat("x", server.MaxBodyBytes+10)
	body := `{"title":"` + pad + `","doc_url":"http://h/documents/1/"}`
	rec := do(t, newHandler(q, nil), http.MethodPost, "/webhook", strings.NewReader(body), authed())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if m := decodeJSON(t, rec); m["error"] != "invalid body" {
		t.Errorf("body = %v", m)
	}
	if len(q.Calls()) != 0 {
		t.Errorf("Enqueue called")
	}
}

func TestWebhookNoDocID(t *testing.T) {
	bodies := []string{
		`{}`,
		`{"title":"x"}`,
		`{"doc_url":""}`,
		`{"doc_url":"{{doc_url}}"}`,
		`{"doc_url":"http://h/documents/0/"}`,
		`{"doc_url":null}`,
	}
	for _, body := range bodies {
		q := &fakeQueue{}
		rec := do(t, newHandler(q, nil), http.MethodPost, "/webhook", strings.NewReader(body), authed())
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", body, rec.Code)
			continue
		}
		if m := decodeJSON(t, rec); m["error"] != "no document id" {
			t.Errorf("%s: body = %v", body, m)
		}
		if len(q.Calls()) != 0 {
			t.Errorf("%s: Enqueue called", body)
		}
	}
}

func TestWebhookQueued(t *testing.T) {
	q := &fakeQueue{}
	// Content-Type not enforced, extra fields and trailing data ignored.
	rec := do(t, newHandler(q, nil), http.MethodPost, "/webhook",
		strings.NewReader(validBody+"\n trailing"), map[string]string{"X-Webhook-Secret": secret, "Content-Type": "text/plain"})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	m := decodeJSON(t, rec)
	if m["status"] != "queued" || m["doc_id"] != float64(123) {
		t.Errorf("body = %v", m)
	}
	if c := q.Calls(); len(c) != 1 || c[0] != 123 {
		t.Errorf("calls = %v", c)
	}
}

func TestWebhookEnqueueErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		key    string
		val    string
	}{
		{"duplicate", queue.ErrDuplicate, http.StatusAccepted, "status", "duplicate"},
		{"duplicate_wrapped", errors.Join(errors.New("x"), queue.ErrDuplicate), http.StatusAccepted, "status", "duplicate"},
		{"full", queue.ErrFull, http.StatusServiceUnavailable, "error", "queue full"},
		{"closed", queue.ErrClosed, http.StatusServiceUnavailable, "error", "shutting down"},
		{"other", errors.New("boom"), http.StatusInternalServerError, "error", "internal error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := &fakeQueue{err: c.err}
			rec := do(t, newHandler(q, nil), http.MethodPost, "/webhook", strings.NewReader(validBody), authed())
			if rec.Code != c.status {
				t.Fatalf("status = %d, want %d", rec.Code, c.status)
			}
			m := decodeJSON(t, rec)
			if m[c.key] != c.val {
				t.Errorf("body = %v, want %s=%s", m, c.key, c.val)
			}
			if c.name == "duplicate" && m["doc_id"] != float64(123) {
				t.Errorf("duplicate doc_id = %v", m["doc_id"])
			}
			if len(q.Calls()) != 1 {
				t.Errorf("calls = %v", q.Calls())
			}
		})
	}
}

func TestWebhookWithRealQueueDuplicateAndFull(t *testing.T) {
	q := queue.New(1, 1, func(ctx context.Context, id int) {}, nil) // not started: jobs wait
	t.Cleanup(func() { _ = q.Shutdown(context.Background()) })
	h := newHandler(q, nil)
	post := func(id string) *httptest.ResponseRecorder {
		return do(t, h, http.MethodPost, "/webhook",
			strings.NewReader(`{"doc_url":"http://h/documents/`+id+`/"}`), authed())
	}
	if rec := post("1"); rec.Code != http.StatusAccepted || decodeJSON(t, rec)["status"] != "queued" {
		t.Fatalf("first: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("1"); rec.Code != http.StatusAccepted || decodeJSON(t, rec)["status"] != "duplicate" {
		t.Fatalf("dup: %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("2"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("full: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMethodNotAllowedAndNotFound(t *testing.T) {
	h := newHandler(&fakeQueue{}, nil)
	if rec := do(t, h, http.MethodGet, "/webhook", nil, authed()); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /webhook = %d, want 405", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/healthz", nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz = %d, want 405", rec.Code)
	}
	if rec := do(t, h, http.MethodGet, "/nope", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope = %d, want 404", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	h := server.New(server.Options{Secret: secret, Queue: &fakeQueue{}, Ready: map[string]server.Check{
		"x": func(context.Context) error { return errors.New("down") },
	}})
	rec := do(t, h, http.MethodGet, "/healthz", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if m := decodeJSON(t, rec); m["status"] != "ok" {
		t.Errorf("body = %v", m)
	}
}

func TestReadyz(t *testing.T) {
	ok := func(context.Context) error { return nil }
	t.Run("all ok", func(t *testing.T) {
		h := server.New(server.Options{Secret: secret, Queue: &fakeQueue{}, Ready: map[string]server.Check{"paperless": ok, "ollama": ok}})
		rec := do(t, h, http.MethodGet, "/readyz", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		m := decodeJSON(t, rec)
		checks, _ := m["checks"].(map[string]any)
		if m["status"] != "ok" || checks["paperless"] != "ok" || checks["ollama"] != "ok" || len(checks) != 2 {
			t.Errorf("body = %v", m)
		}
	})
	t.Run("one failing", func(t *testing.T) {
		h := server.New(server.Options{Secret: secret, Queue: &fakeQueue{}, Ready: map[string]server.Check{
			"paperless": ok,
			"ollama":    func(context.Context) error { return errors.New("model missing") },
		}})
		rec := do(t, h, http.MethodGet, "/readyz", nil, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d", rec.Code)
		}
		m := decodeJSON(t, rec)
		checks, _ := m["checks"].(map[string]any)
		if m["status"] != "unavailable" || checks["paperless"] != "ok" || checks["ollama"] != "model missing" {
			t.Errorf("body = %v", m)
		}
	})
	t.Run("empty", func(t *testing.T) {
		h := server.New(server.Options{Secret: secret, Queue: &fakeQueue{}})
		rec := do(t, h, http.MethodGet, "/readyz", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		m := decodeJSON(t, rec)
		checks, isMap := m["checks"].(map[string]any)
		if m["status"] != "ok" || !isMap || len(checks) != 0 {
			t.Errorf("body = %v", m)
		}
	})
	t.Run("timeout and concurrency", func(t *testing.T) {
		// Two blocking checks; each must see its context canceled within ReadyTimeout.
		// If they ran sequentially with a shared budget the result is the same, but
		// both must be reported.
		block := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
		h := server.New(server.Options{Secret: secret, Queue: &fakeQueue{}, ReadyTimeout: 50 * time.Millisecond,
			Ready: map[string]server.Check{"a": block, "b": block}})
		start := time.Now()
		rec := do(t, h, http.MethodGet, "/readyz", nil, nil)
		if el := time.Since(start); el > 2*time.Second {
			t.Errorf("readyz took %v", el)
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d", rec.Code)
		}
		checks, _ := decodeJSON(t, rec)["checks"].(map[string]any)
		if checks["a"] == "ok" || checks["b"] == "ok" || checks["a"] == nil || checks["b"] == nil {
			t.Errorf("checks = %v", checks)
		}
	})
	t.Run("concurrent", func(t *testing.T) {
		// Each check waits for the other to have started: only passes if run concurrently.
		var wg sync.WaitGroup
		wg.Add(2)
		both := make(chan struct{})
		go func() { wg.Wait(); close(both) }()
		chk := func(ctx context.Context) error {
			wg.Done()
			select {
			case <-both:
				return nil
			case <-ctx.Done():
				return errors.New("not concurrent")
			}
		}
		h := server.New(server.Options{Secret: secret, Queue: &fakeQueue{}, ReadyTimeout: 2 * time.Second,
			Ready: map[string]server.Check{"a": chk, "b": chk}})
		rec := do(t, h, http.MethodGet, "/readyz", nil, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})
}
