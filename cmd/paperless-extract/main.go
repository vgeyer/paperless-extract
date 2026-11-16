// Command paperless-extract receives Paperless-ngx webhooks and writes the
// total amount of invoices and quotes back as a custom field.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/vgeyer/paperless-extract/internal/config"
	"github.com/vgeyer/paperless-extract/internal/eval"
	"github.com/vgeyer/paperless-extract/internal/extract"
	"github.com/vgeyer/paperless-extract/internal/ollama"
	"github.com/vgeyer/paperless-extract/internal/paperless"
	"github.com/vgeyer/paperless-extract/internal/pipeline"
	"github.com/vgeyer/paperless-extract/internal/queue"
	"github.com/vgeyer/paperless-extract/internal/retry"
	"github.com/vgeyer/paperless-extract/internal/server"
)

// Set via -ldflags "-X main.version=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		os.Exit(serve())
	case "eval":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		code := eval.Run(ctx, os.Args[2:], os.Getenv, os.Stdout, os.Stderr)
		stop()
		os.Exit(code)
	case "healthcheck":
		os.Exit(healthcheck())
	case "version":
		fmt.Printf("paperless-extract %s (commit %s, built %s)\n", version, commit, date)
	default:
		fmt.Fprintf(os.Stderr, "usage: paperless-extract [serve|eval|healthcheck|version]\nunknown command %q\n", cmd)
		os.Exit(2)
	}
}

func healthcheck() int {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck: invalid LISTEN_ADDR")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Fixed loopback target, only the port comes from LISTEN_ADDR.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", port)+"/healthz", nil) //nolint:gosec // G704: loopback target, port from own config
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: loopback target built above
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}

func serve() int {
	cfg, err := config.Load(os.Getenv, os.ReadFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)
	logger.Info("starting", "version", version, "commit", commit, "date", date,
		"go_version", runtime.Version(), "config", cfg)

	prompt, err := extract.LoadPrompt(cfg.PromptFile)
	if err != nil {
		logger.Error("loading prompt failed", "error", err.Error())
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pc := paperless.NewClient(cfg.PaperlessURL, cfg.PaperlessToken, nil)
	var field paperless.CustomField
	startup := retry.Policy{Attempts: 10, BaseDelay: 2 * time.Second, MaxDelay: 30 * time.Second}
	attempt := 0
	err = retry.Do(ctx, startup, retry.IsTransient, func(ctx context.Context) error {
		attempt++
		var rerr error
		field, rerr = pc.ResolveCustomField(ctx, cfg.CustomFieldName)
		if rerr != nil && retry.IsTransient(rerr) {
			logger.Warn("retrying", "attempt", attempt, "error", rerr.Error())
		}
		return rerr
	})
	if err != nil {
		logger.Error("resolving custom field failed", "field", cfg.CustomFieldName, "error", err.Error())
		return 1
	}
	logger.Info("custom field resolved", "field", cfg.CustomFieldName, "field_id", field.ID)

	oc := ollama.NewClient(cfg.OllamaURL, cfg.OllamaTimeout, nil)
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if err := oc.CheckModel(checkCtx, cfg.OllamaModel); err != nil {
		logger.Warn("ollama model check failed", "model", cfg.OllamaModel, "error", err.Error())
	}
	cancel()

	extractor := &extract.Extractor{
		Chat: oc, Prompt: prompt, Model: cfg.OllamaModel,
		NumCtx: cfg.OllamaNumCtx, NumPredict: cfg.OllamaNumPredict, KeepAlive: cfg.OllamaKeepAlive,
		Validator: extract.Validator{MaxAmount: cfg.MaxAmount, DefaultCurrency: cfg.DefaultCurrency},
		Retry:     retry.Default, Logger: logger,
	}
	pl := &pipeline.Pipeline{
		Store: pc, Extractor: extractor, FieldID: field.ID,
		Overwrite: cfg.OverwriteExisting, Retry: retry.Default, Logger: logger,
	}
	q := queue.New(cfg.QueueSize, cfg.Workers, pl.Handle, logger)
	q.Start(context.Background())

	handler := server.New(server.Options{
		Secret: cfg.WebhookSecret,
		Queue:  q,
		Ready: map[string]server.Check{
			"paperless": pc.Ping,
			"ollama":    func(ctx context.Context) error { return oc.CheckModel(ctx, cfg.OllamaModel) },
		},
		Logger: logger,
	})
	srv := &http.Server{
		Addr: cfg.ListenAddr, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second,
	}

	lerr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.ListenAddr)
		lerr <- srv.ListenAndServe()
	}()

	select {
	case err := <-lerr:
		logger.Error("http server failed", "error", err.Error())
		sctx, c := context.WithTimeout(context.Background(), time.Second)
		_ = q.Shutdown(sctx)
		c()
		return 1
	case <-ctx.Done():
	}

	signaled := time.Now()
	logger.Info("shutting down")
	code := 0
	hctx, hcancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := srv.Shutdown(hctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Warn("http shutdown", "error", err.Error())
	}
	hcancel()

	remaining := max(cfg.ShutdownTimeout-time.Since(signaled), 0)
	qctx, qcancel := context.WithTimeout(context.Background(), remaining)
	if err := q.Shutdown(qctx); err != nil {
		logger.Warn("queue shutdown", "error", err.Error())
	}
	qcancel()

	logger.Info("stopped")
	return code
}
