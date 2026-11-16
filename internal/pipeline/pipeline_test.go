package pipeline_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vgeyer/paperless-extract/internal/extract"
	"github.com/vgeyer/paperless-extract/internal/paperless"
	"github.com/vgeyer/paperless-extract/internal/pipeline"
	"github.com/vgeyer/paperless-extract/internal/retry"
)

type setCall struct {
	docID, fieldID int
	value          string
}

type fakeStore struct {
	mu       sync.Mutex
	doc      *paperless.Document
	getErrs  []error // returned in order, then nil
	setErrs  []error
	gets     int
	sets     []setCall
	setTries int
}

func (s *fakeStore) GetDocument(ctx context.Context, id int) (*paperless.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if s.gets <= len(s.getErrs) && s.getErrs[s.gets-1] != nil {
		return nil, s.getErrs[s.gets-1]
	}
	d := *s.doc
	d.ID = id
	return &d, nil
}

func (s *fakeStore) SetCustomField(ctx context.Context, docID, fieldID int, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setTries++
	if s.setTries <= len(s.setErrs) && s.setErrs[s.setTries-1] != nil {
		return s.setErrs[s.setTries-1]
	}
	s.sets = append(s.sets, setCall{docID, fieldID, value})
	return nil
}

type fakeExtractor struct {
	mu    sync.Mutex
	texts []string
	ex    extract.Extraction
	err   error
}

func (f *fakeExtractor) Extract(ctx context.Context, text string) (extract.Extraction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, text)
	return f.ex, f.err
}

func (f *fakeExtractor) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.texts)
}

const fieldID = 7

func doc(fields ...paperless.CustomFieldInstance) *paperless.Document {
	return &paperless.Document{Title: "Rechnung", Content: "Gesamt 1.234,56 EUR", CustomFields: fields}
}

func cf(field int, raw string) paperless.CustomFieldInstance {
	var v json.RawMessage
	if raw != "" {
		v = json.RawMessage(raw)
	}
	return paperless.CustomFieldInstance{Field: field, Value: v}
}

func goodExtraction() extract.Extraction {
	return extract.Extraction{Amount: decimal.RequireFromString("1234.56"), Currency: "EUR", AmountRaw: "1.234,56"}
}

func newPipeline(s *fakeStore, e *fakeExtractor, logger *slog.Logger) *pipeline.Pipeline {
	return &pipeline.Pipeline{
		Store: s, Extractor: e, FieldID: fieldID,
		Retry:  retry.Policy{Attempts: 3, BaseDelay: time.Millisecond},
		Logger: logger,
	}
}

func TestProcessWritten(t *testing.T) {
	s := &fakeStore{doc: doc(cf(3, `"abc"`))}
	e := &fakeExtractor{ex: goodExtraction()}
	out, err := newPipeline(s, e, nil).Process(context.Background(), 123)
	if err != nil || out != pipeline.OutcomeWritten {
		t.Fatalf("Process = %s, %v", out, err)
	}
	if len(e.texts) != 1 || e.texts[0] != "Gesamt 1.234,56 EUR" {
		t.Errorf("extractor got %q", e.texts)
	}
	if len(s.sets) != 1 || s.sets[0] != (setCall{123, fieldID, "EUR1234.56"}) {
		t.Errorf("sets = %+v", s.sets)
	}
}

func TestProcessFormatsMonetary(t *testing.T) {
	s := &fakeStore{doc: doc()}
	e := &fakeExtractor{ex: extract.Extraction{Amount: decimal.RequireFromString("50"), Currency: "CHF"}}
	if out, err := newPipeline(s, e, nil).Process(context.Background(), 1); out != pipeline.OutcomeWritten || err != nil {
		t.Fatalf("Process = %s, %v", out, err)
	}
	if s.sets[0].value != "CHF50.00" {
		t.Errorf("value = %q", s.sets[0].value)
	}
}

func TestProcessSkipWhenFieldSet(t *testing.T) {
	s := &fakeStore{doc: doc(cf(3, `"x"`), cf(fieldID, `"EUR10.00"`))}
	e := &fakeExtractor{ex: goodExtraction()}
	out, err := newPipeline(s, e, nil).Process(context.Background(), 1)
	if out != pipeline.OutcomeSkipped || err != nil {
		t.Fatalf("Process = %s, %v", out, err)
	}
	if e.calls() != 0 {
		t.Errorf("extractor called")
	}
	if len(s.sets) != 0 {
		t.Errorf("written: %+v", s.sets)
	}
}

func TestProcessEmptyFieldNotSkipped(t *testing.T) {
	for _, raw := range []string{"", "null", `""`} {
		s := &fakeStore{doc: doc(cf(fieldID, raw))}
		e := &fakeExtractor{ex: goodExtraction()}
		out, err := newPipeline(s, e, nil).Process(context.Background(), 1)
		if out != pipeline.OutcomeWritten || err != nil {
			t.Errorf("value %q: Process = %s, %v", raw, out, err)
		}
	}
}

func TestProcessOtherFieldSetNotSkipped(t *testing.T) {
	s := &fakeStore{doc: doc(cf(8, `"EUR10.00"`))}
	e := &fakeExtractor{ex: goodExtraction()}
	if out, _ := newPipeline(s, e, nil).Process(context.Background(), 1); out != pipeline.OutcomeWritten {
		t.Errorf("out = %s", out)
	}
}

func TestProcessOverwrite(t *testing.T) {
	s := &fakeStore{doc: doc(cf(fieldID, `"EUR10.00"`))}
	e := &fakeExtractor{ex: goodExtraction()}
	p := newPipeline(s, e, nil)
	p.Overwrite = true
	out, err := p.Process(context.Background(), 5)
	if out != pipeline.OutcomeWritten || err != nil {
		t.Fatalf("Process = %s, %v", out, err)
	}
	if e.calls() != 1 || len(s.sets) != 1 || s.sets[0].value != "EUR1234.56" {
		t.Errorf("calls=%d sets=%+v", e.calls(), s.sets)
	}
}

func TestProcessRejected(t *testing.T) {
	errs := []error{
		fmt.Errorf("%w: %w: detail", extract.ErrRejected, extract.ErrNotInText),
		fmt.Errorf("%w: %w", extract.ErrRejected, extract.ErrNotFound),
		fmt.Errorf("%w: %w", extract.ErrRejected, extract.ErrContextOverflow),
		extract.ErrEmptyText,
		fmt.Errorf("wrapped: %w", extract.ErrEmptyText),
	}
	for _, xerr := range errs {
		s := &fakeStore{doc: doc()}
		e := &fakeExtractor{err: xerr}
		out, err := newPipeline(s, e, nil).Process(context.Background(), 1)
		if out != pipeline.OutcomeRejected || !errors.Is(err, xerr) {
			t.Errorf("%v: Process = %s, %v", xerr, out, err)
		}
		if s.setTries != 0 {
			t.Errorf("%v: SetCustomField called", xerr)
		}
	}
}

func TestProcessExtractorFailed(t *testing.T) {
	errs := []error{
		extract.ErrMalformedResponse,
		extract.ErrBudgetTooSmall,
		errors.New("connection refused"),
		context.DeadlineExceeded,
	}
	for _, xerr := range errs {
		s := &fakeStore{doc: doc()}
		e := &fakeExtractor{err: xerr}
		out, err := newPipeline(s, e, nil).Process(context.Background(), 1)
		if out != pipeline.OutcomeFailed || !errors.Is(err, xerr) {
			t.Errorf("%v: Process = %s, %v", xerr, out, err)
		}
		if s.setTries != 0 {
			t.Errorf("%v: SetCustomField called", xerr)
		}
		if e.calls() != 1 {
			t.Errorf("%v: extractor called %d times (pipeline must not retry extraction)", xerr, e.calls())
		}
	}
}

type statusErr int

func (s statusErr) Error() string   { return fmt.Sprintf("status %d", int(s)) }
func (s statusErr) HTTPStatus() int { return int(s) }

func TestProcessGetTransientRetried(t *testing.T) {
	s := &fakeStore{doc: doc(), getErrs: []error{statusErr(503), &paperless.StatusError{StatusCode: 502}}}
	e := &fakeExtractor{ex: goodExtraction()}
	out, err := newPipeline(s, e, nil).Process(context.Background(), 1)
	if out != pipeline.OutcomeWritten || err != nil {
		t.Fatalf("Process = %s, %v", out, err)
	}
	if s.gets != 3 {
		t.Errorf("gets = %d, want 3", s.gets)
	}
}

func TestProcessGetTransientExhausted(t *testing.T) {
	s := &fakeStore{doc: doc(), getErrs: []error{statusErr(503), statusErr(503), statusErr(503), statusErr(503)}}
	e := &fakeExtractor{ex: goodExtraction()}
	out, err := newPipeline(s, e, nil).Process(context.Background(), 1)
	if out != pipeline.OutcomeFailed || err == nil {
		t.Fatalf("Process = %s, %v", out, err)
	}
	if s.gets != 3 || e.calls() != 0 {
		t.Errorf("gets=%d extractor calls=%d", s.gets, e.calls())
	}
}

func TestProcessGetNotFoundNotRetried(t *testing.T) {
	nf := &paperless.StatusError{Method: "GET", URL: "http://p/api/documents/1/", StatusCode: 404}
	s := &fakeStore{doc: doc(), getErrs: []error{nf}}
	e := &fakeExtractor{ex: goodExtraction()}
	out, err := newPipeline(s, e, nil).Process(context.Background(), 1)
	if out != pipeline.OutcomeFailed || !errors.Is(err, paperless.ErrNotFound) {
		t.Fatalf("Process = %s, %v", out, err)
	}
	if s.gets != 1 || e.calls() != 0 || s.setTries != 0 {
		t.Errorf("gets=%d extract=%d sets=%d", s.gets, e.calls(), s.setTries)
	}
}

func TestProcessSetRetriedAndFailed(t *testing.T) {
	s := &fakeStore{doc: doc(), setErrs: []error{statusErr(500)}}
	e := &fakeExtractor{ex: goodExtraction()}
	if out, err := newPipeline(s, e, nil).Process(context.Background(), 1); out != pipeline.OutcomeWritten || err != nil {
		t.Fatalf("Process = %s, %v", out, err)
	}
	if s.setTries != 2 || e.calls() != 1 {
		t.Errorf("setTries=%d extract=%d", s.setTries, e.calls())
	}

	s2 := &fakeStore{doc: doc(), setErrs: []error{statusErr(400)}}
	out, err := newPipeline(s2, &fakeExtractor{ex: goodExtraction()}, nil).Process(context.Background(), 1)
	if out != pipeline.OutcomeFailed || err == nil {
		t.Errorf("Process = %s, %v", out, err)
	}
	if s2.setTries != 1 {
		t.Errorf("non-transient set retried: %d", s2.setTries)
	}
}

func TestHandleLogsOutcome(t *testing.T) {
	cases := []struct {
		name  string
		store *fakeStore
		ext   *fakeExtractor
		level string
		want  string
	}{
		{"written", &fakeStore{doc: doc()}, &fakeExtractor{ex: goodExtraction()}, "INFO", "written"},
		{"skipped", &fakeStore{doc: doc(cf(fieldID, `"EUR1.00"`))}, &fakeExtractor{}, "INFO", "skipped"},
		{"rejected", &fakeStore{doc: doc()}, &fakeExtractor{err: fmt.Errorf("%w: %w", extract.ErrRejected, extract.ErrNotFound)}, "INFO", "rejected"},
		{"failed", &fakeStore{doc: doc()}, &fakeExtractor{err: extract.ErrMalformedResponse}, "ERROR", "failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
			newPipeline(c.store, c.ext, logger).Handle(context.Background(), 42)
			var found bool
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				var m map[string]any
				if json.Unmarshal([]byte(line), &m) != nil {
					continue
				}
				if m["outcome"] == c.want {
					found = true
					if m["level"] != c.level {
						t.Errorf("level = %v, want %s", m["level"], c.level)
					}
					if m["doc_id"] != float64(42) {
						t.Errorf("doc_id = %v", m["doc_id"])
					}
				}
			}
			if !found {
				t.Errorf("no log line with outcome=%s:\n%s", c.want, buf.String())
			}
			// amount and document content only at debug
			if strings.Contains(buf.String(), "1234.56") || strings.Contains(buf.String(), "Gesamt 1.234,56") {
				t.Errorf("amount/content logged at info:\n%s", buf.String())
			}
		})
	}
}
