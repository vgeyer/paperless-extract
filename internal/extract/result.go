package extract

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

const resultSchema = `{"type":"object","properties":{"found":{"type":"boolean"},"amount_raw":{"type":"string","description":"Total exactly as printed"},"currency":{"type":"string","description":"ISO 4217, e.g. EUR"},"evidence":{"type":"string","description":"The line the amount was taken from"}},"required":["found"]}`

// ResultSchema returns the JSON schema sent as "format".
func ResultSchema() json.RawMessage { return json.RawMessage(resultSchema) }

// Result is the model's structured answer.
type Result struct {
	Found     bool   `json:"found"`
	AmountRaw string `json:"amount_raw"`
	Currency  string `json:"currency"`
	Evidence  string `json:"evidence"`
}

var (
	ErrMalformedResponse = errors.New("malformed model response")
	ErrEmptyText         = errors.New("document has no text")
	ErrBudgetTooSmall    = errors.New("context budget too small")

	ErrRejected        = errors.New("result rejected")
	ErrNotFound        = errors.New("model found no total")
	ErrUnparseable     = errors.New("amount not parseable")
	ErrNotInText       = errors.New("amount does not occur in document")
	ErrNotPositive     = errors.New("amount not positive")
	ErrTooLarge        = errors.New("amount exceeds limit")
	ErrTooPrecise      = errors.New("more than 2 decimal places")
	ErrCurrency        = errors.New("unknown currency")
	ErrContextOverflow = errors.New("prompt at context limit")
)

func reject(e error, detail string) error {
	if detail == "" {
		return fmt.Errorf("%w: %w", ErrRejected, e)
	}
	return fmt.Errorf("%w: %w: %s", ErrRejected, e, detail)
}

// ParseResult parses the model's message content.
func ParseResult(content string) (Result, error) {
	s := strings.TrimSpace(content)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		} else {
			s = strings.TrimPrefix(s, "json")
		}
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "```"))
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return Result{}, fmt.Errorf("%w: not a JSON object", ErrMalformedResponse)
	}
	var found bool
	f, ok := m["found"]
	if !ok || json.Unmarshal(f, &found) != nil || string(f) == "null" {
		return Result{}, fmt.Errorf("%w: missing or invalid \"found\"", ErrMalformedResponse)
	}
	var r Result
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return Result{}, fmt.Errorf("%w: invalid field types", ErrMalformedResponse)
	}
	return r, nil
}

// Extraction is a validated extraction result.
type Extraction struct {
	Amount          decimal.Decimal
	Currency        string
	AmountRaw       string
	Evidence        string
	Truncated       bool
	PromptEvalCount int
}

// Validator applies the acceptance rules.
type Validator struct {
	MaxAmount       decimal.Decimal
	DefaultCurrency string
}

// Validate applies the rules in order; the first failure is returned.
func (v Validator) Validate(r Result, text string) (Extraction, error) {
	if !r.Found {
		return Extraction{}, reject(ErrNotFound, "")
	}
	amount, err := ParseAmount(r.AmountRaw)
	if err != nil {
		return Extraction{}, reject(ErrUnparseable, "")
	}
	if !AmountInText(r.AmountRaw, text) {
		return Extraction{}, reject(ErrNotInText, "")
	}
	if !amount.IsPositive() {
		return Extraction{}, reject(ErrNotPositive, "")
	}
	if amount.GreaterThanOrEqual(v.MaxAmount) {
		return Extraction{}, reject(ErrTooLarge, "limit "+v.MaxAmount.String())
	}
	if !amount.Equal(amount.Round(2)) {
		return Extraction{}, reject(ErrTooPrecise, "")
	}
	var cur string
	if c := strings.TrimSpace(r.Currency); c != "" {
		var ok bool
		if cur, ok = NormalizeCurrency(c); !ok {
			return Extraction{}, reject(ErrCurrency, "")
		}
	} else if c, ok := CurrencyFromAmount(r.AmountRaw); ok {
		cur = c
	} else {
		cur = v.DefaultCurrency
	}
	return Extraction{
		Amount:    amount.Round(2),
		Currency:  cur,
		AmountRaw: r.AmountRaw,
		Evidence:  r.Evidence,
	}, nil
}
