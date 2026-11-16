package eval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vgeyer/paperless-extract/internal/eval"
	"github.com/vgeyer/paperless-extract/internal/extract"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadCases(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(t.TempDir(), "abs.txt")
	writeFile(t, filepath.Join(dir, "texts", "inv-001.txt"), "Gesamt 1.234,56 EUR")
	writeFile(t, filepath.Join(dir, "texts", "cn-001.txt"), "Gutschrift -50,00")
	writeFile(t, abs, "Total CHF 99.00")
	manifest := strings.Join([]string{
		"# evaluation set",
		"",
		`{"id":"inv-001","text_file":"texts/inv-001.txt","amount":"1234.56","currency":"EUR"}`,
		"   ",
		`{"id":"cn-001","text_file":"texts/cn-001.txt","amount":null}`,
		`# another comment`,
		fmt.Sprintf(`{"id":"abs","text_file":%q,"amount":"99"}`, abs),
		`{"id":"noamount","text_file":"texts/cn-001.txt"}`,
	}, "\n") + "\n"
	path := filepath.Join(dir, "cases.jsonl")
	writeFile(t, path, manifest)

	cases, err := eval.LoadCases(path)
	if err != nil {
		t.Fatalf("LoadCases: %v", err)
	}
	if len(cases) != 4 {
		t.Fatalf("got %d cases", len(cases))
	}
	c0 := cases[0]
	if c0.ID != "inv-001" || c0.TextFile != "texts/inv-001.txt" || c0.Currency != "EUR" || c0.Text != "Gesamt 1.234,56 EUR" {
		t.Errorf("case 0 = %+v", c0)
	}
	if c0.Amount == nil || !c0.Amount.Equal(decimal.RequireFromString("1234.56")) {
		t.Errorf("case 0 amount = %v", c0.Amount)
	}
	if cases[1].Amount != nil || cases[1].Text != "Gutschrift -50,00" {
		t.Errorf("case 1 = %+v", cases[1])
	}
	if cases[2].Text != "Total CHF 99.00" || cases[2].Amount == nil || !cases[2].Amount.Equal(decimal.NewFromInt(99)) {
		t.Errorf("case 2 = %+v", cases[2])
	}
	if cases[3].Amount != nil {
		t.Errorf("omitted amount should be nil")
	}
}

func TestLoadCasesErrors(t *testing.T) {
	lineNo := func(n int) *regexp.Regexp { return regexp.MustCompile(fmt.Sprintf(`\b%d\b`, n)) }
	cases := []struct {
		name     string
		manifest string
		line     int // 0 = not checked
	}{
		{"invalid json", "# c\n{\"id\":\"a\",\"text_file\":\"a.txt\"}\n{oops\n", 3},
		{"duplicate id", "{\"id\":\"a\",\"text_file\":\"a.txt\"}\n\n{\"id\":\"a\",\"text_file\":\"a.txt\"}\n", 3},
		{"empty id", "{\"id\":\"\",\"text_file\":\"a.txt\"}\n", 1},
		{"missing id", "{\"text_file\":\"a.txt\"}\n", 1},
		{"empty text_file", "{\"id\":\"a\",\"text_file\":\"a.txt\"}\n{\"id\":\"b\",\"text_file\":\"\"}\n", 2},
		{"unreadable text", "{\"id\":\"a\",\"text_file\":\"missing.txt\"}\n", 1},
		{"no cases", "# only comments\n\n", 0},
		{"empty file", "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "a.txt"), "x")
			path := filepath.Join(dir, "cases.jsonl")
			writeFile(t, path, c.manifest)
			_, err := eval.LoadCases(path)
			if err == nil {
				t.Fatal("expected error")
			}
			if c.line > 0 && !lineNo(c.line).MatchString(err.Error()) {
				t.Errorf("error %q lacks line number %d", err.Error(), c.line)
			}
		})
	}
	if _, err := eval.LoadCases(filepath.Join(t.TempDir(), "nope.jsonl")); err == nil {
		t.Error("missing manifest: no error")
	}
}

func dec(s string) *decimal.Decimal {
	d := decimal.RequireFromString(s)
	return &d
}

func TestScore(t *testing.T) {
	rejected := fmt.Errorf("%w: %w", extract.ErrRejected, extract.ErrNotInText)
	got := extract.Extraction{Amount: decimal.RequireFromString("1234.56"), Currency: "EUR"}
	cases := []struct {
		name string
		c    eval.Case
		got  extract.Extraction
		err  error
		want eval.Outcome
	}{
		{"rejected, expected none", eval.Case{ID: "1"}, extract.Extraction{}, rejected, eval.Correct},
		{"rejected, expected value", eval.Case{ID: "1", Amount: dec("1")}, extract.Extraction{}, rejected, eval.Missed},
		{"empty text, expected none", eval.Case{ID: "1"}, extract.Extraction{}, extract.ErrEmptyText, eval.Correct},
		{"empty text, expected value", eval.Case{ID: "1", Amount: dec("1")}, extract.Extraction{}, extract.ErrEmptyText, eval.Missed},
		{"malformed", eval.Case{ID: "1", Amount: dec("1")}, extract.Extraction{}, extract.ErrMalformedResponse, eval.Error},
		{"transport, expected none", eval.Case{ID: "1"}, extract.Extraction{}, errors.New("conn refused"), eval.Error},
		{"value, expected none", eval.Case{ID: "1"}, got, nil, eval.Wrong},
		{"match no currency", eval.Case{ID: "1", Amount: dec("1234.560")}, got, nil, eval.Correct},
		{"match currency", eval.Case{ID: "1", Amount: dec("1234.56"), Currency: "EUR"}, got, nil, eval.Correct},
		{"currency mismatch", eval.Case{ID: "1", Amount: dec("1234.56"), Currency: "CHF"}, got, nil, eval.Wrong},
		{"amount mismatch", eval.Case{ID: "1", Amount: dec("1234.57")}, got, nil, eval.Wrong},
	}
	for _, c := range cases {
		if o := eval.Score(c.c, c.got, c.err); o != c.want {
			t.Errorf("%s: Score = %s, want %s", c.name, o, c.want)
		}
	}
}

func TestSummarize(t *testing.T) {
	rs := []eval.CaseResult{
		{CaseID: "a", Model: "m1", NumCtx: 4096, Outcome: eval.Correct, Duration: 1 * time.Second},
		{CaseID: "a", Model: "m2", NumCtx: 4096, Outcome: eval.Wrong, Duration: 4 * time.Second},
		{CaseID: "b", Model: "m1", NumCtx: 4096, Outcome: eval.Missed, Duration: 3 * time.Second},
		{CaseID: "a", Model: "m1", NumCtx: 8192, Outcome: eval.Error, Duration: 2 * time.Second},
		{CaseID: "c", Model: "m1", NumCtx: 4096, Outcome: eval.Correct, Duration: 2 * time.Second},
		{CaseID: "d", Model: "m1", NumCtx: 4096, Outcome: eval.Correct, Duration: 2 * time.Second},
	}
	s := eval.Summarize(rs)
	if len(s) != 3 {
		t.Fatalf("got %d summaries: %+v", len(s), s)
	}
	keys := []string{fmt.Sprint(s[0].Model, s[0].NumCtx), fmt.Sprint(s[1].Model, s[1].NumCtx), fmt.Sprint(s[2].Model, s[2].NumCtx)}
	if strings.Join(keys, ",") != "m14096,m24096,m18192" {
		t.Errorf("order = %v", keys)
	}
	a := s[0]
	if a.Cases != 4 || a.Correct != 3 || a.Missed != 1 || a.Wrong != 0 || a.Errors != 0 {
		t.Errorf("summary m1/4096 = %+v", a)
	}
	if a.Accuracy != 0.75 || a.AvgDuration != 2*time.Second {
		t.Errorf("accuracy/avg = %v/%v", a.Accuracy, a.AvgDuration)
	}
	if s[1].Wrong != 1 || s[1].Accuracy != 0 || s[1].AvgDuration != 4*time.Second {
		t.Errorf("summary m2 = %+v", s[1])
	}
	if s[2].Errors != 1 || s[2].Cases != 1 {
		t.Errorf("summary m1/8192 = %+v", s[2])
	}
	if len(eval.Summarize(nil)) != 0 {
		t.Error("Summarize(nil) not empty")
	}
}

func TestWriteReport(t *testing.T) {
	var buf bytes.Buffer
	err := eval.WriteReport(&buf, []eval.Summary{
		{Model: "qwen2.5:14b", NumCtx: 65536, Cases: 10, Correct: 9, Wrong: 1, Accuracy: 0.9, AvgDuration: 12340 * time.Millisecond},
		{Model: "llama3", NumCtx: 8192, Cases: 3, Correct: 3, Accuracy: 1, AvgDuration: 1250 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %q", lines)
	}
	if h := strings.Join(strings.Fields(lines[0]), " "); h != "MODEL NUM_CTX CASES CORRECT WRONG MISSED ERRORS ACCURACY AVG_TIME" {
		t.Errorf("header = %q", lines[0])
	}
	if f := strings.Join(strings.Fields(lines[1]), " "); f != "qwen2.5:14b 65536 10 9 1 0 0 90.0% 12.3s" {
		t.Errorf("row 1 = %q", f)
	}
	if f := strings.Fields(lines[2]); len(f) != 9 || f[7] != "100.0%" || (f[8] != "1.2s" && f[8] != "1.3s") {
		t.Errorf("row 2 = %q", lines[2])
	}
}

type ollamaFake struct {
	mu    sync.Mutex
	reqs  []map[string]any
	reply string
}

func (o *ollamaFake) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	o.mu.Lock()
	o.reqs = append(o.reqs, m)
	o.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/api/tags" {
		_, _ = io.WriteString(w, `{"models":[{"name":"m1:latest","model":"m1:latest"},{"name":"m2:latest","model":"m2:latest"},{"name":"envmodel:latest","model":"envmodel:latest"}]}`)
		return
	}
	if r.URL.Path != "/api/chat" {
		w.WriteHeader(404)
		return
	}
	resp := map[string]any{
		"model":             m["model"],
		"message":           map[string]any{"role": "assistant", "content": o.reply},
		"done":              true,
		"done_reason":       "stop",
		"prompt_eval_count": 100,
		"eval_count":        10,
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func evalSet(t *testing.T) string {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "texts", "inv.txt"), "ACME\nGesamtbetrag 1.234,56 EUR\n")
	writeFile(t, filepath.Join(dir, "texts", "cn.txt"), "Gutschrift\nBetrag -50,00 EUR\n")
	path := filepath.Join(dir, "cases.jsonl")
	writeFile(t, path, `{"id":"inv-001","text_file":"texts/inv.txt","amount":"1234.56","currency":"EUR"}`+"\n"+
		`{"id":"cn-001","text_file":"texts/cn.txt","amount":null}`+"\n")
	return path
}

func emptyEnv(string) string { return "" }

func TestRunUsageErrors(t *testing.T) {
	set := evalSet(t)
	cases := map[string][]string{
		"no -set":      {"-models", "m1"},
		"no models":    {"-set", set},
		"unknown flag": {"-set", set, "-models", "m1", "-bogus"},
		"bad num-ctx":  {"-set", set, "-models", "m1", "-num-ctx", "abc"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			if code := eval.Run(context.Background(), args, emptyEnv, &out, &errb); code != 2 {
				t.Errorf("exit = %d, want 2 (stderr %q)", code, errb.String())
			}
		})
	}
}

func TestRunRuntimeErrors(t *testing.T) {
	var out, errb bytes.Buffer
	code := eval.Run(context.Background(), []string{"-set", filepath.Join(t.TempDir(), "missing.jsonl"), "-models", "m1"}, emptyEnv, &out, &errb)
	if code != 1 {
		t.Errorf("missing set: exit = %d, want 1", code)
	}
	badPrompt := filepath.Join(t.TempDir(), "p.tmpl")
	writeFile(t, badPrompt, "{{.Nope}}")
	code = eval.Run(context.Background(), []string{"-set", evalSet(t), "-models", "m1", "-prompt", badPrompt}, emptyEnv, &out, &errb)
	if code != 1 {
		t.Errorf("bad prompt: exit = %d, want 1", code)
	}
}

func TestRunAgainstFakeOllama(t *testing.T) {
	fake := &ollamaFake{reply: `{"found":true,"amount_raw":"1.234,56","currency":"EUR","evidence":"Gesamtbetrag 1.234,56 EUR"}`}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	var out, errb bytes.Buffer
	args := []string{"-set", evalSet(t), "-models", "m1,m2", "-num-ctx", "4096,8192", "-ollama-url", srv.URL, "-v"}
	code := eval.Run(context.Background(), args, emptyEnv, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	fake.mu.Lock()
	var chats []map[string]any
	for _, r := range fake.reqs {
		if r != nil && r["messages"] != nil {
			chats = append(chats, r)
		}
	}
	fake.mu.Unlock()
	if len(chats) != 8 {
		t.Fatalf("chat calls = %d, want 8 (2 models × 2 ctx × 2 cases)", len(chats))
	}
	// models outer, num-ctx inner
	wantOrder := []string{"m1/4096", "m1/4096", "m1/8192", "m1/8192", "m2/4096", "m2/4096", "m2/8192", "m2/8192"}
	for i, r := range chats {
		opts, _ := r["options"].(map[string]any)
		got := fmt.Sprintf("%v/%v", r["model"], opts["num_ctx"])
		if got != wantOrder[i] {
			t.Errorf("call %d = %s, want %s", i, got, wantOrder[i])
		}
	}
	s := out.String()
	for _, want := range []string{
		"case=inv-001 model=m1 num_ctx=4096 outcome=correct expected=EUR1234.56 got=EUR1234.56",
		// credit note text does not contain 1.234,56 → rejected → correct (expected none)
		"case=cn-001 model=m1 num_ctx=4096 outcome=correct expected=none got=none",
		"MODEL",
		"ACCURACY",
		"100.0%",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("stdout lacks %q:\n%s", want, s)
		}
	}
	if n := strings.Count(s, "case="); n != 8 {
		t.Errorf("verbose lines = %d, want 8", n)
	}
}

func TestRunEnvFallback(t *testing.T) {
	fake := &ollamaFake{reply: `{"found":false}`}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	env := map[string]string{"OLLAMA_MODEL": "envmodel", "OLLAMA_URL": srv.URL, "OLLAMA_NUM_CTX": "4096"}
	var out, errb bytes.Buffer
	code := eval.Run(context.Background(), []string{"-set", evalSet(t)}, func(k string) string { return env[k] }, &out, &errb)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, errb.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var chats int
	for _, r := range fake.reqs {
		if r == nil || r["messages"] == nil {
			continue
		}
		chats++
		opts, _ := r["options"].(map[string]any)
		if r["model"] != "envmodel" || opts["num_ctx"] != float64(4096) {
			t.Errorf("request model=%v num_ctx=%v", r["model"], opts["num_ctx"])
		}
	}
	if chats != 2 {
		t.Errorf("chat calls = %d", chats)
	}
	if !strings.Contains(out.String(), "envmodel") || !strings.Contains(out.String(), "50.0%") {
		t.Errorf("report:\n%s", out.String())
	}
}
