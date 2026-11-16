# paperless-extract — Architecture contract

Binding contract for implementation (engineer), tests (tester) and build/CI (DevOps).
It refines PLAN.md. Where PLAN.md was open, the choice is marked **Decision:**.
Every identifier below is exported API. Tests are written against exactly these
signatures, so do not rename, reorder parameters or change types. Unexported helpers
are free.

- Module: `github.com/vgeyer/paperless-extract`, `go.mod`: `go 1.25.0`.
- Dependencies: standard library + `github.com/shopspring/decimal` (v1.4.x). Nothing else, also not in tests.
- Money is never `float64`. All amounts are `decimal.Decimal`.

---

## 1. Packages and dependencies

| Package | Purpose |
|---|---|
| `cmd/paperless-extract` | `main`: subcommands, wiring, logging, graceful shutdown, build variables |
| `internal/config` | env parsing, `_FILE` secrets, defaults, validation |
| `internal/server` | HTTP handlers `/webhook`, `/healthz`, `/readyz`; doc ID extraction |
| `internal/queue` | bounded queue, worker pool, in-flight dedupe |
| `internal/paperless` | Paperless REST client, monetary formatting |
| `internal/ollama` | Ollama `/api/chat` + `/api/tags` client |
| `internal/extract` | amount parsing, currency mapping, prompt, budgeting, validation, `Extractor` |
| `internal/retry` | **Decision:** small own package: retry with exponential backoff, transient-error classification |
| `internal/pipeline` | **Decision:** own package (keeps `extract` free of Paperless): fetch → skip check → extract → write |
| `internal/eval` | **Decision:** own package: the `eval` subcommand (testable without `main`) |

Allowed imports (internal only; arrows = "imports"). No other internal imports, no cycles.

```
cmd/paperless-extract → config, server, queue, paperless, ollama, extract, retry, pipeline, eval
server   → queue                      (sentinel errors only)
pipeline → paperless, extract, retry
extract  → ollama, retry
eval     → extract, ollama, retry
config, paperless, ollama, queue, retry → (no internal imports)
```

`decimal` may be used by config, paperless, extract, pipeline, eval.

---

## 2. `internal/config`

```go
package config

// Sentinel errors, wrapped by *FieldError. Test with errors.Is.
var (
	ErrRequired = errors.New("required")          // variable missing or empty
	ErrInvalid  = errors.New("invalid value")     // unparseable or out of range
	ErrConflict = errors.New("both X and X_FILE set")
)

// FieldError describes one problem with one variable.
type FieldError struct {
	Var    string // env variable name, e.g. "OLLAMA_NUM_CTX" or "PAPERLESS_TOKEN_FILE"
	Err    error  // one of the sentinels above
	Detail string // human-readable detail; never contains a secret value
}

func (e *FieldError) Error() string // "config: <Var>: <Err>[: <Detail>]"
func (e *FieldError) Unwrap() error // returns Err

type Config struct {
	ListenAddr        string
	WebhookSecret     string
	PaperlessURL      string // trailing "/" removed
	PaperlessToken    string
	OllamaURL         string // trailing "/" removed
	OllamaModel       string
	OllamaNumCtx      int
	OllamaNumPredict  int
	OllamaTimeout     time.Duration
	OllamaKeepAlive   time.Duration // may be negative (= keep loaded forever) or 0 (= unload)
	CustomFieldName   string
	DefaultCurrency   string // 3 upper-case letters
	MaxAmount         decimal.Decimal
	OverwriteExisting bool
	QueueSize         int
	Workers           int
	PromptFile        string // "" = built-in template
	LogLevel          slog.Level
	ShutdownTimeout   time.Duration
}

// Load reads configuration through getenv (os.Getenv in production) and
// readFile (os.ReadFile) for *_FILE variables. It never touches the real
// environment or filesystem itself.
//
// It validates everything and reports ALL problems at once: the returned
// error is errors.Join of *FieldError values (one per offending variable),
// so errors.Is(err, ErrRequired) etc. and strings.Contains(err.Error(), "<VAR>")
// both work. On error the returned Config is the zero value.
func Load(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error)

// LogValue implements slog.LogValuer: all fields, but WebhookSecret and
// PaperlessToken rendered as "[redacted]" (empty stays empty).
func (c Config) LogValue() slog.Value
```

Rules for `Load`:

- Every value is `strings.TrimSpace`d. Empty after trimming = unset = default applies.
- `_FILE` variants exist for `WEBHOOK_SECRET` and `PAPERLESS_TOKEN` only.
  - `X_FILE` set → `readFile(path)`; read error → `FieldError{Var:"X_FILE", Err:ErrInvalid}`. Content is `TrimSpace`d; empty content → `FieldError{Var:"X", Err:ErrRequired}`.
  - Both `X` and `X_FILE` set → `FieldError{Var:"X", Err:ErrConflict}`.

| Variable | Default | Validation (failure → `ErrInvalid` unless noted) |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | non-empty; `net.SplitHostPort` must succeed |
| `WEBHOOK_SECRET` | — | missing → `ErrRequired`; **Decision:** length < 16 bytes → `ErrInvalid` |
| `PAPERLESS_URL` | — | missing → `ErrRequired`; scheme `http`/`https`, non-empty host, no query/fragment, no userinfo (`user:pass@` → `ErrInvalid`, since URLs are logged); path allowed (sub-path deployments); trailing `/` stripped |
| `PAPERLESS_TOKEN` | — | missing → `ErrRequired` |
| `OLLAMA_URL` | `http://ollama:11434` | as `PAPERLESS_URL` |
| `OLLAMA_MODEL` | — | missing → `ErrRequired` |
| `OLLAMA_NUM_CTX` | `65536` | integer ≥ 1024 |
| `OLLAMA_NUM_PREDICT` | `256` | integer ≥ 1 and < `OLLAMA_NUM_CTX` |
| `OLLAMA_TIMEOUT` | `5m` | `time.ParseDuration`, > 0 |
| `OLLAMA_KEEP_ALIVE` | `5m` | `time.ParseDuration`, or a bare integer = seconds (`"-1"` → `-1s`, `"300"` → `5m`) |
| `CUSTOM_FIELD_NAME` | `Total amount` | — |
| `DEFAULT_CURRENCY` | `EUR` | upper-cased, must match `^[A-Z]{3}$` |
| `MAX_AMOUNT` | `1000000` | `decimal.NewFromString`, > 0 |
| `OVERWRITE_EXISTING` | `false` | `strconv.ParseBool` |
| `QUEUE_SIZE` | `100` | integer ≥ 1 |
| `WORKERS` | `1` | integer ≥ 1 |
| `PROMPT_FILE` | `""` | not read here (main loads it and fails fast) |
| `LOG_LEVEL` | `info` | case-insensitive `debug`/`info`/`warn`/`error` |
| `SHUTDOWN_TIMEOUT` | `25s` | **Decision (new variable):** `time.ParseDuration`, > 0 |

---

## 3. `internal/server`

```go
package server

// MaxBodyBytes limits the webhook request body.
const MaxBodyBytes = 64 << 10

// ErrNoDocID is returned (wrapped) by ParseDocID.
var ErrNoDocID = errors.New("no document id in doc_url")

// Enqueuer is satisfied by *queue.Queue.
type Enqueuer interface {
	Enqueue(docID int) error
}

// Check is one readiness probe; nil = healthy.
type Check func(ctx context.Context) error

type Options struct {
	Secret      string            // expected X-Webhook-Secret value
	Queue       Enqueuer
	Ready       map[string]Check  // e.g. {"paperless": ..., "ollama": ...}
	ReadyTimeout time.Duration    // total budget for all checks; 0 → 5s
	Logger      *slog.Logger      // nil → discard
}

// New returns the HTTP handler (a *http.ServeMux) with routes
//   POST /webhook, GET /healthz, GET /readyz.
// Other methods on these paths → 405 (ServeMux behaviour), unknown paths → 404.
func New(opts Options) http.Handler

// ParseDocID extracts the Paperless document ID from a doc_url.
func ParseDocID(docURL string) (int, error)
```

### ParseDocID

Trim whitespace; parse with `url.Parse` (absolute URL or bare path); split the
path on `/` ignoring empty segments; find the **last** segment equal to `documents`
that is followed by a segment consisting only of ASCII digits; that segment must
parse to an `int` > 0. Query and fragment are ignored. Anything else → error
wrapping `ErrNoDocID`.

| Input | Result |
|---|---|
| `http://paperless:8000/documents/123/` | 123 |
| `https://p.example.com/documents/42` | 42 |
| `https://p.example.com/documents/42/details` | 42 |
| `https://p.example.com/sub/api/documents/7/?x=1#f` | 7 |
| `/documents/5/` | 5 |
| `  https://h/documents/3/  ` | 3 |
| `""`, `https://h/documents/`, `https://h/` | error |
| `https://h/documents/abc/`, `https://h/documents/+5/`, `https://h/documents/-1/` | error |
| `https://h/documents/0/` | error |
| `https://h/documents/99999999999999999999/` (overflow) | error |
| `{{doc_url}}` (placeholder not rendered) | error |

### POST /webhook

Processing order; all responses `Content-Type: application/json`:

1. Secret: compare `r.Header.Get("X-Webhook-Secret")` with `Options.Secret` in
   constant time: `subtle.ConstantTimeCompare(sha256(got), sha256(want)) == 1`
   (hashing removes the length leak). Missing or wrong → **401** `{"error":"unauthorized"}`.
   Log only `warn "webhook unauthorized"` with `remote_addr`; never the received value.
2. Body: `http.MaxBytesReader(MaxBodyBytes)`, decode one JSON object
   `{"doc_url": string, ...}` (other fields ignored, Content-Type not enforced,
   trailing data ignored). Empty, too large, not JSON, not an object →
   **400** `{"error":"invalid body"}`.
3. `ParseDocID(doc_url)` fails (incl. missing `doc_url`) → **400** `{"error":"no document id"}`.
4. `Enqueue(id)`:
   - `nil` → **202** `{"status":"queued","doc_id":<id>}`, info log `webhook accepted`.
   - `queue.ErrDuplicate` → **202** `{"status":"duplicate","doc_id":<id>}`.
   - `queue.ErrFull` → **503** `{"error":"queue full"}`, warn log.
   - `queue.ErrClosed` → **503** `{"error":"shutting down"}`.
   - other → **500** `{"error":"internal error"}`.

### GET /healthz

Always **200** `{"status":"ok"}` (liveness: process serves HTTP).

### GET /readyz

Runs all `Ready` checks concurrently with a shared context of `ReadyTimeout`.
All nil → **200** `{"status":"ok","checks":{"paperless":"ok","ollama":"ok"}}`.
Any error → **503** `{"status":"unavailable","checks":{"paperless":"ok","ollama":"<err.Error()>"}}`.
Empty `Ready` map → 200 with `"checks":{}`.

---

## 4. `internal/queue`

```go
package queue

var (
	ErrFull      = errors.New("queue full")
	ErrDuplicate = errors.New("document already queued")
	ErrClosed    = errors.New("queue closed")
)

// Handler processes one document. It must honour ctx cancellation.
type Handler func(ctx context.Context, docID int)

type Queue struct{ /* unexported */ }

// New creates a queue holding up to size waiting jobs, processed by
// `workers` goroutines. size and workers < 1 are treated as 1.
// nil logger → discard.
func New(size, workers int, h Handler, logger *slog.Logger) *Queue

// Start launches the workers. Handlers receive a context derived from ctx.
// Calling Start more than once is a no-op. Jobs enqueued before Start wait.
func (q *Queue) Start(ctx context.Context)

// Enqueue adds docID. Checks in this order:
//   1. Shutdown called           → ErrClosed
//   2. docID in flight           → ErrDuplicate (no second job)
//   3. size waiting jobs present → ErrFull (docID NOT marked in flight)
// Otherwise nil. Never blocks.
// "In flight" = from successful Enqueue until its handler returns (waiting or
// running). After the handler returns (also after a panic) the same ID can be
// enqueued again.
func (q *Queue) Enqueue(docID int) error

// Len returns the number of waiting (not yet started) jobs.
func (q *Queue) Len() int

// InFlight returns the number of waiting + running jobs.
func (q *Queue) InFlight() int

// Shutdown stops accepting jobs (Enqueue → ErrClosed from now on), guarantees
// that no handler invocation STARTS after it was called, discards waiting jobs
// (warn log with their count), and waits for running handlers to return.
// If ctx expires first, it cancels the handlers' context and returns ctx.Err()
// without waiting further. Returns nil when all workers have exited.
// Safe to call multiple times and before Start (returns nil).
func (q *Queue) Shutdown(ctx context.Context) error
```

Capacity counts waiting jobs only; running jobs do not occupy a slot.
A panicking handler is recovered, logged at error with `doc_id`, and the worker continues.
**Decision:** discarded waiting jobs are lost (recovery via the phase-4 reconciliation sweep).

---

## 5. `internal/paperless`

```go
package paperless

var (
	ErrNotFound       = errors.New("paperless: not found")         // via StatusError 404
	ErrUnauthorized   = errors.New("paperless: unauthorized")      // via StatusError 401/403
	ErrFieldNotFound  = errors.New("paperless: custom field not found")
	ErrFieldWrongType = errors.New("paperless: custom field is not monetary")
)

// StatusError is returned for every non-2xx response.
type StatusError struct {
	Method     string
	URL        string // request URL (contains no credentials)
	StatusCode int
	Body       string // first 512 bytes of the response body
}

func (e *StatusError) Error() string
func (e *StatusError) HTTPStatus() int // == StatusCode; used by retry.IsTransient
// Unwrap: 404 → ErrNotFound, 401/403 → ErrUnauthorized, else nil.
func (e *StatusError) Unwrap() error

type Document struct {
	ID           int                   `json:"id"`
	Title        string                `json:"title"`
	Content      string                `json:"content"`
	CustomFields []CustomFieldInstance `json:"custom_fields"`
}

type CustomFieldInstance struct {
	Field int             `json:"field"`
	Value json.RawMessage `json:"value"` // kept verbatim for read-merge-write
}

// IsEmpty: true if Value is nil/empty, JSON null, or the JSON string "".
func (c CustomFieldInstance) IsEmpty() bool

type CustomField struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	DataType string `json:"data_type"` // "monetary", "string", ...
}

type Client struct{ /* unexported */ }

// NewClient: baseURL without trailing "/" (a trailing "/" is trimmed anyway).
// nil httpClient → &http.Client{Timeout: 30 * time.Second}.
func NewClient(baseURL, token string, httpClient *http.Client) *Client

// GetDocument: GET {base}/api/documents/{id}/. JSON null custom_fields → empty slice.
func (c *Client) GetDocument(ctx context.Context, id int) (*Document, error)

// ListCustomFields: GET {base}/api/custom_fields/?page_size=100, then follows
// `next` until null. The `next` URL's path+query is resolved against the
// configured base scheme+host (host in `next` is ignored — reverse-proxy safe).
// More than 50 pages → error.
func (c *Client) ListCustomFields(ctx context.Context) ([]CustomField, error)

// ResolveCustomField finds the field whose Name equals name exactly
// (case-sensitive; first match wins). None → ErrFieldNotFound;
// DataType != "monetary" → ErrFieldWrongType (wrapped, includes the actual type).
func (c *Client) ResolveCustomField(ctx context.Context, name string) (CustomField, error)

// SetCustomField does read-merge-write: GET the document (fresh), replace the
// value of the instance with Field == fieldID in place, or append
// {fieldID, value} at the end if absent; all other instances keep order and
// value; then PATCH {base}/api/documents/{docID}/ with {"custom_fields":[...]}.
// value is sent as a JSON string. Any 2xx is success; response body ignored.
func (c *Client) SetCustomField(ctx context.Context, docID, fieldID int, value string) error

// Ping: GET {base}/api/custom_fields/?page_size=1; 2xx → nil (checks reachability + token).
func (c *Client) Ping(ctx context.Context) error

// FormatMonetary returns currency + amount with exactly 2 decimals, rounding
// half away from zero (decimal.StringFixed(2)). ("EUR", 1234.5) → "EUR1234.50";
// ("CHF", 0.005) → "CHF0.01"; ("EUR", 1234) → "EUR1234.00".
func FormatMonetary(currency string, amount decimal.Decimal) string
```

### Wire format (all requests)

Headers: `Authorization: Token <token>`, `Accept: application/json`; PATCH adds
`Content-Type: application/json`. Response bodies are read up to 64 MiB.

`GET /api/documents/123/` → (other fields ignored)
```json
{ "id": 123, "title": "Rechnung 4711", "content": "…OCR text…",
  "custom_fields": [ {"field": 3, "value": "abc"}, {"field": 7, "value": null} ] }
```

`GET /api/custom_fields/?page_size=100`
```json
{ "count": 2, "next": "http://paperless:8000/api/custom_fields/?page=2&page_size=100", "previous": null,
  "results": [ {"id": 7, "name": "Total amount", "data_type": "monetary"} ] }
```

`PATCH /api/documents/123/` body (field 7 replaced in place):
```json
{ "custom_fields": [ {"field": 3, "value": "abc"}, {"field": 7, "value": "EUR1234.56"} ] }
```

---

## 6. `internal/ollama`

```go
package ollama

var ErrModelNotFound = errors.New("ollama: model not available")

// StatusError: non-2xx response. Message is the "error" field of the body if
// present, else the first 512 bytes of the body.
type StatusError struct {
	StatusCode int
	Message    string
}

func (e *StatusError) Error() string
func (e *StatusError) HTTPStatus() int

type Message struct {
	Role    string `json:"role"` // "system" | "user" | "assistant"
	Content string `json:"content"`
}

type Options struct {
	NumCtx      int     `json:"num_ctx"`
	Temperature float64 `json:"temperature"` // no omitempty: 0 must be sent
	NumPredict  int     `json:"num_predict"`
}

type ChatRequest struct {
	Model     string          `json:"model"`
	Messages  []Message       `json:"messages"`
	Stream    bool            `json:"stream"`           // client always sends false
	Format    json.RawMessage `json:"format,omitempty"` // JSON schema
	Options   Options         `json:"options"`
	KeepAlive string          `json:"keep_alive,omitempty"` // time.Duration.String(), e.g. "5m0s"
}

type ChatResponse struct {
	Model           string  `json:"model"`
	Message         Message `json:"message"`
	Done            bool    `json:"done"`
	DoneReason      string  `json:"done_reason"` // "stop" | "length" | ...
	PromptEvalCount int     `json:"prompt_eval_count"`
	EvalCount       int     `json:"eval_count"`
	TotalDuration   int64   `json:"total_duration"` // nanoseconds
}

type Client struct{ /* unexported */ }

// NewClient: timeout is applied per Chat call via context.WithTimeout
// (0 = none). nil httpClient → &http.Client{} (no client-level timeout).
func NewClient(baseURL string, timeout time.Duration, httpClient *http.Client) *Client

// Chat: POST {base}/api/chat with req (Stream forced to false),
// Content-Type: application/json. Non-2xx → *StatusError. Undecodable body →
// error (not transient). Response body read up to 4 MiB.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)

// CheckModel: GET {base}/api/tags. Returns nil if any entry's "name" or "model"
// equals model, where a model without ":" is compared as model+":latest".
// Not present → ErrModelNotFound. Transport/status errors returned as is.
func (c *Client) CheckModel(ctx context.Context, model string) error
```

Wire format. Request:
```json
{ "model": "qwen2.5:14b",
  "messages": [ {"role":"system","content":"<rendered prompt>"}, {"role":"user","content":"<fitted document text>"} ],
  "stream": false,
  "format": { "type":"object", "properties": { "found": {"type":"boolean"}, "amount_raw": {"type":"string","description":"Total exactly as printed"}, "currency": {"type":"string","description":"ISO 4217, e.g. EUR"}, "evidence": {"type":"string","description":"The line the amount was taken from"} }, "required": ["found"] },
  "options": { "num_ctx": 65536, "temperature": 0, "num_predict": 256 },
  "keep_alive": "5m0s" }
```
Response (extra fields ignored):
```json
{ "model":"qwen2.5:14b", "message": {"role":"assistant","content":"{\"found\":true,\"amount_raw\":\"1.234,56\",\"currency\":\"€\",\"evidence\":\"Gesamt 1.234,56 €\"}"},
  "done": true, "done_reason": "stop", "prompt_eval_count": 1834, "eval_count": 41, "total_duration": 5123456789 }
```
Error: any non-2xx with `{"error":"model \"x\" not found, try pulling it first"}`.
`GET /api/tags` → `{"models":[{"name":"qwen2.5:14b","model":"qwen2.5:14b"}]}`.

---

## 7. `internal/retry`

```go
package retry

type Policy struct {
	Attempts  int           // total attempts incl. the first; < 1 treated as 1
	BaseDelay time.Duration // delay before attempt 2; doubles each time
	MaxDelay  time.Duration // cap per delay; 0 = no cap
}

// Default: 3 attempts, delays 1s then 2s.
var Default = Policy{Attempts: 3, BaseDelay: time.Second, MaxDelay: 30 * time.Second}

// Do calls fn until it returns nil, a non-retryable error (retryable(err) ==
// false), or attempts are exhausted; returns fn's last error unchanged
// (errors.Is/As keep working). Delay before attempt n (n ≥ 2) =
// min(BaseDelay * 2^(n-2), MaxDelay); no jitter. If ctx is done before or
// during a delay, returns immediately with an error wrapping both ctx.Err()
// and the last fn error. retryable == nil → IsTransient.
func Do(ctx context.Context, p Policy, retryable func(error) bool, fn func(ctx context.Context) error) error

// IsTransient reports whether err is worth retrying:
//   nil → false; errors.Is(err, context.Canceled) → false;
//   errors.As(err, &x) with x interface{ HTTPStatus() int }: true iff status
//     is 408, 429 or ≥ 500 (decided here, before the checks below);
//   *url.Error, net.Error, context.DeadlineExceeded, io.ErrUnexpectedEOF → true;
//   everything else → false.
func IsTransient(err error) bool
```

**Decision:** clients never retry themselves. Retries are applied by the
callers: the pipeline around each Paperless call, the Extractor around each
Ollama `Chat` call. A per-request Ollama timeout counts as transient.

---

## 8. `internal/extract`

### 8.1 Amount parsing

```go
var ErrInvalidAmount = errors.New("invalid amount")

// ParseAmount parses a total as printed into a decimal. Errors wrap ErrInvalidAmount.
func ParseAmount(raw string) (decimal.Decimal, error)
```

Algorithm (in order):

1. **Affixes:** strip the maximal leading and trailing runs of runes that are
   letters (`unicode.IsLetter`), currency symbols (`unicode.Sc`), whitespace,
   `:` or `.` — except that a leading `.`/`,` immediately followed by a digit is
   kept (so `.50` stays invalid).
2. **Parentheses:** if the result is `(`…`)` → negative; take the inside and repeat step 1.
3. **Dash suffix:** a trailing `.` or `,` followed by one or more of `-` `–` `—`
   (e.g. `1234.-`, `50,–`, `1.234,--`) is removed; the amount has no decimals.
4. **Sign:** one leading `-`, `−` (U+2212) or `+`, or one trailing `-`/`−`; then
   trim whitespace/affixes again. More than one sign indicator (incl. with
   parentheses) → error.
5. **Core:** must consist only of digits and separators `.` `,` `'` `’` space
   U+00A0 U+202F U+2009, start and end with a digit, no two adjacent separators.
6. **Separator roles:**
   - Space-like and apostrophes are always thousands separators.
   - Both `.` and `,` present: the one occurring last is the decimal separator;
     it must occur exactly once; the other is a thousands separator.
   - Only one of `.`/`,`, occurring more than once: thousands separator.
   - Only one of `.`/`,`, occurring once (k = digits after it):
     - a space/apostrophe thousands separator is also present → decimal;
     - k ≠ 3 → decimal;
     - k = 3 → **Decision:** thousands separator if the integer part has 1–3
       digits and is not all zeros (`1.234` → 1234); otherwise decimal
       (`0,125` → 0.125, `1234,567` → 1234.567).
   - All thousands separators in one number must be the same rune, and none
     may appear after the decimal separator.
   - Thousands grouping: first group 1–3 digits, every further group exactly 3.
7. Build the decimal from the digits; apply the sign. More than 2 decimals is
   allowed here (rejected later by validation).

| Input | Output |
|---|---|
| `1.234,56` / `1,234.56` / `1234,56` / `1234.56` | 1234.56 |
| `1 234,56` / `1 234,56` / `1 234,56` | 1234.56 |
| `1'234.56` / `1’234.56` | 1234.56 |
| `€ 1.234,56` / `1.234,56 €` / `EUR1234.56` / `1.234,56 EUR.` | 1234.56 |
| `CHF 1'234.50` / `Fr. 1'234.50` | 1234.5 |
| `1234.-` / `1.234,-` / `1.234,--` | 1234 |
| `50,–` | 50 |
| `0,99` | 0.99 |
| `12,5` / `1,23` | 12.5 / 1.23 |
| `1234` | 1234 |
| `1.234` / `1,234` | 1234 (ambiguous → thousands) |
| `12.345` / `999.999` | 12345 / 999999 |
| `0,125` / `1234,567` / `123,4567` | 0.125 / 1234.567 / 123.4567 |
| `1 234.567` | 1234.567 |
| `1.234.567` / `1 234 567` | 1234567 |
| `1.234.567,89` / `1,234,567.89` / `1.234,5` | 1234567.89 / 1234567.89 / 1234.5 |
| `-1.234,56` / `1.234,56-` / `(1.234,56)` / `−12,00` / `- 12,00 EUR` | -1234.56 / -1234.56 / -1234.56 / -12 / -12 |
| `+12,00` | 12 |
| `""`, `"   "`, `€`, `abc`, `.50`, `--12`, `-(12)` | error |
| `1.23.45`, `12 50`, `1..234`, `1,234.567,89`, `1.234,56.78` | error |
| `1.234 567` (mixed thousands), `12,00 + 3,00` | error |

### 8.2 Currency

```go
// NormalizeCurrency maps a printed currency to ISO 4217.
// Key normalisation: TrimSpace, ToUpper, remove all '.' and whitespace.
//   "€","EUR","EURO","EUROS" → EUR   "$","US$","USD" → USD (Decision: $ = USD)
//   "£","GBP" → GBP                  "CHF","FR","SFR" → CHF
//   "ZŁ","ZL","PLN" → PLN            "KČ","KC","CZK" → CZK
//   any other ^[A-Z]{3}$ → itself.   Otherwise ("", "KR", "¥", "Dollar") → ("", false).
func NormalizeCurrency(raw string) (string, bool)

// CurrencyFromAmount removes digits, separators (. , ' ’ space-likes), signs,
// parentheses and dashes from raw and normalises the rest.
// "€ 1.234,56" → EUR,true; "Fr. 1'234.50" → CHF,true; "1.234,56" → "",false.
func CurrencyFromAmount(raw string) (string, bool)
```

### 8.3 Literal-occurrence check

```go
// AmountInText reports whether the amount in amountRaw occurs in text as a
// complete number, compared by VALUE (decimal separators are significant).
//   want = |ParseAmount(amountRaw)|; parse error → false.
//   Tokenise text into candidates: a candidate starts and ends with an ASCII
//   digit and has no ASCII digit directly before or after it (maximal run).
//   Inside a candidate, digits may be joined by exactly one separator rune:
//     . , ' ’            — always (if a digit follows), or
//     space, U+00A0, U+202F, U+2009 — only if followed by exactly three
//                          digits and then a non-digit or end of text.
//   Nothing else joins; in particular ';', '-', '/', ':' and two adjacent
//   separators end the candidate.
//   Only whole candidates are compared; no suffixes or sub-ranges
//   ("2 120,00" is the single candidate 2120.00).
//   Each candidate is parsed with ParseAmount (failures skipped).
//   True iff any candidate value Equals want.
func AmountInText(amountRaw, text string) bool
```

The absolute value is compared so that negative amounts reach the positivity
rule (`ErrNotPositive`) rather than failing here.

| amountRaw | text | Result |
|---|---|---|
| `1.234,56` | `Gesamt: 1.234,56 EUR` | true |
| `1234,56` | `Gesamt: 1.234,56 EUR` | true |
| `1.234,56` | `Summe 1 234,56` | true |
| `1 234,56` | `Summe 1.234,56` | true |
| `1.234,56 €` | `Total EUR1234.56` | true |
| `1'234.50` | `Total CHF 1'234.50` | true |
| `1234.-` | `Total 1234.- CHF` | true |
| `12,00` | `Betrag 12,00` | true |
| `12` | `Betrag 12,00` | true (equal value) |
| `120,00` | `Menge 2 120,00` | false (accepted false negative) |
| `234,56` | `Gesamt 1 234,56` | false (leading group dropped) |
| `567` | `Total 1 234 567` | false (leading groups dropped) |
| `234 567,00` | `1 234 567,00` | false (leading group dropped) |
| `-1.234,56` | `Gutschrift -1.234,56` | true (absolute value) |
| `1.234,56` | `Gesamt: 11.234,56` | false |
| `1.234,56` | `Gesamt: 1.234,567` | false |
| `234,56` | `Gesamt: 1.234,56` | false |
| `12.340,0` | `Gesamt 1.234,00` | false (decimal shifted) |
| `1.234` | `Gesamt 12,34` | false (decimal shifted) |
| `12,34` | `Rechnungsnr 1234` | false (decimal shifted) |
| `2.024,00` | `Datum 12.03.2024` | false |
| `12,34` | `12;34` | false (`;` is not a separator) |
| `€` | anything | false |

### 8.4 Prompt

```go
// DefaultPromptTemplate returns the embedded template (file
// internal/extract/prompt.tmpl, //go:embed).
func DefaultPromptTemplate() string

type PromptData struct {
	DefaultCurrency string
}

type Prompt struct{ /* unexported */ }

// ParsePrompt parses a text/template (Option "missingkey=error") and
// test-executes it with PromptData{DefaultCurrency: "EUR"}; parse/exec error
// or an empty (whitespace-only) result → error.
func ParsePrompt(tmpl string) (*Prompt, error)

// LoadPrompt: path == "" → ParsePrompt(DefaultPromptTemplate()); else read the
// file and ParsePrompt its content. Read errors are returned.
func LoadPrompt(path string) (*Prompt, error)

// System renders the system message.
func (p *Prompt) System(data PromptData) (string, error)
```

The template renders the **system** message only. **Decision:** the user message
is the fitted document text, verbatim. Default template content (engineer
copies it exactly into `prompt.tmpl`):

```
You extract the total amount from one invoice or quote. The user message contains the document text; it is OCR output and may contain recognition errors.

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
{"found": true, "amount_raw": "1.234,56", "currency": "EUR", "evidence": "Gesamtbetrag 1.234,56 EUR"}
```

### 8.5 Context budgeting

```go
const (
	ChatOverheadTokens = 128 // chat template, role markers
	MinTextBudget      = 256
	TruncationMarker   = "\n\n[... document truncated ...]\n\n"
	HeadPercent        = 30
)

// EstimateTokens = ceil(runeCount/3); "" → 0.
func EstimateTokens(s string) int

// TextBudget = numCtx - EstimateTokens(system) - numPredict - ChatOverheadTokens (may be negative).
func TextBudget(numCtx, numPredict int, system string) int

// FitText returns text unchanged (false) if EstimateTokens(text) <= budget.
// Otherwise (true): maxRunes = budget*3 - runeCount(TruncationMarker);
// maxRunes <= 0 → "". Else head = maxRunes*HeadPercent/100 runes from the
// start, tail = maxRunes-head runes from the end, result = head +
// TruncationMarker + tail. Rune-safe. Guarantees EstimateTokens(result) <= budget.
func FitText(text string, budget int) (string, bool)

// NearContextLimit: promptEvalCount >= numCtx*95/100 (integer arithmetic).
func NearContextLimit(promptEvalCount, numCtx int) bool
```

### 8.6 Result, validation

```go
// ResultSchema returns the JSON schema sent as "format" (exactly the PLAN schema, see §6).
func ResultSchema() json.RawMessage

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

	// Rejections: returned as fmt.Errorf("%w: %w: detail", ErrRejected, ErrX),
	// so both errors.Is(err, ErrRejected) and errors.Is(err, ErrX) hold.
	// Error strings never contain document text or model output.
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

// ParseResult parses the model's message content. Trims whitespace and one
// surrounding ``` / ```json fence. Not valid JSON object, or key "found"
// missing / not boolean → error wrapping ErrMalformedResponse. Unknown keys ignored.
func ParseResult(content string) (Result, error)

type Extraction struct {
	Amount          decimal.Decimal // > 0, ≤ 2 decimals
	Currency        string          // ISO 4217
	AmountRaw       string
	Evidence        string
	Truncated       bool // text was cut by FitText
	PromptEvalCount int
}

type Validator struct {
	MaxAmount       decimal.Decimal
	DefaultCurrency string
}

// Validate applies the rules in this order; the first failure is returned:
//   1. !r.Found                                    → ErrNotFound
//   2. ParseAmount(r.AmountRaw) fails              → ErrUnparseable
//   3. !AmountInText(r.AmountRaw, text)            → ErrNotInText
//   4. amount <= 0                                 → ErrNotPositive
//   5. amount >= MaxAmount                         → ErrTooLarge
//   6. amount has non-zero digits beyond 2 decimals→ ErrTooPrecise (Decision)
//   7. currency: r.Currency non-empty → NormalizeCurrency, failure → ErrCurrency;
//      else CurrencyFromAmount(r.AmountRaw); else DefaultCurrency.
// On success Amount is rounded to 2 places (exact, since rule 6 passed).
func (v Validator) Validate(r Result, text string) (Extraction, error)
```

### 8.7 Extractor

```go
// ChatClient is satisfied by *ollama.Client.
type ChatClient interface {
	Chat(ctx context.Context, req ollama.ChatRequest) (*ollama.ChatResponse, error)
}

type Extractor struct {
	Chat       ChatClient
	Prompt     *Prompt
	Model      string
	NumCtx     int
	NumPredict int
	KeepAlive  time.Duration
	Validator  Validator
	Retry      retry.Policy // zero value = single attempt
	Logger     *slog.Logger // nil → discard
}

// Extract runs one extraction on the document text.
func (e *Extractor) Extract(ctx context.Context, text string) (Extraction, error)
```

`Extract` steps:

1. `strings.TrimSpace(text) == ""` → `ErrEmptyText`.
2. `system := Prompt.System(PromptData{DefaultCurrency: Validator.DefaultCurrency})`.
3. `budget := TextBudget(NumCtx, NumPredict, system)`; `< MinTextBudget` → `ErrBudgetTooSmall`.
4. `fitted, truncated := FitText(text, budget)`; truncated → warn log `document truncated` (doc sizes only).
5. Request: messages `[system, user=fitted]`, `Format: ResultSchema()`,
   `Options{NumCtx, Temperature: 0, NumPredict}`, `KeepAlive: KeepAlive.String()`.
6. `retry.Do(ctx, Retry, retry.IsTransient, Chat)`; error → returned (wrapped).
7. `ParseResult(resp.Message.Content)`; if it fails **or** `resp.DoneReason == "length"`,
   repeat steps 6–7 **once** with the identical request. Second failure →
   `ErrMalformedResponse`. Validation failures are never retried.
8. `NearContextLimit(resp.PromptEvalCount, NumCtx)` → warn log and reject with
   `ErrRejected`+`ErrContextOverflow` (**Decision:** low confidence = not written).
9. `Validator.Validate(result, text)` — against the **full original** text.
   Fill `Truncated`, `PromptEvalCount`.

Debug logs (only): system prompt, fitted text, raw model content.

---

## 9. `internal/pipeline`

```go
package pipeline

// DocumentStore is satisfied by *paperless.Client.
type DocumentStore interface {
	GetDocument(ctx context.Context, id int) (*paperless.Document, error)
	SetCustomField(ctx context.Context, docID, fieldID int, value string) error
}

// AmountExtractor is satisfied by *extract.Extractor.
type AmountExtractor interface {
	Extract(ctx context.Context, text string) (extract.Extraction, error)
}

type Outcome string

const (
	OutcomeWritten  Outcome = "written"
	OutcomeSkipped  Outcome = "skipped"  // field already set, OverwriteExisting false
	OutcomeRejected Outcome = "rejected" // validation / not found / empty text / context overflow
	OutcomeFailed   Outcome = "failed"   // transport, malformed JSON, Paperless errors
)

type Pipeline struct {
	Store     DocumentStore
	Extractor AmountExtractor
	FieldID   int
	Overwrite bool
	Retry     retry.Policy // applied to GetDocument and SetCustomField; zero = single attempt
	Logger    *slog.Logger // nil → discard
}

// Process handles one document:
//  1. GetDocument via retry.Do(IsTransient). Error → (OutcomeFailed, err).
//  2. Instance with Field == FieldID exists and !IsEmpty() and !Overwrite →
//     (OutcomeSkipped, nil); Extractor is NOT called.
//  3. Extractor.Extract(doc.Content). errors.Is(err, extract.ErrRejected) or
//     extract.ErrEmptyText → (OutcomeRejected, err); other error → (OutcomeFailed, err).
//  4. SetCustomField(id, FieldID, paperless.FormatMonetary(ex.Currency, ex.Amount))
//     via retry.Do(IsTransient). Error → (OutcomeFailed, err); else (OutcomeWritten, nil).
// Nothing is written unless step 4 is reached.
func (p *Pipeline) Process(ctx context.Context, docID int) (Outcome, error)

// Handle adapts Process to queue.Handler and logs the outcome:
//   written  → info  "document processed" doc_id, outcome, currency, duration_ms (amount at debug)
//   skipped  → info  outcome, reason="field already set"
//   rejected → info  outcome, reason=err.Error()
//   failed   → error outcome, error=err.Error()
func (p *Pipeline) Handle(ctx context.Context, docID int)
```

---

## 10. `internal/eval` and the `eval` subcommand

```go
package eval

// Case is one line of the manifest (JSON Lines).
type Case struct {
	ID       string           `json:"id"`
	TextFile string           `json:"text_file"` // relative to the manifest's directory, or absolute
	Amount   *decimal.Decimal `json:"amount"`    // null/omitted = expected "not found" (e.g. credit note)
	Currency string           `json:"currency"`  // "" = not compared
	Text     string           `json:"-"`         // filled by LoadCases
}

// LoadCases reads the manifest. Blank lines and lines starting with "#" are
// skipped. Errors (with 1-based line number): invalid JSON, empty or duplicate
// id, empty text_file, unreadable text file. No cases → error.
func LoadCases(path string) ([]Case, error)

type Outcome string

const (
	Correct Outcome = "correct" // value matches, or expected none and nothing written
	Wrong   Outcome = "wrong"   // a value would be written but differs, or expected none
	Missed  Outcome = "missed"  // expected a value, extraction rejected
	Error   Outcome = "error"   // transport / malformed / other errors
)

// Score: err != nil && errors.Is(err, extract.ErrRejected) or ErrEmptyText →
// Correct if c.Amount == nil else Missed; other err → Error. err == nil:
// c.Amount == nil → Wrong; amounts decimal-Equal and (c.Currency == "" or
// equal to got.Currency) → Correct; else Wrong.
func Score(c Case, got extract.Extraction, err error) Outcome

type CaseResult struct {
	CaseID   string
	Model    string
	NumCtx   int
	Outcome  Outcome
	Got      extract.Extraction
	Err      error
	Duration time.Duration
}

type Summary struct {
	Model                                 string
	NumCtx                                int
	Cases, Correct, Wrong, Missed, Errors int
	Accuracy                              float64 // Correct / Cases, 0..1
	AvgDuration                           time.Duration
}

// Summarize groups by (Model, NumCtx) in order of first appearance.
func Summarize(results []CaseResult) []Summary

// WriteReport writes a text/tabwriter table with header
// MODEL NUM_CTX CASES CORRECT WRONG MISSED ERRORS ACCURACY AVG_TIME
// accuracy as "90.0%", avg time rounded to 0.1s (e.g. "12.3s").
func WriteReport(w io.Writer, s []Summary) error

// Run is the eval subcommand (args exclude "eval"). Returns the exit code:
// 0 = ran (any accuracy), 1 = runtime error (cases unreadable, prompt invalid),
// 2 = usage error (bad/missing flags).
func Run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int
```

Flags (`flag.NewFlagSet("eval", flag.ContinueOnError)`, output to stderr). Default = env var if set, else the value shown:

| Flag | Env fallback | Default |
|---|---|---|
| `-set` (required) | — | — |
| `-models` comma list (required if env unset) | `OLLAMA_MODEL` | — |
| `-num-ctx` comma list of ints | `OLLAMA_NUM_CTX` | `65536` |
| `-ollama-url` | `OLLAMA_URL` | `http://localhost:11434` (**Decision:** eval runs on the host) |
| `-num-predict` | `OLLAMA_NUM_PREDICT` | `256` |
| `-timeout` | `OLLAMA_TIMEOUT` | `5m` |
| `-keep-alive` | `OLLAMA_KEEP_ALIVE` | `5m` |
| `-prompt` | `PROMPT_FILE` | built-in |
| `-default-currency` | `DEFAULT_CURRENCY` | `EUR` |
| `-max-amount` | `MAX_AMOUNT` | `1000000` |
| `-v` | — | false: one line per case to stdout |

Runs cases sequentially: models outer loop, num-ctx inner, in flag order, using
`extract.Extractor` with `retry.Default`. `-v` line format:
`case=<id> model=<m> num_ctx=<n> outcome=<o> expected=<EUR1234.56|none> got=<EUR1234.56|none> duration=<1.2s> [reason=<err>]`.
Then the report table to stdout. Manifest example (`cases.jsonl`):

```
{"id":"inv-001","text_file":"texts/inv-001.txt","amount":"1234.56","currency":"EUR"}
{"id":"cn-001","text_file":"texts/cn-001.txt","amount":null}
```

---

## 11. `cmd/paperless-extract`

Build variables (set via ldflags, defaults in source):

```go
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)
```

Subcommands (`os.Args[1]`):

| Command | Behaviour | Exit |
|---|---|---|
| none or `serve` | run the service | 0 clean shutdown, 1 config/startup error |
| `eval …` | `eval.Run(ctx, os.Args[2:], os.Getenv, os.Stdout, os.Stderr)` | from Run |
| `healthcheck` | **Decision:** GET `http://127.0.0.1:<port of LISTEN_ADDR>/healthz`, timeout 3s (for distroless `HEALTHCHECK`) | 0 if 200, else 1 |
| `version` | prints `paperless-extract <version> (commit <commit>, built <date>)` | 0 |
| other | usage to stderr | 2 |

Serve startup:

1. `config.Load(os.Getenv, os.ReadFile)`; error → print to stderr, exit 1.
2. Logger: `slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))`, set as default.
3. Info log `starting` with `version`, `commit`, `date`, `go_version`, `config` (`cfg` via LogValue).
4. `extract.LoadPrompt(cfg.PromptFile)`; error → exit 1.
5. Paperless client; `ResolveCustomField` wrapped in `retry.Do` with
   `Policy{Attempts: 10, BaseDelay: 2s, MaxDelay: 30s}` for transient errors
   (Paperless may still be starting). `ErrFieldNotFound`/`ErrFieldWrongType`/
   `ErrUnauthorized`/exhausted → error log, exit 1.
6. Ollama client; `CheckModel` once — failure is a warn log only (readyz reports it).
7. Extractor + Pipeline (`retry.Default`), `queue.New(cfg.QueueSize, cfg.Workers, pipeline.Handle, logger)`, `Start`.
8. `server.New` with `Ready{"paperless": pc.Ping, "ollama": func(ctx) error { return oc.CheckModel(ctx, cfg.OllamaModel) }}`.
9. `http.Server{Addr: cfg.ListenAddr, ReadHeaderTimeout: 5s, ReadTimeout: 10s, WriteTimeout: 30s, IdleTimeout: 60s}`.

Graceful shutdown (`signal.NotifyContext` on SIGINT, SIGTERM):

1. Info log `shutting down`.
2. `httpServer.Shutdown` with a 5s context (stop accepting webhooks).
3. `queue.Shutdown` with the remainder of `cfg.ShutdownTimeout` (measured from signal):
   running job finishes or is cancelled; waiting jobs are discarded.
4. Info log `stopped`; exit 0. A listen error (e.g. port in use) → exit 1.

---

## 12. Logging rules

- `log/slog` JSON handler to stdout; level from `LOG_LEVEL`.
- Standard attribute keys: `doc_id`, `outcome`, `reason`, `error`, `attempt`, `duration_ms`, `model`, `remote_addr`.
- **Only at debug:** document content, prompts, raw model output, `amount_raw`, `evidence`, extracted amount. Error strings at info/warn/error must not contain these (rejection errors are designed so).
- **Never, at any level:** `WEBHOOK_SECRET`, `PAPERLESS_TOKEN`, received `X-Webhook-Secret`, `Authorization` header.
- Retries: warn `retrying` with `attempt`, `error`.

---

## 13. Notes for the tester

Write black-box tests in external test packages (`package config_test`, …) using
only the exported API above, stdlib (`net/http/httptest`, `testing/synctest` is
available in Go 1.25) and `decimal`. Use `retry.Policy{Attempts: 3, BaseDelay: time.Millisecond}` to keep tests fast. Must-test:

- **config:** all defaults with only required vars set; each validation rule; all
  problems reported together; `_FILE` read + trim, conflict, unreadable file;
  `OLLAMA_KEEP_ALIVE` integer form; `LogValue` redacts secrets. Use secrets ≥ 16 chars.
- **server:** `ParseDocID` table; webhook 401 (missing/wrong secret, no enqueue
  happens), 400 (bad body, no ID, oversized body), 202 queued, 202 duplicate,
  503 full/closed, 500 other error; 405 on GET /webhook; `/healthz`; `/readyz`
  200/503 with check map. Use a fake `Enqueuer`.
- **queue:** dedupe while waiting and while running; ID reusable after handler
  returns; full (deterministic: enqueue before `Start`, `size` jobs fit, next →
  `ErrFull`, duplicate of a waiting ID while full → `ErrDuplicate`); `ErrClosed`
  after `Shutdown`; Shutdown waits for running handler; waiting jobs never
  start after Shutdown; Shutdown ctx expiry cancels handler ctx and returns
  ctx error; panic recovery. Run with `-race`.
- **paperless (httptest):** auth + accept headers; GetDocument decode, 404 →
  `ErrNotFound`, 401 → `ErrUnauthorized`; ListCustomFields pagination via
  `next` (with a different host in `next`); ResolveCustomField found / not
  found / wrong type; SetCustomField replace in place, append, other fields
  preserved (compare JSON semantically), null `custom_fields`; `FormatMonetary`.
- **ollama (httptest):** request body shape incl. `"stream":false`,
  `"temperature":0` present, `format`, `keep_alive`; response decode; non-2xx
  → `*StatusError` with message; per-call timeout; `CheckModel` incl. `:latest` rule.
- **retry:** attempt counts, no retry on non-transient, ctx cancel during
  delay, `IsTransient` table (StatusError 404/429/500/503, `*url.Error`,
  `context.Canceled`, plain error).
- **extract:** `ParseAmount` full table §8.1; `NormalizeCurrency`,
  `CurrencyFromAmount`; `AmountInText` table §8.3; `EstimateTokens`;
  `FitText` (unchanged when fits; head 30 %/tail 70 % + marker; result within
  budget; multi-byte runes not split); `NearContextLimit` boundary; `ParseResult`
  (fences, missing `found`); `Validate` each rule and order; `Extractor` with a
  fake `ChatClient`: one retry on malformed JSON then `ErrMalformedResponse`,
  no retry on validation failure, retry on transient error, context-overflow
  rejection, truncation flag, request fields; `ParsePrompt` errors; default
  template renders the default currency.
- **pipeline:** skip when field set (extractor not called), overwrite flag,
  rejected → no `SetCustomField`, written value `EUR1234.56`, transient GET
  error retried, 404 not retried → failed.
- **eval:** `LoadCases` (comments, null amount, duplicate id, relative paths),
  `Score` table, `Summarize`, `WriteReport` header, `Run` exit codes 2 (no
  `-set`) and 0 against an httptest Ollama.

## 14. Notes for DevOps

- Go **1.25** (`golang:1.25` builder), `CGO_ENABLED=0`, `go build -trimpath`.
- Main package `./cmd/paperless-extract`; binary in image at `/paperless-extract`; `ENTRYPOINT ["/paperless-extract"]` (default subcommand = serve).
- ldflags: `-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}`; Docker build args `VERSION` (default `dev`), `COMMIT`, `DATE` (RFC 3339 UTC); same values as OCI labels `org.opencontainers.image.{version,revision,created,source}`.
- Final image `gcr.io/distroless/static-debian12:nonroot` (uid/gid 65532). No shell: `HEALTHCHECK CMD ["/paperless-extract","healthcheck"]`.
- Listens on `:8080` (`EXPOSE 8080`); liveness `GET /healthz`, readiness `GET /readyz`. No port publishing needed in compose.
- Compose: `stop_grace_period: 30s` (service `SHUTDOWN_TIMEOUT` default 25s); secrets via `WEBHOOK_SECRET_FILE` / `PAPERLESS_TOKEN_FILE` and Docker secrets; optional prompt override mounted read-only, e.g. `/config/prompt.tmpl` with `PROMPT_FILE`.
- CI: `go vet ./...`, golangci-lint (v2 config), `go test -race ./...`, `docker build` without push. No Paperless/Ollama needed.
- `.gitignore` / `.dockerignore`: `.env`, `.env.*`, `*.env`, `evalset/` (local eval set directory, **Decision**), build output `bin/`, `dist/`.
- Dependabot: `gomod`, `github-actions`, `docker`.
