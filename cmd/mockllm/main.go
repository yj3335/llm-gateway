// Command mockllm runs the mock upstream LLM server used for local
// development, chaos testing, and benchmarks.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yj3335/llm-gateway/internal/mockllm"
)

func main() {
	cfg := mockllm.DefaultConfig()
	addr := flag.String("addr", envOr("MOCKLLM_ADDR", ":8090"), "listen address")
	flag.DurationVar(&cfg.TTFT, "ttft", cfg.TTFT, "default time to first token")
	flag.DurationVar(&cfg.TPOT, "tpot", cfg.TPOT, "default time per output token")
	flag.IntVar(&cfg.Tokens, "tokens", cfg.Tokens, "default completion length in tokens")
	flag.Float64Var(&cfg.FailRate, "fail-rate", cfg.FailRate, "default probability of injected failure")
	flag.IntVar(&cfg.FailStatus, "fail-status", cfg.FailStatus, "status code for injected failures")
	flag.Float64Var(&cfg.Jitter, "jitter", cfg.Jitter, "TTFT jitter fraction")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	srv := &http.Server{Addr: *addr, Handler: mockllm.New(cfg), ReadHeaderTimeout: 5 * time.Second}

	go func() {
		log.Info("mockllm listening", "addr", *addr, "ttft", cfg.TTFT.String(), "tpot", cfg.TPOT.String(), "fail_rate", cfg.FailRate)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("listen", "err", err)
			os.Exit(1)
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
