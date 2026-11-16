package extract_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vgeyer/paperless-extract/internal/extract"
	"github.com/vgeyer/paperless-extract/internal/ollama"
	"github.com/vgeyer/paperless-extract/internal/retry"
)

type reply struct {
	resp *ollama.ChatResponse
	err  error
}

type fakeChat struct {
	mu      sync.Mutex
	reqs    []ollama.ChatRequest
	replies []reply
}

func (f *fakeChat) Chat(ctx context.Context, req ollama.ChatRequest) (*ollama.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	i := len(f.reqs) - 1
	if i >= len(f.replies) {
		i = len(f.replies) - 1
	}
	r := f.replies[i]
	return r.resp, r.err
}

func (f *fakeChat) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func ok(content string) reply {
	return reply{resp: &ollama.ChatResponse{
		Model: "m", Message: ollama.Message{Role: "assistant", Content: content},
		Done: true, DoneReason: "stop", PromptEvalCount: 500, EvalCount: 30,
	}}
}

func withReason(r reply, reason string) reply {
	c := *r.resp
	c.DoneReason = reason
	return reply{resp: &c}
}

func withPromptEval(r reply, n int) reply {
	c := *r.resp
	c.PromptEvalCount = n
	return reply{resp: &c}
}

const goodJSON = `{"found":true,"amount_raw":"1.234,56","currency":"€","evidence":"Gesamtbetrag 1.234,56 €"}`
const docText = "ACME GmbH\nRechnung 4711\nNetto 1.037,44 €\nMwSt 197,12 €\nGesamtbetrag 1.234,56 €\n"

func newExtractor(t *testing.T, chat extract.ChatClient) *extract.Extractor {
	t.Helper()
	p, err := extract.LoadPrompt("")
	if err != nil {
		t.Fatal(err)
	}
	return &extract.Extractor{
		Chat:       chat,
		Prompt:     p,
		Model:      "qwen2.5:14b",
		NumCtx:     8192,
		NumPredict: 256,
		KeepAlive:  5 * time.Minute,
		Validator:  extract.Validator{MaxAmount: decimal.NewFromInt(1000000), DefaultCurrency: "EUR"},
		Retry:      retry.Policy{Attempts: 3, BaseDelay: time.Millisecond},
	}
}

func TestExtractSuccessAndRequest(t *testing.T) {
	fc := &fakeChat{replies: []reply{ok(goodJSON)}}
	e := newExtractor(t, fc)
	ex, err := e.Extract(context.Background(), docText)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !ex.Amount.Equal(decimal.RequireFromString("1234.56")) || ex.Currency != "EUR" {
		t.Errorf("extraction = %+v", ex)
	}
	if ex.AmountRaw != "1.234,56" || ex.Evidence != "Gesamtbetrag 1.234,56 €" || ex.Truncated || ex.PromptEvalCount != 500 {
		t.Errorf("extraction = %+v", ex)
	}
	if fc.calls() != 1 {
		t.Fatalf("calls = %d", fc.calls())
	}
	req := fc.reqs[0]
	if req.Model != "qwen2.5:14b" || req.Stream {
		t.Errorf("model/stream = %q/%v", req.Model, req.Stream)
	}
	sys, _ := e.Prompt.System(extract.PromptData{DefaultCurrency: "EUR"})
	want := []ollama.Message{{Role: "system", Content: sys}, {Role: "user", Content: docText}}
	if !reflect.DeepEqual(req.Messages, want) {
		t.Errorf("messages = %+v", req.Messages)
	}
	var gotF, wantF any
	if err := json.Unmarshal(req.Format, &gotF); err != nil {
		t.Fatalf("format not JSON: %v", err)
	}
	_ = json.Unmarshal(extract.ResultSchema(), &wantF)
	if !reflect.DeepEqual(gotF, wantF) {
		t.Errorf("format = %s", req.Format)
	}
	if req.Options != (ollama.Options{NumCtx: 8192, Temperature: 0, NumPredict: 256}) {
		t.Errorf("options = %+v", req.Options)
	}
	if req.KeepAlive != "5m0s" {
		t.Errorf("keep_alive = %q", req.KeepAlive)
	}
}

func TestExtractKeepAliveNegative(t *testing.T) {
	fc := &fakeChat{replies: []reply{ok(goodJSON)}}
	e := newExtractor(t, fc)
	e.KeepAlive = -time.Second
	if _, err := e.Extract(context.Background(), docText); err != nil {
		t.Fatal(err)
	}
	if fc.reqs[0].KeepAlive != "-1s" {
		t.Errorf("keep_alive = %q", fc.reqs[0].KeepAlive)
	}
}

func TestExtractEmptyText(t *testing.T) {
	fc := &fakeChat{replies: []reply{ok(goodJSON)}}
	for _, txt := range []string{"", "  \n\t "} {
		_, err := newExtractor(t, fc).Extract(context.Background(), txt)
		if !errors.Is(err, extract.ErrEmptyText) {
			t.Errorf("Extract(%q) = %v", txt, err)
		}
	}
	if fc.calls() != 0 {
		t.Errorf("chat called for empty text")
	}
}

func TestExtractBudgetTooSmall(t *testing.T) {
	fc := &fakeChat{replies: []reply{ok(goodJSON)}}
	e := newExtractor(t, fc)
	e.NumCtx = 1024
	e.NumPredict = 1000
	if _, err := e.Extract(context.Background(), docText); !errors.Is(err, extract.ErrBudgetTooSmall) {
		t.Errorf("err = %v", err)
	}
	// budget exactly MinTextBudget-1
	sys, _ := e.Prompt.System(extract.PromptData{DefaultCurrency: "EUR"})
	e.NumPredict = 100
	e.NumCtx = extract.MinTextBudget - 1 + extract.EstimateTokens(sys) + e.NumPredict + extract.ChatOverheadTokens
	if _, err := e.Extract(context.Background(), docText); !errors.Is(err, extract.ErrBudgetTooSmall) {
		t.Errorf("budget 255: err = %v", err)
	}
	if fc.calls() != 0 {
		t.Errorf("chat called")
	}
	e.NumCtx++ // budget == MinTextBudget is allowed
	if _, err := e.Extract(context.Background(), docText); err != nil {
		t.Errorf("budget 256: err = %v", err)
	}
}

func TestExtractRetriesMalformedOnce(t *testing.T) {
	fc := &fakeChat{replies: []reply{ok("not json"), ok(goodJSON)}}
	ex, err := newExtractor(t, fc).Extract(context.Background(), docText)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !ex.Amount.Equal(decimal.RequireFromString("1234.56")) {
		t.Errorf("amount = %s", ex.Amount)
	}
	if fc.calls() != 2 {
		t.Fatalf("calls = %d, want 2", fc.calls())
	}
	if !reflect.DeepEqual(fc.reqs[0], fc.reqs[1]) {
		t.Errorf("retry request differs from original")
	}
}

func TestExtractMalformedTwice(t *testing.T) {
	fc := &fakeChat{replies: []reply{ok(`{"amount_raw":"1"}`), ok("```json\n{oops\n```"), ok(goodJSON)}}
	_, err := newExtractor(t, fc).Extract(context.Background(), docText)
	if !errors.Is(err, extract.ErrMalformedResponse) {
		t.Errorf("err = %v", err)
	}
	if errors.Is(err, extract.ErrRejected) {
		t.Errorf("malformed must not be a rejection")
	}
	if fc.calls() != 2 {
		t.Errorf("calls = %d, want 2", fc.calls())
	}
}

func TestExtractDoneReasonLengthRetried(t *testing.T) {
	fc := &fakeChat{replies: []reply{withReason(ok(goodJSON), "length"), ok(goodJSON)}}
	if _, err := newExtractor(t, fc).Extract(context.Background(), docText); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if fc.calls() != 2 {
		t.Errorf("calls = %d, want 2", fc.calls())
	}

	fc2 := &fakeChat{replies: []reply{withReason(ok(goodJSON), "length")}}
	_, err := newExtractor(t, fc2).Extract(context.Background(), docText)
	if !errors.Is(err, extract.ErrMalformedResponse) {
		t.Errorf("length twice: err = %v", err)
	}
	if fc2.calls() != 2 {
		t.Errorf("calls = %d, want 2", fc2.calls())
	}
}

func TestExtractNoRetryOnValidationFailure(t *testing.T) {
	cases := map[string]struct {
		content string
		want    error
	}{
		"not in text": {`{"found":true,"amount_raw":"9.999,99","currency":"EUR"}`, extract.ErrNotInText},
		"not found":   {`{"found":false}`, extract.ErrNotFound},
		"negative":    {`{"found":true,"amount_raw":"-197,12"}`, extract.ErrNotPositive},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			text := docText + "Storno -197,12\n"
			fc := &fakeChat{replies: []reply{ok(c.content), ok(goodJSON)}}
			_, err := newExtractor(t, fc).Extract(context.Background(), text)
			if !errors.Is(err, extract.ErrRejected) || !errors.Is(err, c.want) {
				t.Errorf("err = %v, want ErrRejected+%v", err, c.want)
			}
			if fc.calls() != 1 {
				t.Errorf("calls = %d, want 1", fc.calls())
			}
		})
	}
}

func TestExtractRetriesTransientError(t *testing.T) {
	fc := &fakeChat{replies: []reply{
		{err: &ollama.StatusError{StatusCode: 503, Message: "busy"}},
		{err: &ollama.StatusError{StatusCode: 500, Message: "oops"}},
		ok(goodJSON),
	}}
	if _, err := newExtractor(t, fc).Extract(context.Background(), docText); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if fc.calls() != 3 {
		t.Errorf("calls = %d, want 3", fc.calls())
	}
}

func TestExtractTransientExhausted(t *testing.T) {
	fc := &fakeChat{replies: []reply{{err: &ollama.StatusError{StatusCode: 503, Message: "busy"}}}}
	_, err := newExtractor(t, fc).Extract(context.Background(), docText)
	var se *ollama.StatusError
	if !errors.As(err, &se) || se.StatusCode != 503 {
		t.Errorf("err = %v", err)
	}
	if errors.Is(err, extract.ErrRejected) || errors.Is(err, extract.ErrMalformedResponse) {
		t.Errorf("transport error misclassified: %v", err)
	}
	if fc.calls() != 3 {
		t.Errorf("calls = %d, want 3", fc.calls())
	}
}

func TestExtractNoRetryOnNonTransient(t *testing.T) {
	fc := &fakeChat{replies: []reply{{err: &ollama.StatusError{StatusCode: 404, Message: "model not found"}}, ok(goodJSON)}}
	_, err := newExtractor(t, fc).Extract(context.Background(), docText)
	var se *ollama.StatusError
	if !errors.As(err, &se) || se.StatusCode != 404 {
		t.Errorf("err = %v", err)
	}
	if fc.calls() != 1 {
		t.Errorf("calls = %d, want 1", fc.calls())
	}
}

func TestExtractZeroRetryPolicySingleAttempt(t *testing.T) {
	fc := &fakeChat{replies: []reply{{err: &ollama.StatusError{StatusCode: 503}}, ok(goodJSON)}}
	e := newExtractor(t, fc)
	e.Retry = retry.Policy{}
	if _, err := e.Extract(context.Background(), docText); err == nil {
		t.Error("expected error")
	}
	if fc.calls() != 1 {
		t.Errorf("calls = %d, want 1", fc.calls())
	}
}

func TestExtractContextOverflow(t *testing.T) {
	fc := &fakeChat{replies: []reply{withPromptEval(ok(goodJSON), 8192*95/100)}}
	_, err := newExtractor(t, fc).Extract(context.Background(), docText)
	if !errors.Is(err, extract.ErrRejected) || !errors.Is(err, extract.ErrContextOverflow) {
		t.Errorf("err = %v", err)
	}
	if fc.calls() != 1 {
		t.Errorf("calls = %d", fc.calls())
	}
	fc2 := &fakeChat{replies: []reply{withPromptEval(ok(goodJSON), 8192*95/100-1)}}
	if _, err := newExtractor(t, fc2).Extract(context.Background(), docText); err != nil {
		t.Errorf("just below limit: %v", err)
	}
}

func TestExtractTruncationValidatesAgainstFullText(t *testing.T) {
	fc := &fakeChat{}
	e := newExtractor(t, fc)
	sys, _ := e.Prompt.System(extract.PromptData{DefaultCurrency: "EUR"})
	e.NumPredict = 64
	const budget = 400
	e.NumCtx = budget + extract.EstimateTokens(sys) + e.NumPredict + extract.ChatOverheadTokens
	// The amount sits in the middle and is cut out by FitText; validation must
	// still use the full text.
	filler := strings.Repeat("Allgemeine Geschäftsbedingungen. ", 200)
	text := "ACME GmbH\n" + filler + "\nZwischensumme 5.432,10 EUR\n" + filler + "\nEnde"
	fitted, truncated := extract.FitText(text, budget)
	if !truncated || strings.Contains(fitted, "5.432,10") {
		t.Fatalf("test setup: amount should be truncated away")
	}
	fc.replies = []reply{ok(`{"found":true,"amount_raw":"5.432,10","currency":"EUR"}`)}
	ex, err := e.Extract(context.Background(), text)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !ex.Truncated {
		t.Error("Truncated = false")
	}
	if !ex.Amount.Equal(decimal.RequireFromString("5432.10")) {
		t.Errorf("amount = %s", ex.Amount)
	}
	if got := fc.reqs[0].Messages[1].Content; got != fitted {
		t.Errorf("user message is not FitText result (len %d vs %d)", len(got), len(fitted))
	}
}

func TestExtractDefaultCurrencyInPrompt(t *testing.T) {
	fc := &fakeChat{replies: []reply{ok(`{"found":true,"amount_raw":"1.234,56"}`)}}
	e := newExtractor(t, fc)
	e.Validator.DefaultCurrency = "PLN"
	ex, err := e.Extract(context.Background(), docText)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fc.reqs[0].Messages[0].Content, "PLN will then be assumed") {
		t.Errorf("system prompt does not mention default currency")
	}
	if ex.Currency != "PLN" {
		t.Errorf("Currency = %q", ex.Currency)
	}
}

func TestExtractContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fc := &fakeChat{replies: []reply{{err: context.Canceled}}}
	_, err := newExtractor(t, fc).Extract(ctx, docText)
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, extract.ErrRejected) {
		t.Errorf("cancel must not be a rejection: %v", err)
	}
}
