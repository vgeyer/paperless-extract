package config_test

import (
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/vgeyer/paperless-extract/internal/config"
)

const (
	testSecret = "0123456789abcdef-secret"
	testToken  = "paperless-token-0123456789"
)

func baseEnv() map[string]string {
	return map[string]string{
		"WEBHOOK_SECRET":  testSecret,
		"PAPERLESS_URL":   "http://paperless:8000",
		"PAPERLESS_TOKEN": testToken,
		"OLLAMA_MODEL":    "qwen2.5:14b",
	}
}

func getenvFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func noFiles(path string) ([]byte, error) {
	return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
}

func filesFrom(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if v, ok := m[p]; ok {
			return []byte(v), nil
		}
		return noFiles(p)
	}
}

func load(t *testing.T, env map[string]string) (config.Config, error) {
	t.Helper()
	return config.Load(getenvFrom(env), noFiles)
}

// fieldErrors extracts all *FieldError from a (joined) error.
func fieldErrors(err error) []*config.FieldError {
	var out []*config.FieldError
	var walk func(error)
	walk = func(e error) {
		if e == nil {
			return
		}
		if fe, ok := e.(*config.FieldError); ok { //nolint:errorlint // walking the join tree manually
			out = append(out, fe)
			return
		}
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, x := range j.Unwrap() {
				walk(x)
			}
			return
		}
		if u := errors.Unwrap(e); u != nil {
			walk(u)
		}
	}
	walk(err)
	return out
}

func requireFieldError(t *testing.T, err error, varName string, sentinel error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error for %s, got nil", varName)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, %v) = false; err = %v", sentinel, err)
	}
	if !strings.Contains(err.Error(), varName) {
		t.Errorf("error %q does not mention %s", err.Error(), varName)
	}
	for _, fe := range fieldErrors(err) {
		if fe.Var == varName && errors.Is(fe, sentinel) {
			return
		}
	}
	t.Errorf("no *FieldError{Var:%q, Err:%v} in %v", varName, sentinel, err)
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(t, baseEnv())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"ListenAddr", cfg.ListenAddr, ":8080"},
		{"WebhookSecret", cfg.WebhookSecret, testSecret},
		{"PaperlessURL", cfg.PaperlessURL, "http://paperless:8000"},
		{"PaperlessToken", cfg.PaperlessToken, testToken},
		{"OllamaURL", cfg.OllamaURL, "http://ollama:11434"},
		{"OllamaModel", cfg.OllamaModel, "qwen2.5:14b"},
		{"OllamaNumCtx", cfg.OllamaNumCtx, 65536},
		{"OllamaNumPredict", cfg.OllamaNumPredict, 256},
		{"OllamaTimeout", cfg.OllamaTimeout, 5 * time.Minute},
		{"OllamaKeepAlive", cfg.OllamaKeepAlive, 5 * time.Minute},
		{"CustomFieldName", cfg.CustomFieldName, "Total amount"},
		{"DefaultCurrency", cfg.DefaultCurrency, "EUR"},
		{"OverwriteExisting", cfg.OverwriteExisting, false},
		{"QueueSize", cfg.QueueSize, 100},
		{"Workers", cfg.Workers, 1},
		{"PromptFile", cfg.PromptFile, ""},
		{"LogLevel", cfg.LogLevel, slog.LevelInfo},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 25 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if !cfg.MaxAmount.Equal(decimal.NewFromInt(1000000)) {
		t.Errorf("MaxAmount = %s, want 1000000", cfg.MaxAmount)
	}
}

func TestLoadAllSet(t *testing.T) {
	env := baseEnv()
	for k, v := range map[string]string{
		"LISTEN_ADDR":        "127.0.0.1:9090",
		"PAPERLESS_URL":      " https://p.example.com/sub/ ",
		"OLLAMA_URL":         "http://gpu:11434/",
		"OLLAMA_NUM_CTX":     "8192",
		"OLLAMA_NUM_PREDICT": "512",
		"OLLAMA_TIMEOUT":     "90s",
		"OLLAMA_KEEP_ALIVE":  "-1",
		"CUSTOM_FIELD_NAME":  "Betrag",
		"DEFAULT_CURRENCY":   "chf",
		"MAX_AMOUNT":         "5000.50",
		"OVERWRITE_EXISTING": "true",
		"QUEUE_SIZE":         "7",
		"WORKERS":            "3",
		"PROMPT_FILE":        "/config/prompt.tmpl",
		"LOG_LEVEL":          "DEBUG",
		"SHUTDOWN_TIMEOUT":   "10s",
	} {
		env[k] = v
	}
	cfg, err := load(t, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"ListenAddr", cfg.ListenAddr, "127.0.0.1:9090"},
		{"PaperlessURL", cfg.PaperlessURL, "https://p.example.com/sub"},
		{"OllamaURL", cfg.OllamaURL, "http://gpu:11434"},
		{"OllamaNumCtx", cfg.OllamaNumCtx, 8192},
		{"OllamaNumPredict", cfg.OllamaNumPredict, 512},
		{"OllamaTimeout", cfg.OllamaTimeout, 90 * time.Second},
		{"OllamaKeepAlive", cfg.OllamaKeepAlive, -1 * time.Second},
		{"CustomFieldName", cfg.CustomFieldName, "Betrag"},
		{"DefaultCurrency", cfg.DefaultCurrency, "CHF"},
		{"OverwriteExisting", cfg.OverwriteExisting, true},
		{"QueueSize", cfg.QueueSize, 7},
		{"Workers", cfg.Workers, 3},
		{"PromptFile", cfg.PromptFile, "/config/prompt.tmpl"},
		{"LogLevel", cfg.LogLevel, slog.LevelDebug},
		{"ShutdownTimeout", cfg.ShutdownTimeout, 10 * time.Second},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if !cfg.MaxAmount.Equal(decimal.RequireFromString("5000.50")) {
		t.Errorf("MaxAmount = %s", cfg.MaxAmount)
	}
}

func TestLoadWhitespaceOnlyMeansDefault(t *testing.T) {
	env := baseEnv()
	env["OLLAMA_NUM_CTX"] = "   "
	env["LISTEN_ADDR"] = "\t"
	cfg, err := load(t, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OllamaNumCtx != 65536 || cfg.ListenAddr != ":8080" {
		t.Errorf("got NumCtx=%d ListenAddr=%q, want defaults", cfg.OllamaNumCtx, cfg.ListenAddr)
	}
}

func TestLoadRequired(t *testing.T) {
	for _, v := range []string{"WEBHOOK_SECRET", "PAPERLESS_URL", "PAPERLESS_TOKEN", "OLLAMA_MODEL"} {
		t.Run(v, func(t *testing.T) {
			env := baseEnv()
			delete(env, v)
			cfg, err := load(t, env)
			requireFieldError(t, err, v, config.ErrRequired)
			if cfg != (config.Config{}) {
				t.Errorf("Config not zero on error: %+v", cfg)
			}
		})
		t.Run(v+"_blank", func(t *testing.T) {
			env := baseEnv()
			env[v] = "   "
			_, err := load(t, env)
			requireFieldError(t, err, v, config.ErrRequired)
		})
	}
}

func TestLoadAllErrorsReportedTogether(t *testing.T) {
	env := map[string]string{
		"OLLAMA_NUM_CTX": "abc",
		"WORKERS":        "0",
		"LOG_LEVEL":      "verbose",
	}
	_, err := load(t, env)
	if err == nil {
		t.Fatal("expected error")
	}
	for _, v := range []string{"WEBHOOK_SECRET", "PAPERLESS_URL", "PAPERLESS_TOKEN", "OLLAMA_MODEL"} {
		requireFieldError(t, err, v, config.ErrRequired)
	}
	for _, v := range []string{"OLLAMA_NUM_CTX", "WORKERS", "LOG_LEVEL"} {
		requireFieldError(t, err, v, config.ErrInvalid)
	}
	if n := len(fieldErrors(err)); n != 7 {
		t.Errorf("got %d field errors, want 7: %v", n, err)
	}
}

func TestLoadInvalidValues(t *testing.T) {
	cases := []struct {
		v, val string
	}{
		{"LISTEN_ADDR", "8080"},
		{"LISTEN_ADDR", "localhost"},
		{"WEBHOOK_SECRET", "short"},
		{"WEBHOOK_SECRET", "123456789012345"}, // 15 bytes
		{"PAPERLESS_URL", "paperless:8000"},
		{"PAPERLESS_URL", "ftp://paperless"},
		{"PAPERLESS_URL", "http://"},
		{"PAPERLESS_URL", "http://paperless:8000/?x=1"},
		{"PAPERLESS_URL", "http://paperless:8000/#frag"},
		{"OLLAMA_URL", "ollama:11434"},
		{"OLLAMA_URL", "http://ollama:11434?x=y"},
		{"PAPERLESS_URL", "http://user:pass@paperless:8000"},
		{"PAPERLESS_URL", "https://admin@p.example.com/"},
		{"OLLAMA_URL", "http://u:p@ollama:11434"},
		{"OLLAMA_NUM_CTX", "1023"},
		{"OLLAMA_NUM_CTX", "abc"},
		{"OLLAMA_NUM_CTX", "4096.5"},
		{"OLLAMA_NUM_PREDICT", "0"},
		{"OLLAMA_NUM_PREDICT", "65536"}, // must be < num_ctx (default 65536)
		{"OLLAMA_NUM_PREDICT", "x"},
		{"OLLAMA_TIMEOUT", "0s"},
		{"OLLAMA_TIMEOUT", "-5s"},
		{"OLLAMA_TIMEOUT", "5"},
		{"OLLAMA_TIMEOUT", "soon"},
		{"OLLAMA_KEEP_ALIVE", "forever"},
		{"DEFAULT_CURRENCY", "EU"},
		{"DEFAULT_CURRENCY", "EURO"},
		{"DEFAULT_CURRENCY", "€"},
		{"DEFAULT_CURRENCY", "E1R"},
		{"MAX_AMOUNT", "0"},
		{"MAX_AMOUNT", "-1"},
		{"MAX_AMOUNT", "1e"},
		{"MAX_AMOUNT", "lots"},
		{"OVERWRITE_EXISTING", "yes"},
		{"QUEUE_SIZE", "0"},
		{"QUEUE_SIZE", "-3"},
		{"QUEUE_SIZE", "many"},
		{"WORKERS", "0"},
		{"WORKERS", "two"},
		{"LOG_LEVEL", "trace"},
		{"SHUTDOWN_TIMEOUT", "0"},
		{"SHUTDOWN_TIMEOUT", "-1s"},
		{"SHUTDOWN_TIMEOUT", "later"},
	}
	for _, c := range cases {
		t.Run(c.v+"="+c.val, func(t *testing.T) {
			env := baseEnv()
			env[c.v] = c.val
			cfg, err := load(t, env)
			requireFieldError(t, err, c.v, config.ErrInvalid)
			if cfg != (config.Config{}) {
				t.Errorf("Config not zero on error")
			}
		})
	}
}

func TestLoadNumPredictRelativeToNumCtx(t *testing.T) {
	env := baseEnv()
	env["OLLAMA_NUM_CTX"] = "2048"
	env["OLLAMA_NUM_PREDICT"] = "2048"
	_, err := load(t, env)
	requireFieldError(t, err, "OLLAMA_NUM_PREDICT", config.ErrInvalid)

	env["OLLAMA_NUM_PREDICT"] = "2047"
	cfg, err := load(t, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OllamaNumCtx != 2048 || cfg.OllamaNumPredict != 2047 {
		t.Errorf("got %d/%d", cfg.OllamaNumCtx, cfg.OllamaNumPredict)
	}
	env["OLLAMA_NUM_CTX"] = "1024"
	env["OLLAMA_NUM_PREDICT"] = "1"
	if _, err := load(t, env); err != nil {
		t.Errorf("boundary 1024/1 rejected: %v", err)
	}
}

func TestLoadValidValues(t *testing.T) {
	cases := []struct {
		v, val string
		check  func(config.Config) bool
	}{
		{"LISTEN_ADDR", "0.0.0.0:80", func(c config.Config) bool { return c.ListenAddr == "0.0.0.0:80" }},
		{"LISTEN_ADDR", "[::1]:8080", func(c config.Config) bool { return c.ListenAddr == "[::1]:8080" }},
		{"WEBHOOK_SECRET", "1234567890123456", func(c config.Config) bool { return c.WebhookSecret == "1234567890123456" }},
		{"WEBHOOK_SECRET", "  1234567890123456  ", func(c config.Config) bool { return c.WebhookSecret == "1234567890123456" }},
		{"PAPERLESS_URL", "https://p.example.com", func(c config.Config) bool { return c.PaperlessURL == "https://p.example.com" }},
		{"PAPERLESS_URL", "http://paperless:8000/", func(c config.Config) bool { return c.PaperlessURL == "http://paperless:8000" }},
		{"PAPERLESS_URL", "http://h/paperless/", func(c config.Config) bool { return c.PaperlessURL == "http://h/paperless" }},
		{"OLLAMA_KEEP_ALIVE", "300", func(c config.Config) bool { return c.OllamaKeepAlive == 5*time.Minute }},
		{"OLLAMA_KEEP_ALIVE", "0", func(c config.Config) bool { return c.OllamaKeepAlive == 0 }},
		{"OLLAMA_KEEP_ALIVE", "-1", func(c config.Config) bool { return c.OllamaKeepAlive == -time.Second }},
		{"OLLAMA_KEEP_ALIVE", "-1m", func(c config.Config) bool { return c.OllamaKeepAlive == -time.Minute }},
		{"OLLAMA_KEEP_ALIVE", "1h", func(c config.Config) bool { return c.OllamaKeepAlive == time.Hour }},
		{"OLLAMA_NUM_CTX", "1024", func(c config.Config) bool { return c.OllamaNumCtx == 1024 }},
		{"DEFAULT_CURRENCY", " pln ", func(c config.Config) bool { return c.DefaultCurrency == "PLN" }},
		{"MAX_AMOUNT", "0.01", func(c config.Config) bool { return c.MaxAmount.Equal(decimal.RequireFromString("0.01")) }},
		{"OVERWRITE_EXISTING", "1", func(c config.Config) bool { return c.OverwriteExisting }},
		{"OVERWRITE_EXISTING", "FALSE", func(c config.Config) bool { return !c.OverwriteExisting }},
		{"LOG_LEVEL", "Warn", func(c config.Config) bool { return c.LogLevel == slog.LevelWarn }},
		{"LOG_LEVEL", "error", func(c config.Config) bool { return c.LogLevel == slog.LevelError }},
		{"LOG_LEVEL", "INFO", func(c config.Config) bool { return c.LogLevel == slog.LevelInfo }},
	}
	for _, c := range cases {
		t.Run(c.v+"="+c.val, func(t *testing.T) {
			env := baseEnv()
			env[c.v] = c.val
			cfg, err := load(t, env)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !c.check(cfg) {
				t.Errorf("unexpected config: %+v", cfg)
			}
		})
	}
}

func TestLoadFileSecrets(t *testing.T) {
	env := baseEnv()
	delete(env, "WEBHOOK_SECRET")
	delete(env, "PAPERLESS_TOKEN")
	env["WEBHOOK_SECRET_FILE"] = "/run/secrets/webhook"
	env["PAPERLESS_TOKEN_FILE"] = " /run/secrets/token "
	files := map[string]string{
		"/run/secrets/webhook": "  file-secret-0123456789\n",
		"/run/secrets/token":   "file-token-abc\r\n",
	}
	cfg, err := config.Load(getenvFrom(env), filesFrom(files))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebhookSecret != "file-secret-0123456789" {
		t.Errorf("WebhookSecret = %q", cfg.WebhookSecret)
	}
	if cfg.PaperlessToken != "file-token-abc" {
		t.Errorf("PaperlessToken = %q", cfg.PaperlessToken)
	}
}

func TestLoadFileSecretShortStillValidated(t *testing.T) {
	env := baseEnv()
	delete(env, "WEBHOOK_SECRET")
	env["WEBHOOK_SECRET_FILE"] = "/s"
	_, err := config.Load(getenvFrom(env), filesFrom(map[string]string{"/s": "short\n"}))
	requireFieldError(t, err, "WEBHOOK_SECRET", config.ErrInvalid)
}

func TestLoadFileSecretUnreadable(t *testing.T) {
	for _, v := range []string{"WEBHOOK_SECRET", "PAPERLESS_TOKEN"} {
		t.Run(v, func(t *testing.T) {
			env := baseEnv()
			delete(env, v)
			env[v+"_FILE"] = "/does/not/exist"
			_, err := config.Load(getenvFrom(env), noFiles)
			requireFieldError(t, err, v+"_FILE", config.ErrInvalid)
		})
	}
}

func TestLoadFileSecretEmpty(t *testing.T) {
	for _, v := range []string{"WEBHOOK_SECRET", "PAPERLESS_TOKEN"} {
		t.Run(v, func(t *testing.T) {
			env := baseEnv()
			delete(env, v)
			env[v+"_FILE"] = "/empty"
			_, err := config.Load(getenvFrom(env), filesFrom(map[string]string{"/empty": " \n\t "}))
			requireFieldError(t, err, v, config.ErrRequired)
		})
	}
}

func TestLoadFileSecretConflict(t *testing.T) {
	for _, v := range []string{"WEBHOOK_SECRET", "PAPERLESS_TOKEN"} {
		t.Run(v, func(t *testing.T) {
			env := baseEnv()
			env[v+"_FILE"] = "/s"
			_, err := config.Load(getenvFrom(env), filesFrom(map[string]string{"/s": "another-secret-0123456789"}))
			requireFieldError(t, err, v, config.ErrConflict)
		})
	}
}

func TestLoadDoesNotLeakSecretsInErrors(t *testing.T) {
	env := baseEnv()
	env["WEBHOOK_SECRET"] = "tooshort-s3cr"
	env["WEBHOOK_SECRET_FILE"] = ""
	_, err := load(t, env)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "tooshort-s3cr") {
		t.Errorf("error leaks secret: %v", err)
	}
}

func TestFieldErrorFormatting(t *testing.T) {
	fe := &config.FieldError{Var: "OLLAMA_NUM_CTX", Err: config.ErrInvalid, Detail: "must be >= 1024"}
	if got, want := fe.Error(), "config: OLLAMA_NUM_CTX: invalid value: must be >= 1024"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(fe, config.ErrInvalid) {
		t.Error("errors.Is ErrInvalid false")
	}
	if !errors.Is(fe.Unwrap(), config.ErrInvalid) {
		t.Error("Unwrap mismatch")
	}
	fe2 := &config.FieldError{Var: "OLLAMA_MODEL", Err: config.ErrRequired}
	if got, want := fe2.Error(), "config: OLLAMA_MODEL: required"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestLogValueRedactsSecrets(t *testing.T) {
	cfg, err := load(t, baseEnv())
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	logger := slog.New(slog.NewJSONHandler(&sb, nil))
	logger.Info("starting", "config", cfg)
	out := sb.String()
	if strings.Contains(out, testSecret) || strings.Contains(out, testToken) {
		t.Errorf("log leaks secret: %s", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Errorf("log has no [redacted]: %s", out)
	}
	if !strings.Contains(out, "qwen2.5:14b") || !strings.Contains(out, "http://paperless:8000") {
		t.Errorf("log misses non-secret fields: %s", out)
	}

	// empty secrets stay empty
	v := config.Config{}.LogValue()
	var sb2 strings.Builder
	slog.New(slog.NewJSONHandler(&sb2, nil)).Info("x", "config", v)
	if strings.Contains(sb2.String(), "[redacted]") {
		t.Errorf("empty secret rendered as redacted: %s", sb2.String())
	}
	if v.Kind() != slog.KindGroup {
		t.Errorf("LogValue kind = %v, want group", v.Kind())
	}
}

func TestLoadURLUserinfoNotLeaked(t *testing.T) {
	env := baseEnv()
	env["PAPERLESS_URL"] = "http://someuser:hunter2pw@paperless:8000"
	_, err := load(t, env)
	requireFieldError(t, err, "PAPERLESS_URL", config.ErrInvalid)
	if err != nil && strings.Contains(err.Error(), "hunter2pw") {
		t.Errorf("error leaks URL password: %v", err)
	}
}
