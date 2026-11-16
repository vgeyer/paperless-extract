// Package config parses and validates the service configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Sentinel errors, wrapped by *FieldError. Test with errors.Is.
var (
	ErrRequired = errors.New("required")
	ErrInvalid  = errors.New("invalid value")
	ErrConflict = errors.New("both X and X_FILE set")
)

// FieldError describes one problem with one variable.
type FieldError struct {
	Var    string
	Err    error
	Detail string
}

// Error implements error.
func (e *FieldError) Error() string {
	s := "config: " + e.Var + ": " + e.Err.Error()
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// Unwrap returns the sentinel error.
func (e *FieldError) Unwrap() error { return e.Err }

// Config is the validated service configuration.
type Config struct {
	ListenAddr        string
	WebhookSecret     string
	PaperlessURL      string
	PaperlessToken    string
	OllamaURL         string
	OllamaModel       string
	OllamaNumCtx      int
	OllamaNumPredict  int
	OllamaTimeout     time.Duration
	OllamaKeepAlive   time.Duration
	CustomFieldName   string
	DefaultCurrency   string
	MaxAmount         decimal.Decimal
	OverwriteExisting bool
	QueueSize         int
	Workers           int
	PromptFile        string
	LogLevel          slog.Level
	ShutdownTimeout   time.Duration
}

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

type loader struct {
	getenv   func(string) string
	readFile func(string) ([]byte, error)
	errs     []error
}

func (l *loader) fail(name string, sentinel error, format string, args ...any) {
	d := ""
	if format != "" {
		d = fmt.Sprintf(format, args...)
	}
	l.errs = append(l.errs, &FieldError{Var: name, Err: sentinel, Detail: d})
}

func (l *loader) get(name string) string {
	return strings.TrimSpace(l.getenv(name))
}

// secret resolves X / X_FILE.
func (l *loader) secret(name string) string {
	v := l.get(name)
	f := l.get(name + "_FILE")
	switch {
	case v != "" && f != "":
		l.fail(name, ErrConflict, "%s and %s_FILE are mutually exclusive", name, name)
		return ""
	case f != "":
		b, err := l.readFile(f)
		if err != nil {
			l.fail(name+"_FILE", ErrInvalid, "cannot read file")
			return ""
		}
		v = strings.TrimSpace(string(b))
	}
	if v == "" {
		l.fail(name, ErrRequired, "")
	}
	return v
}

func (l *loader) intVar(name string, def, min int) int {
	s := l.get(name)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		l.fail(name, ErrInvalid, "not an integer")
		return def
	}
	if n < min {
		l.fail(name, ErrInvalid, "must be >= %d", min)
	}
	return n
}

func (l *loader) duration(name string, def time.Duration) time.Duration {
	s := l.get(name)
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		l.fail(name, ErrInvalid, "not a duration")
		return def
	}
	if d <= 0 {
		l.fail(name, ErrInvalid, "must be > 0")
	}
	return d
}

func (l *loader) baseURL(name, def string, required bool) string {
	s := l.get(name)
	if s == "" {
		if required {
			l.fail(name, ErrRequired, "")
			return ""
		}
		s = def
	}
	u, err := url.Parse(s)
	switch {
	case err != nil:
		l.fail(name, ErrInvalid, "not a valid URL")
	case u.Scheme != "http" && u.Scheme != "https":
		l.fail(name, ErrInvalid, "scheme must be http or https")
	case err == nil && u.User != nil:
		l.fail(name, ErrInvalid, "must not contain credentials (userinfo)")
	case u.Host == "" || u.Hostname() == "":
		l.fail(name, ErrInvalid, "missing host")
	case u.RawQuery != "" || u.Fragment != "" || strings.Contains(s, "?") || strings.Contains(s, "#"):
		l.fail(name, ErrInvalid, "must not contain query or fragment")
	}
	return strings.TrimRight(s, "/")
}

// Load reads configuration through getenv and readFile, validates everything
// and reports all problems at once as errors.Join of *FieldError values.
func Load(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	l := &loader{getenv: getenv, readFile: readFile}
	var c Config

	c.ListenAddr = l.get("LISTEN_ADDR")
	if c.ListenAddr == "" {
		c.ListenAddr = ":8080"
	}
	if _, _, err := net.SplitHostPort(c.ListenAddr); err != nil {
		l.fail("LISTEN_ADDR", ErrInvalid, "must be host:port")
	}

	c.WebhookSecret = l.secret("WEBHOOK_SECRET")
	if c.WebhookSecret != "" && len(c.WebhookSecret) < 16 {
		l.fail("WEBHOOK_SECRET", ErrInvalid, "must be at least 16 bytes")
	}
	c.PaperlessURL = l.baseURL("PAPERLESS_URL", "", true)
	c.PaperlessToken = l.secret("PAPERLESS_TOKEN")
	c.OllamaURL = l.baseURL("OLLAMA_URL", "http://ollama:11434", false)
	c.OllamaModel = l.get("OLLAMA_MODEL")
	if c.OllamaModel == "" {
		l.fail("OLLAMA_MODEL", ErrRequired, "")
	}
	c.OllamaNumCtx = l.intVar("OLLAMA_NUM_CTX", 65536, 1024)
	c.OllamaNumPredict = l.intVar("OLLAMA_NUM_PREDICT", 256, 1)
	if c.OllamaNumPredict >= c.OllamaNumCtx {
		l.fail("OLLAMA_NUM_PREDICT", ErrInvalid, "must be < OLLAMA_NUM_CTX")
	}
	c.OllamaTimeout = l.duration("OLLAMA_TIMEOUT", 5*time.Minute)

	c.OllamaKeepAlive = 5 * time.Minute
	if s := l.get("OLLAMA_KEEP_ALIVE"); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			c.OllamaKeepAlive = time.Duration(n) * time.Second
		} else if d, err := time.ParseDuration(s); err == nil {
			c.OllamaKeepAlive = d
		} else {
			l.fail("OLLAMA_KEEP_ALIVE", ErrInvalid, "not a duration or number of seconds")
		}
	}

	c.CustomFieldName = l.get("CUSTOM_FIELD_NAME")
	if c.CustomFieldName == "" {
		c.CustomFieldName = "Total amount"
	}

	c.DefaultCurrency = strings.ToUpper(l.get("DEFAULT_CURRENCY"))
	if c.DefaultCurrency == "" {
		c.DefaultCurrency = "EUR"
	}
	if !currencyRe.MatchString(c.DefaultCurrency) {
		l.fail("DEFAULT_CURRENCY", ErrInvalid, "must be 3 letters")
	}

	c.MaxAmount = decimal.NewFromInt(1000000)
	if s := l.get("MAX_AMOUNT"); s != "" {
		d, err := decimal.NewFromString(s)
		switch {
		case err != nil:
			l.fail("MAX_AMOUNT", ErrInvalid, "not a number")
		case !d.IsPositive():
			l.fail("MAX_AMOUNT", ErrInvalid, "must be > 0")
		default:
			c.MaxAmount = d
		}
	}

	if s := l.get("OVERWRITE_EXISTING"); s != "" {
		b, err := strconv.ParseBool(s)
		if err != nil {
			l.fail("OVERWRITE_EXISTING", ErrInvalid, "not a boolean")
		}
		c.OverwriteExisting = b
	}

	c.QueueSize = l.intVar("QUEUE_SIZE", 100, 1)
	c.Workers = l.intVar("WORKERS", 1, 1)
	c.PromptFile = l.get("PROMPT_FILE")

	c.LogLevel = slog.LevelInfo
	if s := l.get("LOG_LEVEL"); s != "" {
		switch strings.ToLower(s) {
		case "debug":
			c.LogLevel = slog.LevelDebug
		case "info":
			c.LogLevel = slog.LevelInfo
		case "warn":
			c.LogLevel = slog.LevelWarn
		case "error":
			c.LogLevel = slog.LevelError
		default:
			l.fail("LOG_LEVEL", ErrInvalid, "must be debug, info, warn or error")
		}
	}

	c.ShutdownTimeout = l.duration("SHUTDOWN_TIMEOUT", 25*time.Second)

	if len(l.errs) > 0 {
		return Config{}, errors.Join(l.errs...)
	}
	return c, nil
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	return "[redacted]"
}

// LogValue implements slog.LogValuer with secrets redacted.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("listen_addr", c.ListenAddr),
		slog.String("webhook_secret", redact(c.WebhookSecret)),
		slog.String("paperless_url", c.PaperlessURL),
		slog.String("paperless_token", redact(c.PaperlessToken)),
		slog.String("ollama_url", c.OllamaURL),
		slog.String("ollama_model", c.OllamaModel),
		slog.Int("ollama_num_ctx", c.OllamaNumCtx),
		slog.Int("ollama_num_predict", c.OllamaNumPredict),
		slog.Duration("ollama_timeout", c.OllamaTimeout),
		slog.Duration("ollama_keep_alive", c.OllamaKeepAlive),
		slog.String("custom_field_name", c.CustomFieldName),
		slog.String("default_currency", c.DefaultCurrency),
		slog.String("max_amount", c.MaxAmount.String()),
		slog.Bool("overwrite_existing", c.OverwriteExisting),
		slog.Int("queue_size", c.QueueSize),
		slog.Int("workers", c.Workers),
		slog.String("prompt_file", c.PromptFile),
		slog.String("log_level", c.LogLevel.String()),
		slog.Duration("shutdown_timeout", c.ShutdownTimeout),
	)
}
