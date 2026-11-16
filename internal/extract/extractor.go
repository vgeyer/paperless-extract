package extract

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vgeyer/paperless-extract/internal/ollama"
	"github.com/vgeyer/paperless-extract/internal/retry"
)

// ChatClient is satisfied by *ollama.Client.
type ChatClient interface {
	Chat(ctx context.Context, req ollama.ChatRequest) (*ollama.ChatResponse, error)
}

// Extractor runs one extraction on document text.
type Extractor struct {
	Chat       ChatClient
	Prompt     *Prompt
	Model      string
	NumCtx     int
	NumPredict int
	KeepAlive  time.Duration
	Validator  Validator
	Retry      retry.Policy
	Logger     *slog.Logger
}

func (e *Extractor) log() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// Extract runs one extraction on the document text.
func (e *Extractor) Extract(ctx context.Context, text string) (Extraction, error) {
	if strings.TrimSpace(text) == "" {
		return Extraction{}, ErrEmptyText
	}
	log := e.log()
	system, err := e.Prompt.System(PromptData{DefaultCurrency: e.Validator.DefaultCurrency})
	if err != nil {
		return Extraction{}, err
	}
	budget := TextBudget(e.NumCtx, e.NumPredict, system)
	if budget < MinTextBudget {
		return Extraction{}, fmt.Errorf("%w: %d tokens", ErrBudgetTooSmall, budget)
	}
	fitted, truncated := FitText(text, budget)
	if truncated {
		log.Warn("document truncated", "chars", utf8.RuneCountInString(text), "kept_chars", utf8.RuneCountInString(fitted))
	}
	log.Debug("prompt", "system", system, "text", fitted)

	req := ollama.ChatRequest{
		Model: e.Model,
		Messages: []ollama.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: fitted},
		},
		Format:    ResultSchema(),
		Options:   ollama.Options{NumCtx: e.NumCtx, Temperature: 0, NumPredict: e.NumPredict},
		KeepAlive: e.KeepAlive.String(),
	}

	var (
		resp   *ollama.ChatResponse
		result Result
	)
	for round := 1; ; round++ {
		attempt := 0
		err := retry.Do(ctx, e.Retry, retry.IsTransient, func(ctx context.Context) error {
			attempt++
			if attempt > 1 {
				log.Warn("retrying", "attempt", attempt, "model", e.Model)
			}
			var cerr error
			resp, cerr = e.Chat.Chat(ctx, req)
			if cerr != nil {
				log.Warn("ollama call failed", "attempt", attempt, "error", cerr.Error())
			}
			return cerr
		})
		if err != nil {
			return Extraction{}, fmt.Errorf("ollama chat: %w", err)
		}
		log.Debug("model output", "content", resp.Message.Content, "done_reason", resp.DoneReason)
		result, err = ParseResult(resp.Message.Content)
		if err == nil && resp.DoneReason != "length" {
			break
		}
		if round >= 2 {
			if err == nil {
				err = errors.New("output cut off at num_predict")
			}
			return Extraction{}, fmt.Errorf("%w: %s", ErrMalformedResponse, err.Error())
		}
		log.Warn("malformed model response, retrying once", "done_reason", resp.DoneReason)
	}

	if NearContextLimit(resp.PromptEvalCount, e.NumCtx) {
		log.Warn("prompt at context limit", "prompt_eval_count", resp.PromptEvalCount, "num_ctx", e.NumCtx)
		return Extraction{}, reject(ErrContextOverflow, fmt.Sprintf("prompt_eval_count %d of %d", resp.PromptEvalCount, e.NumCtx))
	}

	ex, err := e.Validator.Validate(result, text)
	if err != nil {
		return Extraction{}, err
	}
	ex.Truncated = truncated
	ex.PromptEvalCount = resp.PromptEvalCount
	return ex, nil
}
