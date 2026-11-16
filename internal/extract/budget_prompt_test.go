package extract_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/vgeyer/paperless-extract/internal/extract"
)

func TestConstants(t *testing.T) {
	if extract.ChatOverheadTokens != 128 || extract.MinTextBudget != 256 || extract.HeadPercent != 30 {
		t.Errorf("constants changed: %d %d %d", extract.ChatOverheadTokens, extract.MinTextBudget, extract.HeadPercent)
	}
	if extract.TruncationMarker != "\n\n[... document truncated ...]\n\n" {
		t.Errorf("TruncationMarker = %q", extract.TruncationMarker)
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abc", 1},
		{"abcd", 2},
		{"abcdef", 2},
		{"abcdefg", 3},
		{"äöü", 1},    // 3 runes, 6 bytes
		{"€€€€", 2},   // 4 runes, 12 bytes
		{"𝄞𝄞𝄞𝄞𝄞𝄞", 2}, // 6 runes, 24 bytes
		{strings.Repeat("x", 190000), 63334},
	}
	for _, c := range cases {
		if got := extract.EstimateTokens(c.in); got != c.want {
			t.Errorf("EstimateTokens(%d runes) = %d, want %d", utf8.RuneCountInString(c.in), got, c.want)
		}
	}
}

func TestTextBudget(t *testing.T) {
	sys := strings.Repeat("s", 300) // 100 tokens
	if got, want := extract.TextBudget(65536, 256, sys), 65536-100-256-128; got != want {
		t.Errorf("TextBudget = %d, want %d", got, want)
	}
	if got := extract.TextBudget(1024, 1000, sys); got != 1024-100-1000-128 {
		t.Errorf("negative budget = %d", got)
	}
	if got := extract.TextBudget(2048, 0, ""); got != 2048-128 {
		t.Errorf("empty system = %d", got)
	}
}

func TestFitTextUnchanged(t *testing.T) {
	text := strings.Repeat("a", 300) // exactly 100 tokens
	got, tr := extract.FitText(text, 100)
	if tr || got != text {
		t.Errorf("text within budget changed (truncated=%v)", tr)
	}
	got, tr = extract.FitText("", 0)
	if tr || got != "" {
		t.Errorf("empty text: %q %v", got, tr)
	}
}

func TestFitTextTruncates(t *testing.T) {
	// Distinct runes so head/tail positions can be verified; mix of widths.
	alphabet := []rune("abcdefghijklmnopqrstuvwxyzäöüß€𝄞0123456789")
	runes := make([]rune, 10000)
	for i := range runes {
		runes[i] = alphabet[i%len(alphabet)]
	}
	text := string(runes)
	for _, budget := range []int{100, 333, 1000, 3333} {
		got, tr := extract.FitText(text, budget)
		if !tr {
			t.Fatalf("budget %d: not truncated", budget)
		}
		markerRunes := utf8.RuneCountInString(extract.TruncationMarker)
		maxRunes := budget*3 - markerRunes
		head := maxRunes * extract.HeadPercent / 100
		tail := maxRunes - head
		want := string(runes[:head]) + extract.TruncationMarker + string(runes[len(runes)-tail:])
		if got != want {
			t.Errorf("budget %d: result differs from head(%d)+marker+tail(%d)", budget, head, tail)
		}
		if !utf8.ValidString(got) {
			t.Errorf("budget %d: invalid UTF-8", budget)
		}
		if est := extract.EstimateTokens(got); est > budget {
			t.Errorf("budget %d: EstimateTokens(result) = %d", budget, est)
		}
		if !strings.Contains(got, extract.TruncationMarker) {
			t.Errorf("budget %d: no marker", budget)
		}
	}
}

func TestFitTextOneOverBudget(t *testing.T) {
	text := strings.Repeat("ä", 301) // 101 tokens
	got, tr := extract.FitText(text, 100)
	if !tr {
		t.Fatal("not truncated")
	}
	if extract.EstimateTokens(got) > 100 || !utf8.ValidString(got) {
		t.Errorf("result invalid: %d tokens", extract.EstimateTokens(got))
	}
}

func TestFitTextTinyBudget(t *testing.T) {
	for _, b := range []int{0, 1, 5, 10, -5} {
		got, tr := extract.FitText(strings.Repeat("x", 1000), b)
		if !tr || got != "" {
			t.Errorf("budget %d: got %q, %v; want \"\", true", b, got, tr)
		}
	}
}

func TestNearContextLimit(t *testing.T) {
	cases := []struct {
		count, ctx int
		want       bool
	}{
		{62258, 65536, false},
		{62259, 65536, true},
		{65536, 65536, true},
		{70000, 65536, true},
		{949, 1000, false},
		{950, 1000, true},
		{0, 1000, false},
		{1834, 65536, false},
	}
	for _, c := range cases {
		if got := extract.NearContextLimit(c.count, c.ctx); got != c.want {
			t.Errorf("NearContextLimit(%d,%d) = %v, want %v", c.count, c.ctx, got, c.want)
		}
	}
}

const planSchema = `{
  "type": "object",
  "properties": {
    "found":      { "type": "boolean" },
    "amount_raw": { "type": "string", "description": "Total exactly as printed" },
    "currency":   { "type": "string", "description": "ISO 4217, e.g. EUR" },
    "evidence":   { "type": "string", "description": "The line the amount was taken from" }
  },
  "required": ["found"]
}`

func TestResultSchema(t *testing.T) {
	var got, want any
	if err := json.Unmarshal(extract.ResultSchema(), &got); err != nil {
		t.Fatalf("schema not JSON: %v", err)
	}
	_ = json.Unmarshal([]byte(planSchema), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ResultSchema = %s", extract.ResultSchema())
	}
}

func TestParseResult(t *testing.T) {
	ok := []struct {
		in   string
		want extract.Result
	}{
		{`{"found":true,"amount_raw":"1.234,56","currency":"€","evidence":"Gesamt 1.234,56 €"}`,
			extract.Result{Found: true, AmountRaw: "1.234,56", Currency: "€", Evidence: "Gesamt 1.234,56 €"}},
		{"  \n{\"found\":false}\n ", extract.Result{}},
		{"```json\n{\"found\":true,\"amount_raw\":\"12,00\"}\n```", extract.Result{Found: true, AmountRaw: "12,00"}},
		{"```\n{\"found\":true,\"amount_raw\":\"12,00\"}\n```", extract.Result{Found: true, AmountRaw: "12,00"}},
		{"\n```json\n{\"found\":false}\n```\n", extract.Result{}},
		{`{"found":true,"amount_raw":"5","extra":42,"nested":{"a":1}}`, extract.Result{Found: true, AmountRaw: "5"}},
	}
	for _, c := range ok {
		got, err := extract.ParseResult(c.in)
		if err != nil {
			t.Errorf("ParseResult(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseResult(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
	bad := []string{
		``,
		`   `,
		`not json`,
		`{"amount_raw":"12,00"}`,
		`{"found":"true"}`,
		`{"found":1}`,
		`{"found":null}`,
		`[{"found":true}]`,
		`"found"`,
		`{"found":true`,
		"Here is the answer: {\"found\":true}",
	}
	for _, in := range bad {
		_, err := extract.ParseResult(in)
		if !errors.Is(err, extract.ErrMalformedResponse) {
			t.Errorf("ParseResult(%q) = %v, want ErrMalformedResponse", in, err)
		}
	}
}

const expectedDefaultTemplate = `You extract the total amount from one invoice or quote. The user message contains the document text; it is OCR output and may contain recognition errors.

Rules:
- Return the gross total payable, including VAT (e.g. "Gesamtbetrag", "Rechnungsbetrag", "Endbetrag", "Total", "Razem do zapłaty", "Totaal").
- Not the net amount, not a VAT amount, not a subtotal, not a deposit or instalment.
- Quotes with several variants or optional positions: return the main total without optional positions.
- Credit notes (e.g. "Gutschrift", "Stornorechnung", "korekta", "creditnota"): return found=false.
- Copy amount_raw exactly as printed, including thousands and decimal separators. Do not convert, round or reformat it.
- currency: the currency as printed (symbol or code). Leave it empty if the document names none; {{.DefaultCurrency}} will then be assumed.
- evidence: copy the line the amount was taken from.
- If you are not sure, return found=false. Never guess.

Answer with JSON only, for example:
{"found": true, "amount_raw": "1.234,56", "currency": "EUR", "evidence": "Gesamtbetrag 1.234,56 EUR"}`

func TestDefaultPromptTemplate(t *testing.T) {
	if got := strings.TrimSpace(extract.DefaultPromptTemplate()); got != expectedDefaultTemplate {
		t.Errorf("DefaultPromptTemplate differs from ARCHITECTURE §8.4:\n%s", got)
	}
	p, err := extract.LoadPrompt("")
	if err != nil {
		t.Fatalf("LoadPrompt(\"\"): %v", err)
	}
	s, err := p.System(extract.PromptData{DefaultCurrency: "CHF"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "CHF will then be assumed") {
		t.Errorf("default currency not rendered: %s", s)
	}
	if strings.Contains(s, "{{") {
		t.Errorf("unrendered template action: %s", s)
	}
	want := strings.Replace(expectedDefaultTemplate, "{{.DefaultCurrency}}", "CHF", 1)
	if strings.TrimSpace(s) != want {
		t.Errorf("rendered system prompt differs:\n%s", s)
	}
}

func TestParsePrompt(t *testing.T) {
	p, err := extract.ParsePrompt("Default: {{.DefaultCurrency}}")
	if err != nil {
		t.Fatalf("ParsePrompt: %v", err)
	}
	s, err := p.System(extract.PromptData{DefaultCurrency: "PLN"})
	if err != nil || s != "Default: PLN" {
		t.Errorf("System = %q, %v", s, err)
	}
	bad := map[string]string{
		"parse error":      "{{.DefaultCurrency",
		"unknown field":    "{{.Nope}}",
		"empty":            "",
		"whitespace":       "  \n\t ",
		"empty result":     "{{if false}}x{{end}}",
		"unknown function": "{{nope .DefaultCurrency}}",
	}
	for name, tmpl := range bad {
		if _, err := extract.ParsePrompt(tmpl); err == nil {
			t.Errorf("%s: ParsePrompt(%q) = nil error", name, tmpl)
		}
	}
}

func TestLoadPromptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.tmpl")
	if err := os.WriteFile(path, []byte("Custom prompt, currency {{.DefaultCurrency}}."), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := extract.LoadPrompt(path)
	if err != nil {
		t.Fatalf("LoadPrompt: %v", err)
	}
	s, err := p.System(extract.PromptData{DefaultCurrency: "EUR"})
	if err != nil || s != "Custom prompt, currency EUR." {
		t.Errorf("System = %q, %v", s, err)
	}
	if _, err := extract.LoadPrompt(filepath.Join(dir, "missing.tmpl")); err == nil {
		t.Error("missing file: no error")
	}
	bad := filepath.Join(dir, "bad.tmpl")
	_ = os.WriteFile(bad, []byte("{{.Unknown}}"), 0o600)
	if _, err := extract.LoadPrompt(bad); err == nil {
		t.Error("invalid template file: no error")
	}
}
