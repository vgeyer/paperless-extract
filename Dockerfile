# syntax=docker/dockerfile:1

# Build stage runs on the build host's native platform and cross-compiles
# (pure Go, no QEMU emulation of the compiler).
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/paperless-extract ./cmd/paperless-extract


FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

LABEL org.opencontainers.image.title="paperless-extract" \
      org.opencontainers.image.description="Extracts the total amount of invoices and quotes from Paperless-ngx documents via Ollama" \
      org.opencontainers.image.source="https://github.com/vgeyer/paperless-extract" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${DATE}"

COPY --from=build /out/paperless-extract /paperless-extract

USER 65532:65532
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD ["/paperless-extract", "healthcheck"]

ENTRYPOINT ["/paperless-extract"]
