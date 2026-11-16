// Package pipeline ties Paperless and the extractor together.
package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/vgeyer/paperless-extract/internal/extract"
	"github.com/vgeyer/paperless-extract/internal/paperless"
	"github.com/vgeyer/paperless-extract/internal/retry"
)

// DocumentStore is satisfied by *paperless.Client.
type DocumentStore interface {
	GetDocument(ctx context.Context, id int) (*paperless.Document, error)
	SetCustomField(ctx context.Context, docID, fieldID int, value string) error
}

// AmountExtractor is satisfied by *extract.Extractor.
type AmountExtractor interface {
	Extract(ctx context.Context, text string) (extract.Extraction, error)
}

// Outcome is the result class of processing one document.
type Outcome string

const (
	OutcomeWritten  Outcome = "written"
	OutcomeSkipped  Outcome = "skipped"
	OutcomeRejected Outcome = "rejected"
	OutcomeFailed   Outcome = "failed"
)

// Pipeline processes documents.
type Pipeline struct {
	Store     DocumentStore
	Extractor AmountExtractor
	FieldID   int
	Overwrite bool
	Retry     retry.Policy
	Logger    *slog.Logger
}

func (p *Pipeline) log() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// Process handles one document.
func (p *Pipeline) Process(ctx context.Context, docID int) (Outcome, error) {
	return p.process(ctx, docID, nil)
}

func (p *Pipeline) process(ctx context.Context, docID int, written *extract.Extraction) (Outcome, error) {
	log := p.log().With("doc_id", docID)
	retryLog := func(op string) func(ctx context.Context, fn func(context.Context) error) error {
		return func(ctx context.Context, fn func(context.Context) error) error {
			attempt := 0
			return retry.Do(ctx, p.Retry, retry.IsTransient, func(ctx context.Context) error {
				attempt++
				err := fn(ctx)
				if err != nil && retry.IsTransient(err) {
					log.Warn("retrying", "op", op, "attempt", attempt, "error", err.Error())
				}
				return err
			})
		}
	}

	var doc *paperless.Document
	err := retryLog("get_document")(ctx, func(ctx context.Context) error {
		var gerr error
		doc, gerr = p.Store.GetDocument(ctx, docID)
		return gerr
	})
	if err != nil {
		return OutcomeFailed, err
	}

	if !p.Overwrite {
		for _, cf := range doc.CustomFields {
			if cf.Field == p.FieldID && !cf.IsEmpty() {
				return OutcomeSkipped, nil
			}
		}
	}

	ex, err := p.Extractor.Extract(ctx, doc.Content)
	if err != nil {
		if errors.Is(err, extract.ErrRejected) || errors.Is(err, extract.ErrEmptyText) {
			return OutcomeRejected, err
		}
		return OutcomeFailed, err
	}

	value := paperless.FormatMonetary(ex.Currency, ex.Amount)
	err = retryLog("set_custom_field")(ctx, func(ctx context.Context) error {
		return p.Store.SetCustomField(ctx, docID, p.FieldID, value)
	})
	if err != nil {
		return OutcomeFailed, err
	}
	if written != nil {
		*written = ex
	}
	return OutcomeWritten, nil
}

// Handle adapts Process to queue.Handler and logs the outcome.
func (p *Pipeline) Handle(ctx context.Context, docID int) {
	start := time.Now()
	var ex extract.Extraction
	outcome, err := p.process(ctx, docID, &ex)
	log := p.log()
	ms := time.Since(start).Milliseconds()
	switch outcome {
	case OutcomeWritten:
		log.Info("document processed", "doc_id", docID, "outcome", outcome, "currency", ex.Currency, "duration_ms", ms)
		log.Debug("extracted amount", "doc_id", docID, "amount", ex.Amount.String(), "amount_raw", ex.AmountRaw, "evidence", ex.Evidence)
	case OutcomeSkipped:
		log.Info("document processed", "doc_id", docID, "outcome", outcome, "reason", "field already set", "duration_ms", ms)
	case OutcomeRejected:
		log.Info("document processed", "doc_id", docID, "outcome", outcome, "reason", err.Error(), "duration_ms", ms)
	default:
		var se *paperless.StatusError
		if errors.As(err, &se) && se.Body != "" {
			log.Debug("paperless error body", "doc_id", docID, "body", se.Body)
		}
		log.Error("document processing failed", "doc_id", docID, "outcome", outcome, "error", err.Error(), "duration_ms", ms)
	}
}
