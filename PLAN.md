# paperless-extract — Plan

A small Go service, running in a Docker container, that receives a webhook from
Paperless-ngx, sends the document to a configurable Ollama model and writes the
extracted **total amount** back to the document.

## 1. Scope

In scope:

- Receive a Paperless-ngx workflow webhook for a single document.
- Extract one value: the total amount (plus currency) of an invoice or quote.
- Write the result back to Paperless as a custom field.
- Configurable Ollama model and context size (`num_ctx`).

Out of scope:

- Classification. Paperless decides whether a document is an invoice or quote
  (native matching/classifier) and only then fires the webhook. The service
  trusts that decision and does not re-classify.
- Line items, VAT breakdown, due dates, IBANs. The design should not block
  adding them later, but they are not built now.

## 2. Evaluation

The approach is sound and small. The points below are where it can go wrong
and decide most of the design.

| Topic | Finding | Consequence for the design |
|---|---|---|
| Webhook duration | Local inference takes seconds to minutes. Paperless should not wait on it. | Acknowledge with `202` immediately, process asynchronously. |
| Context overflow | Ollama truncates a prompt that exceeds `num_ctx` without returning an error. The total usually sits at the end of the document, i.e. exactly the part that gets lost. | Budget the prompt ourselves, truncate deliberately (keep head and tail), and check `prompt_eval_count` in the response. |
| Hallucinated numbers | Small models return plausible but invented amounts. | Structured output plus a hard check: the returned amount must literally occur in the document text. Otherwise the result is rejected. |
| Number formats | `1.234,56`, `1,234.56`, `1 234,56`, `1234.-` | The model returns the amount as printed; parsing and normalisation happen in Go with decimal arithmetic, never `float64`. |
| Ambiguity | Net vs. gross, deposits, partial invoices, quotes with optional positions or several variants. | "Total" is the gross amount payable (decided). Return "not found" rather than a guess. See open questions. |
| OCR quality | The service is only as good as the Paperless OCR text. | Text mode first. A vision mode (send page images) is a later option, not part of v1. |
| Lost events | An in-memory queue loses jobs on restart, and Paperless does not reliably retry failed webhooks. | Optional reconciliation sweep on startup (phase 4). |
| Update loops | Writing the custom field counts as a document update. | The Paperless workflow must use the *Document Added* trigger, not *Document Updated*. The service is additionally idempotent. |
| Custom field write | `PATCH` with `custom_fields` replaces the whole list. | Read existing fields, merge, then write. |

Alternatives considered:

- **Paperless-AI / paperless-gpt**: existing projects with broader scope
  (tagging, titles, chat). Worth a look before building. A custom service is
  justified if the goal is one narrow, verifiable extraction with full control
  over validation — which these tools do not focus on.
- **Polling instead of webhook**: simpler to make robust, but slower and needs
  its own state. Used here only as the reconciliation fallback.

## 3. Architecture

```
Paperless-ngx ──(workflow webhook)──► paperless-extract ──► Ollama /api/chat
      ▲                                     │
      └──── REST API: GET doc, PATCH custom field ────┘
```

Flow:

1. Paperless consumes a document, assigns the document type, and the workflow
   (trigger *Document Added*, filter on document type *Invoice* / *Quote*)
   posts to `POST /webhook`.
2. The service checks the shared secret, extracts the document ID, enqueues a
   job and returns `202`.
3. A worker fetches `GET /api/documents/{id}/` and uses the `content` field
   (OCR text).
4. If the target custom field already has a value, the job is skipped
   (idempotency), unless forced.
5. The text is fitted into the context budget and sent to Ollama.
6. The answer is validated and normalised.
7. The service writes the monetary custom field via `PATCH`. On failure
   nothing is written: an empty field is the signal. A saved view in Paperless
   (document type invoice/quote, field empty) lists the documents for manual
   handling.

### Webhook contract

Paperless lets the workflow define the body. Use a JSON body with placeholders:

```json
{ "doc_url": "{{doc_url}}", "title": "{{doc_title}}", "document_type": "{{document_type}}" }
```

The service takes the document ID from `doc_url` (`.../documents/<id>/`). Only
the ID is trusted; everything else is fetched through the API. The document
file is not attached to the webhook.

Authentication: a custom header `X-Webhook-Secret` configured in the workflow
action, compared in constant time.

### Ollama call

- `POST /api/chat`, `stream: false`.
- `format`: JSON schema (structured output).
- `options`: `num_ctx` from config, `temperature: 0`.
- `keep_alive` from config.

Schema:

```json
{
  "type": "object",
  "properties": {
    "found":      { "type": "boolean" },
    "amount_raw": { "type": "string", "description": "Total exactly as printed" },
    "currency":   { "type": "string", "description": "ISO 4217, e.g. EUR" },
    "evidence":   { "type": "string", "description": "The line the amount was taken from" }
  },
  "required": ["found"]
}
```

The prompt states:

- Gross total payable, including VAT.
- Quotes with several variants or optional positions: the main total, without
  optional positions.
- Credit notes: `found=false` (skipped).
- Copy amount and currency as printed.
- `found=false` if unsure.

The prompt lives in a template file that can be overridden by a mounted volume.

### Context budgeting

- Token estimate: characters / 3 (conservative for German text).
- Budget = `num_ctx` − system prompt − reserved output tokens.
- If the text is too long: keep the first ~30 % and the last ~70 % of the
  budget, with a marker in between. Totals are normally on the last page,
  sender and currency on the first.
- After the call: if `prompt_eval_count` is at or near `num_ctx`, log a warning
  and treat the result as low confidence.

With the planned 65k context (roughly 190,000 characters by the estimate
above), truncation will be rare for invoices and quotes. The logic stays in as
a safety net for long attachments such as terms and conditions.

### Validation

A result is accepted only if all of the following hold:

1. `found` is true and `amount_raw` parses to a decimal.
2. The digits of `amount_raw` occur in the document text (whitespace and
   thousands separators normalised).
3. The amount is greater than 0 and below a configurable sanity limit. Negative
   amounts (credit notes) are rejected.
4. The currency is taken as printed and mapped to an ISO code (`€` → `EUR`).
   The configured default is used only if the document names none.

Rejected results are not written. One retry with the same input is allowed for
malformed JSON only, not for validation failures.

### Write-back

- Custom field of type *Monetary*, value format `EUR1234.56`.
- The field is resolved by name on startup (`GET /api/custom_fields/`). The
  service fails fast if it does not exist.
- Existing custom fields of the document are preserved (read–merge–write).

## 4. Configuration

All via environment variables.

| Variable | Default | Purpose |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `WEBHOOK_SECRET` | — (required) | Shared secret for `X-Webhook-Secret` |
| `PAPERLESS_URL` | — (required) | Base URL of Paperless-ngx |
| `PAPERLESS_TOKEN` | — (required) | API token of a dedicated, low-privilege user |
| `OLLAMA_URL` | `http://ollama:11434` | Ollama base URL |
| `OLLAMA_MODEL` | — (required) | Model name, e.g. `qwen2.5:14b` |
| `OLLAMA_NUM_CTX` | `65536` | Context size |
| `OLLAMA_NUM_PREDICT` | `256` | Reserved output tokens |
| `OLLAMA_TIMEOUT` | `5m` | Per-request timeout |
| `OLLAMA_KEEP_ALIVE` | `5m` | How long the model stays loaded |
| `CUSTOM_FIELD_NAME` | `Total amount` | Target monetary custom field |
| `DEFAULT_CURRENCY` | `EUR` | Used when the document names no currency |
| `MAX_AMOUNT` | `1000000` | Sanity limit |
| `OVERWRITE_EXISTING` | `false` | Re-extract even if the field is set |
| `QUEUE_SIZE` | `100` | Bounded in-memory queue |
| `WORKERS` | `1` | Concurrent Ollama calls |
| `PROMPT_FILE` | built-in | Path to an override prompt template |
| `LOG_LEVEL` | `info` | `debug` logs model input/output |

Secrets also accept a `_FILE` variant for Docker secrets.

## 5. Project layout

```
cmd/paperless-extract/main.go   wiring, config, graceful shutdown
internal/config/                env parsing and validation
internal/server/                HTTP handlers: /webhook, /healthz, /readyz
internal/queue/                 bounded queue, worker pool, in-flight dedupe by doc ID
internal/paperless/             API client: get document, custom fields, patch
internal/ollama/                chat client with structured output
internal/extract/               prompt, context budgeting, validation, amount parsing
Dockerfile
compose.example.yaml
.github/workflows/ci.yaml       lint, vet, test, image build check
.github/workflows/release.yaml  build and push the image to GHCR
```

Standard library only where reasonable (`net/http`, `log/slog`, `encoding/json`).
One dependency for decimals (`shopspring/decimal`) or a small own parser. No
Ollama SDK needed for one endpoint.

## 6. Container

- Multi-stage build, `CGO_ENABLED=0`, final image `gcr.io/distroless/static`,
  non-root user.
- Built and published by GitHub Actions to the GitHub Container Registry
  (section 12); `compose.example.yaml` references that image.
- No persistent volume in v1.
- Joins the same Docker network as Paperless and Ollama; no published port
  needed.
- `/healthz` for liveness, `/readyz` checks that Paperless and Ollama are
  reachable and that the model is available.

Paperless side:

- `PAPERLESS_URL` must be set in Paperless, otherwise `doc_url` is not usable.
- Paperless restricts webhook targets (internal addresses, allowed ports).
  Check the `PAPERLESS_WEBHOOKS_*` settings of the deployed version.

## 7. Error handling

| Situation | Behaviour |
|---|---|
| Bad secret | `401`, nothing logged beyond the fact |
| Unparseable body / no ID | `400` |
| Queue full | `503` |
| Document already queued | `202`, no second job |
| Paperless or Ollama unreachable | Retry with backoff (3 attempts), then give up, error log |
| Model returns invalid JSON | One retry, then give up, error log |
| Validation fails / `found=false` | No write, info log with the reason |
| Shutdown | Stop accepting, finish the running job within a grace period |

Logging is structured (`slog`, JSON). Document content and model output are
logged at `debug` only, since invoices contain personal data.

## 8. Testing

- Unit tests: amount parsing (table-driven over DE/EN/CH formats), context
  truncation, validation, ID extraction from `doc_url`.
- Client tests against `httptest` servers for Paperless and Ollama.
- Evaluation set: 30–50 real invoices and quotes with known totals, run through
  a small CLI subcommand (`eval`) that prints accuracy per model and `num_ctx`.
  This is what model selection should be based on, not general benchmarks.
  The set stays local and is not committed.

## 9. Phases

1. **Skeleton**: config, HTTP server, webhook auth, queue, health endpoints,
   Dockerfile, GitHub repository with CI and image publishing (section 12).
2. **Pipeline**: Paperless client, Ollama client, prompt, budgeting,
   validation, write-back.
3. **Hardening**: retries, graceful shutdown, `eval` subcommand,
   model comparison on the evaluation set.
4. **Optional**: startup reconciliation (query documents of the relevant types
   with an empty custom field and enqueue them), Prometheus metrics, vision
   mode for poor OCR.

## 10. Decisions

1. Amount: gross amount payable, including VAT.
2. Quotes with several variants or optional positions: the main total.
3. Credit notes: skipped, nothing is written.
4. Currency: taken as printed on the document.
5. Hardware: AMD Radeon AI PRO R9700, context size 65k (`OLLAMA_NUM_CTX=65536`).
6. Failure signal: an empty field. No failure tag.

Still open: the model. The context window takes VRAM in addition to the model
weights, so the largest model that fits at 65k context has to be measured on
the card (`ollama ps` shows whether it runs fully on the GPU). Candidates are
then compared on the evaluation set (section 8).

## 11. To verify against the deployed Paperless-ngx version

These are assumptions from the documentation and need a quick check on the
actual instance before implementation:

- The *Document Added* trigger fires after automatic document-type assignment,
  so the type filter works on freshly consumed documents.
- Available webhook placeholders (`doc_url`, possibly a direct ID placeholder).
- The exact value format of monetary custom fields (`EUR1234.56`).
- The webhook action's behaviour on non-2xx responses (retries or not).

## 12. CI/CD on GitHub

The repository lives on GitHub. GitHub Actions tests the code and publishes the
container image to the GitHub Container Registry (GHCR) as
`ghcr.io/vgeyer/paperless-extract`.

### `ci.yaml` — every push and pull request

- `go vet`, `golangci-lint`, `go test -race ./...`
- `docker build` without push, so a broken Dockerfile fails the pull request.
- No job needs Paperless or Ollama: clients are tested against `httptest`
  servers. Model evaluation (`eval`) is not part of CI, because GitHub-hosted
  runners have no suitable GPU and the evaluation set stays local.

### `release.yaml` — push to `main` and tags `v*`

- Build with `docker/build-push-action` (Buildx), layer cache via the GitHub
  Actions cache.
- Login to GHCR with the workflow's `GITHUB_TOKEN`; the job needs
  `permissions: packages: write, contents: read`. No personal token or extra
  secret is required.
- Tags via `docker/metadata-action`:

  | Event | Image tags |
  |---|---|
  | Push to `main` | `edge`, `sha-<short>` |
  | Tag `v1.2.3` | `1.2.3`, `1.2`, `latest` |

- Platform `linux/amd64`. `linux/arm64` can be added cheaply (pure Go,
  cross-compiled in the build stage, no QEMU emulation of the compiler) if the
  service should ever run on an ARM host.
- Version, commit and build date are passed in as build arguments and exposed
  in the startup log and as OCI image labels.

### Repository rules

- Third-party actions are pinned to a commit SHA; Dependabot keeps actions,
  Go modules and the base image up to date.
- `main` is protected: changes go through pull requests with a green `ci` run.
- No secrets, tokens or real documents in the repository. `.gitignore` and
  `.dockerignore` exclude `.env` files and the evaluation set.

### Deployment

The host running Paperless pulls the image; there is no automatic deployment
from GitHub into the home/internal network. Updating is
`docker compose pull && docker compose up -d`, or a tool such as Watchtower or
Renovate if automatic updates are wanted. Production should pin a version tag
rather than `latest` or `edge`.

### Open

- Repository: `github.com/vgeyer/paperless-extract`, which is also the Go
  module path.
- Visibility: public (decided), for both repository and image. A GHCR package
  is private on first push and has to be set to public once in the package
  settings; after that the host pulls without login. A private image needs `docker login
  ghcr.io` with a read-only token on the host; a public one does not. The
  code contains nothing confidential by design, but the choice is yours.
