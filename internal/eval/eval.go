// Package eval implements the eval subcommand: it runs the extractor over a
// local set of documents with known totals and reports accuracy per model.
package eval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vgeyer/paperless-extract/internal/extract"
	"github.com/vgeyer/paperless-extract/internal/ollama"
	"github.com/vgeyer/paperless-extract/internal/retry"
)

// Case is one line of the manifest (JSON Lines).
type Case struct {
	ID       string           `json:"id"`
	TextFile string           `json:"text_file"`
	Amount   *decimal.Decimal `json:"amount"`
	Currency string           `json:"currency"`
	Text     string           `json:"-"`
}

// LoadCases reads the manifest and the referenced text files.
func LoadCases(path string) ([]Case, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	dir := filepath.Dir(path)
	var cases []Case
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var c Case
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("%s:%d: invalid JSON: %w", path, n, err)
		}
		switch {
		case c.ID == "":
			return nil, fmt.Errorf("%s:%d: empty id", path, n)
		case seen[c.ID]:
			return nil, fmt.Errorf("%s:%d: duplicate id %q", path, n, c.ID)
		case c.TextFile == "":
			return nil, fmt.Errorf("%s:%d: empty text_file", path, n)
		}
		seen[c.ID] = true
		tf := c.TextFile
		if !filepath.IsAbs(tf) {
			tf = filepath.Join(dir, tf)
		}
		b, err := os.ReadFile(tf)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		c.Text = string(b)
		cases = append(cases, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("%s: no cases", path)
	}
	return cases, nil
}

// Outcome classifies one scored case.
type Outcome string

const (
	Correct Outcome = "correct"
	Wrong   Outcome = "wrong"
	Missed  Outcome = "missed"
	Error   Outcome = "error"
)

// Score compares an extraction with the expectation.
func Score(c Case, got extract.Extraction, err error) Outcome {
	if err != nil {
		if errors.Is(err, extract.ErrRejected) || errors.Is(err, extract.ErrEmptyText) {
			if c.Amount == nil {
				return Correct
			}
			return Missed
		}
		return Error
	}
	if c.Amount == nil {
		return Wrong
	}
	if c.Amount.Equal(got.Amount) && (c.Currency == "" || c.Currency == got.Currency) {
		return Correct
	}
	return Wrong
}

// CaseResult is the result of one case with one model configuration.
type CaseResult struct {
	CaseID   string
	Model    string
	NumCtx   int
	Outcome  Outcome
	Got      extract.Extraction
	Err      error
	Duration time.Duration
}

// Summary aggregates results per (Model, NumCtx).
type Summary struct {
	Model                                 string
	NumCtx                                int
	Cases, Correct, Wrong, Missed, Errors int
	Accuracy                              float64
	AvgDuration                           time.Duration
}

// Summarize groups by (Model, NumCtx) in order of first appearance.
func Summarize(results []CaseResult) []Summary {
	type key struct {
		m string
		n int
	}
	idx := map[key]int{}
	var out []Summary
	var total []time.Duration
	for _, r := range results {
		k := key{r.Model, r.NumCtx}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, Summary{Model: r.Model, NumCtx: r.NumCtx})
			total = append(total, 0)
		}
		s := &out[i]
		s.Cases++
		total[i] += r.Duration
		switch r.Outcome {
		case Correct:
			s.Correct++
		case Wrong:
			s.Wrong++
		case Missed:
			s.Missed++
		default:
			s.Errors++
		}
	}
	for i := range out {
		out[i].Accuracy = float64(out[i].Correct) / float64(out[i].Cases)
		out[i].AvgDuration = total[i] / time.Duration(out[i].Cases)
	}
	return out
}

// WriteReport writes the summary table.
func WriteReport(w io.Writer, s []Summary) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tNUM_CTX\tCASES\tCORRECT\tWRONG\tMISSED\tERRORS\tACCURACY\tAVG_TIME")
	for _, r := range s {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%.1f%%\t%s\n",
			r.Model, r.NumCtx, r.Cases, r.Correct, r.Wrong, r.Missed, r.Errors,
			r.Accuracy*100, r.AvgDuration.Round(100*time.Millisecond))
	}
	return tw.Flush()
}

func format(cur string, a decimal.Decimal) string { return cur + a.StringFixed(2) }

func envOr(getenv func(string) string, name, def string) string {
	if v := strings.TrimSpace(getenv(name)); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Run is the eval subcommand (args exclude "eval"). It returns the exit code.
func Run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	set := fs.String("set", "", "path to the cases manifest (JSON Lines), required")
	models := fs.String("models", getenv("OLLAMA_MODEL"), "comma-separated model names")
	numCtx := fs.String("num-ctx", envOr(getenv, "OLLAMA_NUM_CTX", "65536"), "comma-separated context sizes")
	url := fs.String("ollama-url", envOr(getenv, "OLLAMA_URL", "http://localhost:11434"), "Ollama base URL")
	numPredict := fs.Int("num-predict", 0, "reserved output tokens")
	timeout := fs.Duration("timeout", 0, "per-request timeout")
	keepAlive := fs.String("keep-alive", envOr(getenv, "OLLAMA_KEEP_ALIVE", "5m"), "model keep-alive (duration or seconds)")
	promptFile := fs.String("prompt", getenv("PROMPT_FILE"), "prompt template file")
	defCur := fs.String("default-currency", envOr(getenv, "DEFAULT_CURRENCY", "EUR"), "default currency")
	maxAmount := fs.String("max-amount", envOr(getenv, "MAX_AMOUNT", "1000000"), "sanity limit")
	verbose := fs.Bool("v", false, "one line per case")

	// numeric/duration defaults from env, validated after flag parsing
	npDef, toDef := 256, 5*time.Minute
	var envErrs []string
	if v := strings.TrimSpace(getenv("OLLAMA_NUM_PREDICT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			npDef = n
		} else {
			envErrs = append(envErrs, "OLLAMA_NUM_PREDICT")
		}
	}
	if v := strings.TrimSpace(getenv("OLLAMA_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			toDef = d
		} else {
			envErrs = append(envErrs, "OLLAMA_TIMEOUT")
		}
	}
	*numPredict, *timeout = npDef, toDef

	if err := fs.Parse(args); err != nil {
		return 2
	}
	usage := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "eval: "+format+"\n", a...)
		fs.Usage()
		return 2
	}
	if len(envErrs) > 0 {
		return usage("invalid environment variable(s): %s", strings.Join(envErrs, ", "))
	}
	if *set == "" {
		return usage("-set is required")
	}
	modelList := splitList(*models)
	if len(modelList) == 0 {
		return usage("-models (or OLLAMA_MODEL) is required")
	}
	var ctxList []int
	for _, s := range splitList(*numCtx) {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return usage("invalid -num-ctx value %q", s)
		}
		ctxList = append(ctxList, n)
	}
	if len(ctxList) == 0 {
		return usage("-num-ctx must not be empty")
	}
	if *numPredict < 1 {
		return usage("-num-predict must be >= 1")
	}
	if *timeout <= 0 {
		return usage("-timeout must be > 0")
	}
	var ka time.Duration
	if n, err := strconv.ParseInt(*keepAlive, 10, 64); err == nil {
		ka = time.Duration(n) * time.Second
	} else if d, err := time.ParseDuration(*keepAlive); err == nil {
		ka = d
	} else {
		return usage("invalid -keep-alive %q", *keepAlive)
	}
	maxAmt, err := decimal.NewFromString(*maxAmount)
	if err != nil || !maxAmt.IsPositive() {
		return usage("invalid -max-amount %q", *maxAmount)
	}
	cur := strings.ToUpper(strings.TrimSpace(*defCur))
	if len(cur) != 3 {
		return usage("invalid -default-currency %q", *defCur)
	}

	prompt, err := extract.LoadPrompt(*promptFile)
	if err != nil {
		fmt.Fprintf(stderr, "eval: %v\n", err)
		return 1
	}
	cases, err := LoadCases(*set)
	if err != nil {
		fmt.Fprintf(stderr, "eval: %v\n", err)
		return 1
	}

	client := ollama.NewClient(*url, *timeout, nil)
	var results []CaseResult
loop:
	for _, model := range modelList {
		for _, nc := range ctxList {
			ex := &extract.Extractor{
				Chat: client, Prompt: prompt, Model: model, NumCtx: nc, NumPredict: *numPredict,
				KeepAlive: ka, Validator: extract.Validator{MaxAmount: maxAmt, DefaultCurrency: cur},
				Retry: retry.Default,
			}
			for _, c := range cases {
				if ctx.Err() != nil {
					fmt.Fprintln(stderr, "eval: interrupted")
					break loop
				}
				start := time.Now()
				got, err := ex.Extract(ctx, c.Text)
				r := CaseResult{
					CaseID: c.ID, Model: model, NumCtx: nc, Got: got, Err: err,
					Outcome: Score(c, got, err), Duration: time.Since(start),
				}
				results = append(results, r)
				if *verbose {
					expected, gotS := "none", "none"
					if c.Amount != nil {
						expected = format(c.Currency, *c.Amount)
					}
					if err == nil {
						gotS = format(got.Currency, got.Amount)
					}
					line := fmt.Sprintf("case=%s model=%s num_ctx=%d outcome=%s expected=%s got=%s duration=%s",
						c.ID, model, nc, r.Outcome, expected, gotS, r.Duration.Round(100*time.Millisecond))
					if err != nil {
						line += " reason=" + err.Error()
					}
					fmt.Fprintln(stdout, line)
				}
			}
		}
	}

	if len(results) > 0 {
		var buf bytes.Buffer
		if err := WriteReport(&buf, Summarize(results)); err == nil {
			_, _ = stdout.Write(buf.Bytes())
		}
	}
	if ctx.Err() != nil {
		return 1
	}
	return 0
}
