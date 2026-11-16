# paperless-extract

A small Go service that receives a webhook from [Paperless-ngx](https://docs.paperless-ngx.com/),
asks a local [Ollama](https://ollama.com/) model for the **total amount** (gross, incl. VAT) of an
invoice or quote, validates the answer and writes it back to a monetary custom field.

- Paperless decides the document type; the service only extracts.
- The amount must literally occur in the OCR text, otherwise nothing is written.
- An empty custom field means "not extracted" — use a saved view to find those documents.

Image: `ghcr.io/vgeyer/paperless-extract`

## Configuration

All via environment variables. `WEBHOOK_SECRET` and `PAPERLESS_TOKEN` also accept a `_FILE`
variant (e.g. `PAPERLESS_TOKEN_FILE=/run/secrets/paperless_token`); setting both is an error.

| Variable | Default | Purpose |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | HTTP listen address |
| `WEBHOOK_SECRET` | required | Shared secret for the `X-Webhook-Secret` header, at least 16 bytes |
| `PAPERLESS_URL` | required | Base URL of Paperless-ngx (http/https, sub-path allowed) |
| `PAPERLESS_TOKEN` | required | API token of a dedicated, low-privilege user |
| `OLLAMA_URL` | `http://ollama:11434` | Ollama base URL |
| `OLLAMA_MODEL` | required | Model name, e.g. `qwen2.5:14b` |
| `OLLAMA_NUM_CTX` | `65536` | Context size (>= 1024) |
| `OLLAMA_NUM_PREDICT` | `256` | Reserved output tokens |
| `OLLAMA_TIMEOUT` | `5m` | Per-request timeout |
| `OLLAMA_KEEP_ALIVE` | `5m` | How long the model stays loaded (duration or seconds, `-1` = forever) |
| `CUSTOM_FIELD_NAME` | `Total amount` | Target custom field (type Monetary) |
| `DEFAULT_CURRENCY` | `EUR` | Used when the document names no currency |
| `MAX_AMOUNT` | `1000000` | Sanity limit |
| `OVERWRITE_EXISTING` | `false` | Re-extract even if the field is already set |
| `QUEUE_SIZE` | `100` | Bounded in-memory queue |
| `WORKERS` | `1` | Concurrent Ollama calls |
| `PROMPT_FILE` | built-in | Path to an override prompt template |
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error`; `debug` logs document content |
| `SHUTDOWN_TIMEOUT` | `25s` | Grace period for the running job on shutdown |

## Paperless setup

1. Set `PAPERLESS_URL` in the Paperless container, otherwise `{{doc_url}}` is not usable.
2. Allow the webhook target: check the `PAPERLESS_WEBHOOKS_*` settings of your Paperless version
   (internal hostnames and port 8080 may be blocked by default).
3. Create a custom field of type **Monetary** named `Total amount` (or your `CUSTOM_FIELD_NAME`).
   The service refuses to start without it.
4. Create a dedicated user with access to the relevant documents and an API token for it.
5. Create a workflow:
   - Trigger: **Document Added** — not *Document Updated*, writing the field is an update.
   - Filter: document type Invoice / Quote.
   - Action: Webhook to `http://paperless-extract:8080/webhook`, JSON body
     `{"doc_url": "{{doc_url}}", "title": "{{doc_title}}", "document_type": "{{document_type}}"}`,
     header `X-Webhook-Secret: <secret>`, document not attached.
6. Optional: a saved view "type Invoice/Quote, Total amount empty" for manual follow-up.

## Deployment

See [`compose.example.yaml`](compose.example.yaml). The service joins the network of Paperless and
Ollama; no port needs to be published. Pin a version tag in production (`edge` follows `main`):

```sh
docker compose pull && docker compose up -d
```

Endpoints: `POST /webhook`, `GET /healthz` (liveness), `GET /readyz` (Paperless and Ollama reachable,
model available). The image has a built-in `HEALTHCHECK` (`paperless-extract healthcheck`).

Image tags: push to `main` → `edge`, `sha-<short>`; tag `v1.2.3` → `1.2.3`, `1.2`, `latest`.

> The GHCR package is private after the first push. Set it to public once under
> *Package settings → Change visibility*; afterwards hosts pull without `docker login`.

## Development

Requires Go 1.25.

```sh
go vet ./...
go test -race ./...
golangci-lint run                 # v2
go build -o bin/paperless-extract ./cmd/paperless-extract

docker build \
  --build-arg VERSION=dev \
  --build-arg COMMIT=$(git rev-parse --short HEAD) \
  --build-arg DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  -t paperless-extract:dev .
```

Model evaluation runs on the host against a local evaluation set in `evalset/` (never committed):

```sh
go run ./cmd/paperless-extract eval -set evalset/cases.jsonl -models qwen2.5:14b,llama3.1:8b -v
```

Subcommands: `serve` (default), `eval`, `healthcheck`, `version`.
