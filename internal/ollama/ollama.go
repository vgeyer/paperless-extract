// Package ollama is a minimal Ollama client for /api/chat and /api/tags.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ErrModelNotFound is returned by CheckModel.
var ErrModelNotFound = errors.New("ollama: model not available")

const (
	maxChatBytes = 4 << 20
	maxErrBody   = 512
)

// StatusError is returned for non-2xx responses.
type StatusError struct {
	StatusCode int
	Message    string
}

// Error implements error.
func (e *StatusError) Error() string {
	return fmt.Sprintf("ollama: status %d: %s", e.StatusCode, e.Message)
}

// HTTPStatus returns the status code; used by retry.IsTransient.
func (e *StatusError) HTTPStatus() int { return e.StatusCode }

// Message is one chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Options are the model options.
type Options struct {
	NumCtx      int     `json:"num_ctx"`
	Temperature float64 `json:"temperature"`
	NumPredict  int     `json:"num_predict"`
}

// ChatRequest is the /api/chat request.
type ChatRequest struct {
	Model     string          `json:"model"`
	Messages  []Message       `json:"messages"`
	Stream    bool            `json:"stream"`
	Format    json.RawMessage `json:"format,omitempty"`
	Options   Options         `json:"options"`
	KeepAlive string          `json:"keep_alive,omitempty"`
}

// ChatResponse is the /api/chat response.
type ChatResponse struct {
	Model           string  `json:"model"`
	Message         Message `json:"message"`
	Done            bool    `json:"done"`
	DoneReason      string  `json:"done_reason"`
	PromptEvalCount int     `json:"prompt_eval_count"`
	EvalCount       int     `json:"eval_count"`
	TotalDuration   int64   `json:"total_duration"`
}

// Client talks to Ollama.
type Client struct {
	base    string
	timeout time.Duration
	hc      *http.Client
}

// NewClient creates a client; timeout applies per Chat call (0 = none).
func NewClient(baseURL string, timeout time.Duration, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), timeout: timeout, hc: httpClient}
}

func statusError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	msg := strings.TrimSpace(string(b))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	return &StatusError{StatusCode: resp.StatusCode, Message: msg}
}

// Chat sends a non-streaming chat request.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, statusError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxChatBytes))
	if err != nil {
		return nil, err
	}
	var out ChatResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("ollama: decode response: %w", err)
	}
	return &out, nil
}

// CheckModel verifies that model is available on the server.
func (c *Client) CheckModel(ctx context.Context, model string) error {
	hr, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/tags", nil)
	if err != nil {
		return err
	}
	hr.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(hr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return statusError(resp)
	}
	var tags struct {
		Models []struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxChatBytes)).Decode(&tags); err != nil {
		return fmt.Errorf("ollama: decode tags: %w", err)
	}
	want := model
	if !strings.Contains(want, ":") {
		want += ":latest"
	}
	for _, m := range tags.Models {
		if m.Name == want || m.Model == want || m.Name == model || m.Model == model {
			return nil
		}
	}
	return fmt.Errorf("%w: %q", ErrModelNotFound, model)
}
