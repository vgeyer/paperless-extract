// Package server provides the HTTP handlers of the service.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vgeyer/paperless-extract/internal/queue"
)

// MaxBodyBytes limits the webhook request body.
const MaxBodyBytes = 64 << 10

// ErrNoDocID is returned (wrapped) by ParseDocID.
var ErrNoDocID = errors.New("no document id in doc_url")

// Enqueuer is satisfied by *queue.Queue.
type Enqueuer interface {
	Enqueue(docID int) error
}

// Check is one readiness probe; nil = healthy.
type Check func(ctx context.Context) error

// Options configures New.
type Options struct {
	Secret       string
	Queue        Enqueuer
	Ready        map[string]Check
	ReadyTimeout time.Duration
	Logger       *slog.Logger
}

type handler struct {
	opts Options
	log  *slog.Logger
}

// New returns the HTTP handler with routes POST /webhook, GET /healthz, GET /readyz.
func New(opts Options) http.Handler {
	h := &handler{opts: opts, log: opts.Logger}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	if h.opts.ReadyTimeout <= 0 {
		h.opts.ReadyTimeout = 5 * time.Second
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook", h.webhook)
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /readyz", h.readyz)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }

func (h *handler) webhook(w http.ResponseWriter, r *http.Request) {
	got := sha256.Sum256([]byte(r.Header.Get("X-Webhook-Secret")))
	want := sha256.Sum256([]byte(h.opts.Secret))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		h.log.Warn("webhook unauthorized", "remote_addr", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, errBody("unauthorized"))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid body"))
		return
	}
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		writeJSON(w, http.StatusBadRequest, errBody("invalid body"))
		return
	}
	var body struct {
		DocURL json.RawMessage `json:"doc_url"`
	}
	if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid body"))
		return
	}
	var docURL string
	if len(body.DocURL) > 0 {
		_ = json.Unmarshal(body.DocURL, &docURL) // non-string -> empty -> no document id
	}
	id, err := ParseDocID(docURL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("no document id"))
		return
	}

	switch err := h.opts.Queue.Enqueue(id); {
	case err == nil:
		h.log.Info("webhook accepted", "doc_id", id)
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued", "doc_id": id})
	case errors.Is(err, queue.ErrDuplicate):
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "duplicate", "doc_id": id})
	case errors.Is(err, queue.ErrFull):
		h.log.Warn("queue full", "doc_id", id)
		writeJSON(w, http.StatusServiceUnavailable, errBody("queue full"))
	case errors.Is(err, queue.ErrClosed):
		writeJSON(w, http.StatusServiceUnavailable, errBody("shutting down"))
	default:
		h.log.Error("enqueue failed", "doc_id", id, "error", err.Error())
		writeJSON(w, http.StatusInternalServerError, errBody("internal error"))
	}
}

func (h *handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), h.opts.ReadyTimeout)
	defer cancel()

	checks := make(map[string]string, len(h.opts.Ready))
	var mu sync.Mutex
	var wg sync.WaitGroup
	failed := false
	for name, c := range h.opts.Ready {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := "ok"
			if err := c(ctx); err != nil {
				res = err.Error()
			}
			mu.Lock()
			checks[name] = res
			if res != "ok" {
				failed = true
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if failed {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "checks": checks})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "checks": checks})
}

// ParseDocID extracts the Paperless document ID from a doc_url.
func ParseDocID(docURL string) (int, error) {
	s := strings.TrimSpace(docURL)
	if s == "" {
		return 0, fmt.Errorf("%w: empty", ErrNoDocID)
	}
	u, err := url.Parse(s)
	if err != nil {
		return 0, fmt.Errorf("%w: %w", ErrNoDocID, err)
	}
	var segs []string
	for _, seg := range strings.Split(u.Path, "/") {
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	for i := len(segs) - 2; i >= 0; i-- {
		if segs[i] != "documents" || !allDigits(segs[i+1]) {
			continue
		}
		n, err := strconv.Atoi(segs[i+1])
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%w: invalid id", ErrNoDocID)
		}
		return n, nil
	}
	return 0, ErrNoDocID
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
