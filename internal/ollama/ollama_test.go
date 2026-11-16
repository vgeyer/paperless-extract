package ollama_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vgeyer/paperless-extract/internal/ollama"
)

const schema = `{"type":"object","properties":{"found":{"type":"boolean"},"amount_raw":{"type":"string","description":"Total exactly as printed"},"currency":{"type":"string","description":"ISO 4217, e.g. EUR"},"evidence":{"type":"string","description":"The line the amount was taken from"}},"required":["found"]}`

const chatResp = `{"model":"qwen2.5:14b","created_at":"2024-01-01T00:00:00Z","message":{"role":"assistant","content":"{\"found\":true,\"amount_raw\":\"1.234,56\",\"currency\":\"€\",\"evidence\":\"Gesamt 1.234,56 €\"}"},
 "done":true,"done_reason":"stop","prompt_eval_count":1834,"eval_count":41,"total_duration":5123456789,"load_duration":1}`

type capture struct {
	mu     sync.Mutex
	method string
	path   string
	header http.Header
	body   []byte
}

func server(t *testing.T, status int, resp string) (*capture, *httptest.Server) {
	c := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.method, c.path, c.header, c.body = r.Method, r.URL.Path, r.Header.Clone(), b
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return c, srv
}

func sampleRequest() ollama.ChatRequest {
	return ollama.ChatRequest{
		Model: "qwen2.5:14b",
		Messages: []ollama.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "Gesamt 1.234,56 €"},
		},
		Stream:    true, // must be forced to false
		Format:    json.RawMessage(schema),
		Options:   ollama.Options{NumCtx: 65536, Temperature: 0, NumPredict: 256},
		KeepAlive: (5 * time.Minute).String(),
	}
}

func TestChatRequestShape(t *testing.T) {
	c, srv := server(t, 200, chatResp)
	cl := ollama.NewClient(srv.URL+"/", 0, nil)
	resp, err := cl.Chat(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.method != http.MethodPost || c.path != "/api/chat" {
		t.Errorf("request = %s %s", c.method, c.path)
	}
	if ct := c.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	var got map[string]any
	if err := json.Unmarshal(c.body, &got); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	var want map[string]any
	_ = json.Unmarshal([]byte(`{"model":"qwen2.5:14b",
		"messages":[{"role":"system","content":"sys"},{"role":"user","content":"Gesamt 1.234,56 €"}],
		"stream":false,
		"format":`+schema+`,
		"options":{"num_ctx":65536,"temperature":0,"num_predict":256},
		"keep_alive":"5m0s"}`), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("request body mismatch\n got: %s", c.body)
	}
	// explicit presence checks
	if v, ok := got["stream"]; !ok || v != false {
		t.Errorf("stream = %v (present %v)", v, ok)
	}
	opts, _ := got["options"].(map[string]any)
	if v, ok := opts["temperature"]; !ok || v != float64(0) {
		t.Errorf("temperature missing or wrong: %v", opts)
	}

	if resp.Model != "qwen2.5:14b" || !resp.Done || resp.DoneReason != "stop" ||
		resp.PromptEvalCount != 1834 || resp.EvalCount != 41 || resp.TotalDuration != 5123456789 {
		t.Errorf("resp = %+v", resp)
	}
	if resp.Message.Role != "assistant" || !strings.Contains(resp.Message.Content, `"amount_raw":"1.234,56"`) {
		t.Errorf("message = %+v", resp.Message)
	}
}

func TestChatOmitsEmptyFormatAndKeepAlive(t *testing.T) {
	c, srv := server(t, 200, chatResp)
	req := sampleRequest()
	req.Format = nil
	req.KeepAlive = ""
	if _, err := ollama.NewClient(srv.URL, 0, nil).Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(c.body, &got)
	if _, ok := got["format"]; ok {
		t.Errorf("format present: %s", c.body)
	}
	if _, ok := got["keep_alive"]; ok {
		t.Errorf("keep_alive present: %s", c.body)
	}
}

func TestChatStatusError(t *testing.T) {
	_, srv := server(t, 404, `{"error":"model \"x\" not found, try pulling it first"}`)
	_, err := ollama.NewClient(srv.URL, 0, nil).Chat(context.Background(), sampleRequest())
	var se *ollama.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %T %v", err, err)
	}
	if se.StatusCode != 404 || se.HTTPStatus() != 404 {
		t.Errorf("status = %d", se.StatusCode)
	}
	if se.Message != `model "x" not found, try pulling it first` {
		t.Errorf("Message = %q", se.Message)
	}
	if !strings.Contains(se.Error(), "not found") {
		t.Errorf("Error() = %q", se.Error())
	}
}

func TestChatStatusErrorNonJSONBody(t *testing.T) {
	long := strings.Repeat("e", 2000)
	_, srv := server(t, 500, long)
	_, err := ollama.NewClient(srv.URL, 0, nil).Chat(context.Background(), sampleRequest())
	var se *ollama.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v", err)
	}
	if se.StatusCode != 500 || len(se.Message) != 512 || se.Message != long[:512] {
		t.Errorf("status=%d len(Message)=%d", se.StatusCode, len(se.Message))
	}
}

func TestChatUndecodableBody(t *testing.T) {
	_, srv := server(t, 200, `not json at all`)
	_, err := ollama.NewClient(srv.URL, 0, nil).Chat(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("expected error")
	}
	var se *ollama.StatusError
	if errors.As(err, &se) {
		t.Errorf("undecodable body should not be a StatusError")
	}
}

func TestChatPerCallTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	start := time.Now()
	_, err := ollama.NewClient(srv.URL, 50*time.Millisecond, nil).Chat(context.Background(), sampleRequest())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("timeout not applied, took %v", el)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		var ne interface{ Timeout() bool }
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Errorf("err = %v, want deadline/timeout", err)
		}
	}
}

func TestChatContextCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := ollama.NewClient(srv.URL, 0, nil).Chat(ctx, sampleRequest())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestCheckModel(t *testing.T) {
	tags := `{"models":[{"name":"qwen2.5:14b","model":"qwen2.5:14b"},{"name":"llama3:latest","model":"llama3:latest"},{"name":"alias","model":"mistral:7b"}]}`
	c, srv := server(t, 200, tags)
	cl := ollama.NewClient(srv.URL, 0, nil)
	cases := []struct {
		model string
		ok    bool
	}{
		{"qwen2.5:14b", true},
		{"llama3", true},
		{"llama3:latest", true},
		{"mistral:7b", true}, // matches "model"
		{"qwen2.5", false},   // compared as qwen2.5:latest
		{"llama3:8b", false},
		{"nope", false},
	}
	for _, tc := range cases {
		err := cl.CheckModel(context.Background(), tc.model)
		if tc.ok && err != nil {
			t.Errorf("CheckModel(%q) = %v", tc.model, err)
		}
		if !tc.ok && !errors.Is(err, ollama.ErrModelNotFound) {
			t.Errorf("CheckModel(%q) = %v, want ErrModelNotFound", tc.model, err)
		}
	}
	c.mu.Lock()
	if c.method != http.MethodGet || c.path != "/api/tags" {
		t.Errorf("request = %s %s", c.method, c.path)
	}
	c.mu.Unlock()
}

func TestCheckModelStatusError(t *testing.T) {
	_, srv := server(t, 503, `{"error":"busy"}`)
	err := ollama.NewClient(srv.URL, 0, nil).CheckModel(context.Background(), "x")
	var se *ollama.StatusError
	if !errors.As(err, &se) || se.StatusCode != 503 {
		t.Errorf("err = %v", err)
	}
	if errors.Is(err, ollama.ErrModelNotFound) {
		t.Errorf("status error must not be ErrModelNotFound")
	}
}

func TestCheckModelTransportError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	err := ollama.NewClient(u, 0, nil).CheckModel(context.Background(), "x")
	if err == nil || errors.Is(err, ollama.ErrModelNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestOptionsMarshalTemperatureZero(t *testing.T) {
	b, err := json.Marshal(ollama.Options{NumCtx: 1, NumPredict: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"temperature":0`) {
		t.Errorf("Options JSON = %s", b)
	}
}
