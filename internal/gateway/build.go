package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/yj3335/llm-gateway/internal/auth"
	"github.com/yj3335/llm-gateway/internal/cache"
	"github.com/yj3335/llm-gateway/internal/config"
	"github.com/yj3335/llm-gateway/internal/guardrails"
	"github.com/yj3335/llm-gateway/internal/ratelimit"
	"github.com/yj3335/llm-gateway/internal/router"
	"github.com/yj3335/llm-gateway/internal/storage"
	"github.com/yj3335/llm-gateway/internal/telemetry"
)

// BuildOptions customizes Build.
type BuildOptions struct {
	Logger      *slog.Logger
	AuditWriter io.Writer    // defaults to stdout or cfg.Telemetry.AuditLogPath
	HTTPClient  *http.Client // upstream client
	SkipRedis   bool         // force in-memory rate limiting / exact cache
	SkipPG      bool         // force in-memory semantic cache / key store
}

// Build assembles a Gateway from configuration. Redis and PostgreSQL are
// optional at runtime: when unreachable the gateway logs a warning and runs
// with in-memory fallbacks so it degrades rather than fails.
func Build(ctx context.Context, cfg config.Config, o BuildOptions) (*Gateway, func(), error) {
	log := o.Logger
	if log == nil {
		log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	var closers []func()
	cleanup := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	backends := map[string]string{}

	// ---- Redis --------------------------------------------------------------
	var rdb *redis.Client
	redisUp := false
	if !o.SkipRedis && cfg.Redis.Addr != "" {
		rdb = redis.NewClient(&redis.Options{
			Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB,
			PoolSize: cfg.Redis.PoolSize, DialTimeout: cfg.Redis.DialTimeout,
			ReadTimeout: cfg.Redis.OpTimeout, WriteTimeout: cfg.Redis.OpTimeout,
			MinIdleConns: 16,
		})
		closers = append(closers, func() { _ = rdb.Close() })
		err := waitFor(ctx, cfg.Server.DependencyWait, log, "redis", func(ctx context.Context) error {
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			return rdb.Ping(pctx).Err()
		})
		if err != nil {
			log.Warn("redis unreachable: rate limiter will use local fallback until it recovers; exact cache is memory-only", "addr", cfg.Redis.Addr, "err", err)
		} else {
			redisUp = true
		}
	}

	// ---- PostgreSQL -----------------------------------------------------------
	var pool *pgxpool.Pool
	if !o.SkipPG && cfg.Postgres.DSN != "" {
		var p *pgxpool.Pool
		err := waitFor(ctx, cfg.Server.DependencyWait, log, "postgres", func(ctx context.Context) error {
			var cerr error
			p, cerr = storage.Connect(ctx, storage.Options{DSN: cfg.Postgres.DSN, MaxConns: cfg.Postgres.MaxConns, MinConns: cfg.Postgres.MinConns, Migrate: cfg.Postgres.RunMigrations, HNSWEfSearch: cfg.Postgres.HNSWEfSearch})
			return cerr
		})
		if err != nil {
			log.Warn("postgres unreachable: semantic cache and API keys fall back to memory", "err", err)
		} else {
			pool = p
			closers = append(closers, pool.Close)
		}
	}

	// ---- Telemetry ---------------------------------------------------------------
	metrics := telemetry.NewMetrics()
	collector := telemetry.NewCollector(cfg.Telemetry.SampleWindow)
	aw := o.AuditWriter
	if aw == nil {
		aw = os.Stdout
		if cfg.Telemetry.AuditLogPath != "" {
			f, err := os.OpenFile(cfg.Telemetry.AuditLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
			if err != nil {
				cleanup()
				return nil, nil, err
			}
			aw = f
			closers = append(closers, func() { _ = f.Close() })
		}
	}
	auditor := telemetry.NewAuditor(aw, pool)
	closers = append(closers, auditor.Close)

	// ---- Auth ----------------------------------------------------------------------
	defaults := auth.Defaults{RPM: cfg.Auth.DefaultRPM, TPM: cfg.Auth.DefaultTPM, PIIMode: cfg.Auth.DefaultPIIMode}
	vcfg := auth.VerifierConfig{Issuer: cfg.Auth.Issuer, Audience: cfg.Auth.Audience}
	if cfg.Auth.JWTHMACSecret != "" {
		vcfg.HMACSecret = []byte(cfg.Auth.JWTHMACSecret)
	}
	if cfg.Auth.JWTRSAPublicKey != "" {
		k, err := auth.ParseRSAPublicKeyPEM(cfg.Auth.JWTRSAPublicKey)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		vcfg.RSAPublicKey = k
	}
	if cfg.Auth.JWKSURL != "" {
		vcfg.JWKS = auth.NewJWKS(cfg.Auth.JWKSURL)
	}
	var verifier *auth.Verifier
	if len(vcfg.HMACSecret) > 0 || vcfg.RSAPublicKey != nil || vcfg.JWKS != nil {
		v, err := auth.NewVerifier(vcfg)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		verifier = v
	}
	static := auth.NewStaticKeyStore()
	for _, t := range cfg.Tenants {
		p := auth.Principal{TenantID: t.TenantID, Tier: t.Tier, RPMLimit: t.RPMLimit, TPMLimit: t.TPMLimit, PIIMode: auth.NormalizePIIMode(t.PIIMode, cfg.Auth.DefaultPIIMode)}
		if p.RPMLimit <= 0 {
			p.RPMLimit = defaults.RPM
		}
		if p.TPMLimit <= 0 {
			p.TPMLimit = defaults.TPM
		}
		if p.Tier == "" {
			p.Tier = "standard"
		}
		for _, k := range t.APIKeys {
			static.Add(k, p)
		}
		if pool != nil {
			if err := (&auth.PostgresKeyStore{Pool: pool}).UpsertTenant(ctx, p, t.APIKeys); err != nil {
				log.Warn("seed tenant failed", "tenant", t.TenantID, "err", err)
			}
		}
	}
	var keyStore auth.KeyStore = static
	backends["api_keys"] = "memory"
	if pool != nil {
		var l2 *redis.Client
		if redisUp {
			l2 = rdb
		}
		keyStore = auth.NewCachedKeyStore(auth.ChainKeyStore{&auth.PostgresKeyStore{Pool: pool}, static}, l2, cfg.Auth.APIKeyCacheTTL)
		backends["api_keys"] = "postgres+l1"
	}
	authenticator := &auth.Authenticator{JWT: verifier, Keys: keyStore, Defaults: defaults}

	// ---- Rate limiter ------------------------------------------------------------------
	var limiter ratelimit.Limiter
	if cfg.RateLimit.Enabled {
		if rdb != nil {
			rl := ratelimit.NewRedisLimiter(rdb, cfg.Redis.OpTimeout)
			limiter = rl
			backends["ratelimit"] = "redis-lua"
		} else {
			limiter = ratelimit.NewMemoryLimiter()
			backends["ratelimit"] = "memory"
		}
	}

	// ---- Cache ----------------------------------------------------------------------------
	var c *cache.Cache
	var embedderHealthy func() bool
	if cfg.Cache.Enabled {
		hashing := cache.NewHashingEmbedder()
		var emb cache.Embedder = hashing
		thresholds := map[string]float64{}
		switch cfg.Cache.Embedder {
		case "openai", "http":
			he := cache.NewHTTPEmbedder(cfg.Cache.EmbeddingURL, cfg.Cache.EmbeddingModel, os.Getenv(cfg.Cache.EmbeddingAPIKeyEnv))
			if cfg.Cache.EmbeddingDimensions > 0 {
				he.Dims = cfg.Cache.EmbeddingDimensions
			}
			emb = he
		case "ollama":
			// Neural embeddings from local Ollama; deterministic hashing takes
			// over while Ollama is unreachable. Each embedder has its own
			// similarity scale, hence its own threshold.
			oe := cache.NewOllamaEmbedder(cfg.Cache.EmbeddingURL, cfg.Cache.EmbeddingModel, cfg.Cache.EmbeddingDimensions, cfg.Cache.EmbeddingPrefix, cfg.Cache.EmbeddingTimeout)
			fb := cache.NewFallbackEmbedder(oe, hashing, 10*time.Second)
			fb.OnFallback = func(err error) {
				metrics.EmbedderFallbacks.Inc()
				if err != nil {
					log.Warn("embedder unavailable; using hashing fallback", "embedder", oe.Name(), "err", err)
				}
			}
			emb = fb
			embedderHealthy = fb.Healthy
			pctx, cancel := context.WithTimeout(ctx, 30*time.Second) // first call loads the model
			if _, err := oe.Embed(pctx, "warm-up"); err != nil {
				log.Warn("ollama embedder not reachable at startup; hashing fallback active until it is", "url", cfg.Cache.EmbeddingURL, "err", err)
			}
			cancel()
		}
		if cfg.Cache.FallbackSimilarityThreshold > 0 {
			thresholds[hashing.Name()] = cfg.Cache.FallbackSimilarityThreshold
		}
		thresholds[emb.Name()] = cfg.Cache.SimilarityThreshold
		var sem cache.SemanticStore = cache.NewMemoryVectorStore()
		if pool != nil {
			sem = &cache.PGVectorStore{Pool: pool}
		}
		var exact redis.UniversalClient
		if redisUp {
			exact = rdb
		}
		c = cache.New(cache.Options{
			SimilarityThreshold: cfg.Cache.SimilarityThreshold, Thresholds: thresholds, TTL: cfg.Cache.TTL, L1MaxEntries: cfg.Cache.L1MaxEntries,
			WriteQueueSize: cfg.Cache.WriteQueueSize, WriteWorkers: cfg.Cache.WriteWorkers,
		}, emb, sem, exact)
		c.OnError = func(op string, err error) {
			metrics.CacheBackendErrors.WithLabelValues(op).Inc()
			log.Debug("cache backend error", "op", op, "err", err)
		}
		closers = append(closers, c.Close)
		backends["cache_l1"] = "memory"
		if exact != nil {
			backends["cache_l1"] = "memory+redis"
		}
		backends["cache_l2"] = sem.Name()
		backends["embedder"] = emb.Name()
		if emb != cache.Embedder(hashing) {
			backends["embedder"] += " (fallback: " + hashing.Name() + ")"
		}
	}

	// ---- Router ----------------------------------------------------------------------------
	client := o.HTTPClient
	if client == nil {
		client = router.DefaultHTTPClient()
	}
	var providers []router.Provider
	for _, pc := range cfg.Providers {
		p, err := router.NewProviderFromEnv(router.ProviderSpec{
			Name: pc.Name, Type: pc.Type, BaseURL: pc.BaseURL, Timeout: pc.Timeout,
			DefaultModel: pc.DefaultModel, Query: pc.Query, Headers: pc.Headers, ExtraBody: pc.ExtraBody,
		}, pc.APIKeyEnv, client)
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		providers = append(providers, p)
		metrics.BreakerState.WithLabelValues(pc.Name).Set(0)
	}
	var routes []router.Route
	for _, rc := range cfg.Routes {
		rt := router.Route{Match: rc.Match}
		for _, t := range rc.Targets {
			rt.Targets = append(rt.Targets, router.Target{Provider: t.Provider, Model: t.Model})
		}
		routes = append(routes, rt)
	}
	onChange := func(provider string, from, to router.State) {
		metrics.BreakerState.WithLabelValues(provider).Set(telemetry.BreakerGaugeValue(to.String()))
		metrics.BreakerTransitions.WithLabelValues(provider, from.String(), to.String()).Inc()
		auditor.Event("circuit_breaker_transition", "provider", provider, "from", from.String(), "to", to.String())
	}
	rtr, err := router.New(providers, routes, router.BreakerConfig{
		FailureThreshold: cfg.Breaker.FailureThreshold, FailureRateWindow: cfg.Breaker.FailureRateWindow, MinRequests: cfg.Breaker.MinRequests,
		FailureRate: cfg.Breaker.FailureRate, OpenTimeout: cfg.Breaker.OpenTimeout,
		HalfOpenMaxRequests: cfg.Breaker.HalfOpenMaxRequests, HalfOpenSuccesses: cfg.Breaker.HalfOpenSuccesses,
	}, onChange)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	collector.SetProviderSource(func() any { return rtr.Health() })
	collector.SetBackends(backends)

	g := &Gateway{
		Cfg: cfg, Auth: authenticator, Limiter: limiter, Guard: guardrails.NewEngine(cfg.Guardrails.InjectionThreshold),
		Cache: c, Router: rtr, Metrics: metrics, Collector: collector, Audit: auditor,
	}
	g.Ready = func(ctx context.Context) map[string]string {
		out := map[string]string{}
		if rdb != nil {
			if err := rdb.Ping(ctx).Err(); err != nil {
				out["redis"] = "degraded: " + err.Error()
			} else {
				out["redis"] = "ok"
			}
		}
		if pool != nil {
			if err := pool.Ping(ctx); err != nil {
				out["postgres"] = "degraded: " + err.Error()
			} else {
				out["postgres"] = "ok"
			}
		}
		if embedderHealthy != nil {
			if embedderHealthy() {
				out["embedder"] = "ok"
			} else {
				out["embedder"] = "degraded: hashing fallback in use"
			}
		}
		return out
	}
	if cfg.Auth.DevTokenEndpoint && len(vcfg.HMACSecret) > 0 {
		g.tokenHandler = &auth.TokenHandler{
			Secret: vcfg.HMACSecret, AdminKey: cfg.Auth.AdminKey, Issuer: cfg.Auth.Issuer, Audience: cfg.Auth.Audience,
			MaxTTL: cfg.Auth.DevTokenTTL, Defaults: defaults,
		}
	}
	log.Info("gateway assembled", "backends", backends, "providers", len(providers), "routes", len(routes))
	return g, cleanup, nil
}

// waitFor retries fn every second until it succeeds or wait elapses, so the
// gateway tolerates dependencies that start after it (Kubernetes, Compose).
func waitFor(ctx context.Context, wait time.Duration, log *slog.Logger, name string, fn func(context.Context) error) error {
	deadline := time.Now().Add(wait)
	for attempt := 1; ; attempt++ {
		err := fn(ctx)
		if err == nil || time.Now().After(deadline) || ctx.Err() != nil {
			return err
		}
		if attempt == 1 {
			log.Info("waiting for dependency", "dependency", name, "max_wait", wait.String(), "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
