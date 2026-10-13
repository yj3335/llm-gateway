// Command gateway runs the LLM Gateway HTTP/SSE server.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/yj3335/llm-gateway/internal/config"
	"github.com/yj3335/llm-gateway/internal/gateway"
)

func main() {
	cfgPath := flag.String("config", os.Getenv("LLMGW_CONFIG"), "path to YAML config")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("config", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	gw, cleanup, err := gateway.Build(ctx, cfg, gateway.BuildOptions{Logger: log})
	if err != nil {
		log.Error("build gateway", "err", err)
		os.Exit(1)
	}
	defer cleanup()

	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      gw.Handler(),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}
	go func() {
		log.Info("gateway listening", "addr", cfg.Server.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			stop()
		}
	}()
	<-ctx.Done()
	log.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer cancel()
	_ = srv.Shutdown(shutdown)
}
