// Package config loads gateway configuration from a YAML file with
// environment-variable overrides for deployment-specific values.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root gateway configuration.
type Config struct {
	Server     ServerConfig     `yaml:"server"`
	Auth       AuthConfig       `yaml:"auth"`
	Redis      RedisConfig      `yaml:"redis"`
	Postgres   PostgresConfig   `yaml:"postgres"`
	RateLimit  RateLimitConfig  `yaml:"ratelimit"`
	Guardrails GuardrailsConfig `yaml:"guardrails"`
	Cache      CacheConfig      `yaml:"cache"`
	Providers  []ProviderConfig `yaml:"providers"`
	Routes     []RouteConfig    `yaml:"routes"`
	Breaker    BreakerConfig    `yaml:"circuit_breaker"`
	Telemetry  TelemetryConfig  `yaml:"telemetry"`
	Pricing    map[string]Price `yaml:"pricing"`
	Tenants    []TenantSeed     `yaml:"tenants"`
}

type ServerConfig struct {
	Addr            string        `yaml:"addr"`
	ReadTimeout     time.Duration `yaml:"read_timeout"`
	WriteTimeout    time.Duration `yaml:"write_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	MaxBodyBytes    int64         `yaml:"max_body_bytes"`
	CORSOrigins     []string      `yaml:"cors_origins"`
	// EnableChaosHeaders lets clients request fault injection via
	// X-Gateway-Chaos (used by the playground & benchmarks). Off in prod.
	EnableChaosHeaders bool `yaml:"enable_chaos_headers"`
	// DependencyWait is how long startup retries Redis and PostgreSQL before
	// falling back to in-memory mode (containers may start in any order).
	DependencyWait time.Duration `yaml:"dependency_wait"`
}

type AuthConfig struct {
	JWTHMACSecret    string        `yaml:"jwt_hmac_secret"`
	JWTRSAPublicKey  string        `yaml:"jwt_rsa_public_key_pem"`
	JWKSURL          string        `yaml:"jwks_url"`
	Issuer           string        `yaml:"issuer"`
	Audience         string        `yaml:"audience"`
	DevTokenEndpoint bool          `yaml:"dev_token_endpoint"`
	AdminKey         string        `yaml:"admin_key"`
	APIKeyCacheTTL   time.Duration `yaml:"api_key_cache_ttl"`
	DefaultRPM       int64         `yaml:"default_rpm"`
	DefaultTPM       int64         `yaml:"default_tpm"`
	DefaultPIIMode   string        `yaml:"default_pii_mode"`
	DevTokenTTL      time.Duration `yaml:"dev_token_ttl"`
}

type RedisConfig struct {
	Addr        string        `yaml:"addr"`
	Password    string        `yaml:"password"`
	DB          int           `yaml:"db"`
	PoolSize    int           `yaml:"pool_size"`
	DialTimeout time.Duration `yaml:"dial_timeout"`
	OpTimeout   time.Duration `yaml:"op_timeout"`
}

type PostgresConfig struct {
	DSN           string `yaml:"dsn"`
	MaxConns      int32  `yaml:"max_conns"`
	MinConns      int32  `yaml:"min_conns"`
	RunMigrations bool   `yaml:"run_migrations"`
	HNSWEfSearch  int    `yaml:"hnsw_ef_search"`
}

type RateLimitConfig struct {
	Enabled            bool          `yaml:"enabled"`
	DefaultMaxTokens   int64         `yaml:"default_max_tokens"`
	FallbackRetryAfter time.Duration `yaml:"fallback_retry_after"`
}

type GuardrailsConfig struct {
	Enabled            bool    `yaml:"enabled"`
	InjectionThreshold float64 `yaml:"injection_threshold"`
	ScanOutput         bool    `yaml:"scan_output"`
}

type CacheConfig struct {
	Enabled             bool          `yaml:"enabled"`
	SimilarityThreshold float64       `yaml:"similarity_threshold"`
	TTL                 time.Duration `yaml:"ttl"`
	L1MaxEntries        int           `yaml:"l1_max_entries"`
	// Embedder: "hashing" (local, deterministic), "ollama" (e.g. nomic-embed-text
	// via /api/embeddings, falls back to hashing when Ollama is unreachable),
	// or "openai" (OpenAI-compatible /v1/embeddings).
	Embedder           string `yaml:"embedder"`
	EmbeddingURL       string `yaml:"embedding_url"`
	EmbeddingModel     string `yaml:"embedding_model"`
	EmbeddingAPIKeyEnv string `yaml:"embedding_api_key_env"`
	// EmbeddingDimensions truncates (Matryoshka) and re-normalizes vectors;
	// 0 keeps the model's native size (768 for nomic-embed-text).
	EmbeddingDimensions int `yaml:"embedding_dimensions"`
	// EmbeddingPrefix is prepended to every text (some models use task prefixes).
	EmbeddingPrefix string `yaml:"embedding_prefix"`
	// EmbeddingTimeout is the request-path budget for embedding a prompt (and
	// the connect timeout for detecting an offline embedder). Past it the
	// semantic lookup is skipped for that request; background cache writes
	// still embed with a longer deadline.
	EmbeddingTimeout time.Duration `yaml:"embedding_timeout"`
	// FallbackSimilarityThreshold applies to entries embedded by the hashing
	// fallback; SimilarityThreshold applies to the configured embedder.
	FallbackSimilarityThreshold float64 `yaml:"fallback_similarity_threshold"`
	WriteQueueSize              int     `yaml:"write_queue_size"`
	WriteWorkers                int     `yaml:"write_workers"`
}

type ProviderConfig struct {
	Name         string            `yaml:"name"`
	Type         string            `yaml:"type"` // openai | anthropic | gemini | ollama | mockllm
	BaseURL      string            `yaml:"base_url"`
	APIKeyEnv    string            `yaml:"api_key_env"`
	Timeout      time.Duration     `yaml:"timeout"`
	DefaultModel string            `yaml:"default_model"`
	Query        map[string]string `yaml:"query"`
	Headers      map[string]string `yaml:"headers"`
	// ExtraBody fields are added to OpenAI-compatible request bodies unless the
	// request sets them, e.g. {reasoning_effort: none} to disable "thinking".
	ExtraBody map[string]any `yaml:"extra_body"`
	// FaultInjectionRate (chaos testing) fails this fraction of calls to the
	// provider with a synthetic 503 before they are sent. Keep 0 in production.
	FaultInjectionRate float64 `yaml:"fault_injection_rate"`
}

type RouteTarget struct {
	Provider string `yaml:"provider"`
	Model    string `yaml:"model"`
}

type RouteConfig struct {
	// Match is a model name or prefix pattern ending in '*'; "*" matches all.
	Match   string        `yaml:"match"`
	Targets []RouteTarget `yaml:"targets"`
}

type BreakerConfig struct {
	FailureThreshold    int           `yaml:"failure_threshold"` // consecutive failures; 0 disables
	MinRequests         int           `yaml:"min_requests"`
	FailureRateWindow   int           `yaml:"failure_rate_window"`
	FailureRate         float64       `yaml:"failure_rate"`
	OpenTimeout         time.Duration `yaml:"open_timeout"`
	HalfOpenMaxRequests int           `yaml:"half_open_max_requests"`
	HalfOpenSuccesses   int           `yaml:"half_open_successes"`
}

type TelemetryConfig struct {
	AuditLogPath   string        `yaml:"audit_log_path"`
	StreamInterval time.Duration `yaml:"stream_interval"`
	SampleWindow   int           `yaml:"sample_window"`
}

// Price is USD per 1K tokens.
type Price struct {
	Input  float64 `yaml:"input"`
	Output float64 `yaml:"output"`
}

type TenantSeed struct {
	TenantID string   `yaml:"tenant_id"`
	Tier     string   `yaml:"tier"`
	RPMLimit int64    `yaml:"rpm_limit"`
	TPMLimit int64    `yaml:"tpm_limit"`
	PIIMode  string   `yaml:"pii_mode"`
	APIKeys  []string `yaml:"api_keys"`
}

// Default returns a configuration suitable for local development against
// the bundled mock upstream.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Addr:            ":8080",
			ReadTimeout:     30 * time.Second,
			WriteTimeout:    120 * time.Second,
			ShutdownTimeout: 10 * time.Second,
			DependencyWait:  30 * time.Second,
			MaxBodyBytes:    1 << 20,
			CORSOrigins:     []string{"http://localhost:3000", "http://127.0.0.1:3000"},
		},
		Auth: AuthConfig{
			JWTHMACSecret:  "dev-insecure-change-me",
			Issuer:         "llm-gateway",
			Audience:       "llm-gateway",
			APIKeyCacheTTL: 60 * time.Second,
			DefaultRPM:     600,
			DefaultTPM:     200000,
			DefaultPIIMode: "REDACT",
			DevTokenTTL:    12 * time.Hour,
		},
		Redis: RedisConfig{
			Addr:        "localhost:6379",
			PoolSize:    256,
			DialTimeout: 500 * time.Millisecond,
			OpTimeout:   250 * time.Millisecond,
		},
		Postgres: PostgresConfig{
			DSN:           "postgres://llmgw@localhost:5432/llmgateway?sslmode=disable",
			MaxConns:      64,
			MinConns:      32,
			RunMigrations: true,
			HNSWEfSearch:  40,
		},
		RateLimit: RateLimitConfig{
			Enabled:            true,
			DefaultMaxTokens:   256,
			FallbackRetryAfter: 2 * time.Second,
		},
		Guardrails: GuardrailsConfig{Enabled: true, InjectionThreshold: 0.7, ScanOutput: true},
		Cache: CacheConfig{
			Enabled:                     true,
			SimilarityThreshold:         0.86,
			TTL:                         24 * time.Hour,
			L1MaxEntries:                50000,
			Embedder:                    "hashing",
			EmbeddingModel:              "text-embedding-3-small",
			EmbeddingAPIKeyEnv:          "OPENAI_API_KEY",
			EmbeddingTimeout:            2 * time.Second,
			FallbackSimilarityThreshold: 0.86,
			WriteQueueSize:              4096,
			WriteWorkers:                8,
		},
		Breaker: BreakerConfig{
			FailureThreshold:    0,
			MinRequests:         10,
			FailureRateWindow:   20,
			FailureRate:         0.5,
			OpenTimeout:         5 * time.Second,
			HalfOpenMaxRequests: 2,
			HalfOpenSuccesses:   2,
		},
		Telemetry: TelemetryConfig{StreamInterval: time.Second, SampleWindow: 10000},
		Pricing: map[string]Price{
			"default": {Input: 0.0025, Output: 0.01},
		},
	}
}

// Load reads YAML from path (if non-empty) on top of Default(), then applies
// environment overrides.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(raw))), &cfg); err != nil {
			return cfg, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(&cfg)
	return cfg, cfg.Validate()
}

func applyEnv(cfg *Config) {
	str := func(key string, dst *string) {
		if v, ok := os.LookupEnv(key); ok {
			*dst = v
		}
	}
	boolean := func(key string, dst *bool) {
		if v, ok := os.LookupEnv(key); ok {
			if b, err := strconv.ParseBool(v); err == nil {
				*dst = b
			}
		}
	}
	str("LLMGW_ADDR", &cfg.Server.Addr)
	str("LLMGW_REDIS_ADDR", &cfg.Redis.Addr)
	str("LLMGW_REDIS_PASSWORD", &cfg.Redis.Password)
	str("LLMGW_POSTGRES_DSN", &cfg.Postgres.DSN)
	str("LLMGW_JWT_SECRET", &cfg.Auth.JWTHMACSecret)
	str("LLMGW_JWKS_URL", &cfg.Auth.JWKSURL)
	str("LLMGW_ADMIN_KEY", &cfg.Auth.AdminKey)
	str("LLMGW_AUDIT_LOG", &cfg.Telemetry.AuditLogPath)
	boolean("LLMGW_DEV_TOKEN_ENDPOINT", &cfg.Auth.DevTokenEndpoint)
	boolean("LLMGW_CHAOS_HEADERS", &cfg.Server.EnableChaosHeaders)
	boolean("LLMGW_CACHE_ENABLED", &cfg.Cache.Enabled)
	if v, ok := os.LookupEnv("LLMGW_CORS_ORIGINS"); ok {
		cfg.Server.CORSOrigins = splitCSV(v)
	}
	// LLMGW_OLLAMA_URL rewrites Ollama endpoints (providers and the embedding
	// URL) that point at http://localhost:11434, e.g. to
	// http://host.docker.internal:11434 when the gateway runs in a container.
	if v, ok := os.LookupEnv("LLMGW_OLLAMA_URL"); ok && v != "" {
		v = strings.TrimRight(v, "/")
		for i := range cfg.Providers {
			cfg.Providers[i].BaseURL = strings.Replace(cfg.Providers[i].BaseURL, "http://localhost:11434", v, 1)
		}
		cfg.Cache.EmbeddingURL = strings.Replace(cfg.Cache.EmbeddingURL, "http://localhost:11434", v, 1)
	}
	// LLMGW_MOCKLLM_URL rewrites every provider base URL; convenient for
	// docker-compose / k8s where all providers point at the mock.
	if v, ok := os.LookupEnv("LLMGW_MOCKLLM_URL"); ok && v != "" {
		for i := range cfg.Providers {
			if strings.Contains(cfg.Providers[i].BaseURL, "localhost:8090") {
				cfg.Providers[i].BaseURL = strings.Replace(cfg.Providers[i].BaseURL, "http://localhost:8090", strings.TrimRight(v, "/"), 1)
			}
		}
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate checks internal consistency of the configuration.
func (c Config) Validate() error {
	names := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" || p.BaseURL == "" {
			return fmt.Errorf("provider requires name and base_url: %+v", p)
		}
		switch p.Type {
		case "openai", "anthropic", "gemini", "ollama", "mockllm":
		default:
			return fmt.Errorf("provider %q: unknown type %q", p.Name, p.Type)
		}
		if names[p.Name] {
			return fmt.Errorf("duplicate provider %q", p.Name)
		}
		if p.FaultInjectionRate < 0 || p.FaultInjectionRate > 1 {
			return fmt.Errorf("provider %q: fault_injection_rate must be in [0,1]", p.Name)
		}
		names[p.Name] = true
	}
	for _, r := range c.Routes {
		if len(r.Targets) == 0 {
			return fmt.Errorf("route %q has no targets", r.Match)
		}
		for _, t := range r.Targets {
			if !names[t.Provider] {
				return fmt.Errorf("route %q references unknown provider %q", r.Match, t.Provider)
			}
		}
	}
	if c.Cache.SimilarityThreshold <= 0 || c.Cache.SimilarityThreshold > 1 {
		return fmt.Errorf("cache.similarity_threshold must be in (0,1]")
	}
	if c.Cache.FallbackSimilarityThreshold < 0 || c.Cache.FallbackSimilarityThreshold > 1 {
		return fmt.Errorf("cache.fallback_similarity_threshold must be in [0,1]")
	}
	switch c.Cache.Embedder {
	case "", "hashing":
	case "ollama", "openai":
		if c.Cache.EmbeddingURL == "" || c.Cache.EmbeddingModel == "" {
			return fmt.Errorf("cache.embedder %q requires embedding_url and embedding_model", c.Cache.Embedder)
		}
	default:
		return fmt.Errorf("cache.embedder: unknown value %q (hashing | ollama | openai)", c.Cache.Embedder)
	}
	if c.Cache.EmbeddingDimensions < 0 {
		return fmt.Errorf("cache.embedding_dimensions must be >= 0")
	}
	if c.Auth.JWTHMACSecret == "" && c.Auth.JWTRSAPublicKey == "" && c.Auth.JWKSURL == "" && len(c.Tenants) == 0 {
		return fmt.Errorf("auth: configure a JWT key source or at least one API-key tenant")
	}
	return nil
}

// PriceFor returns the price for a model, falling back to "default".
func (c Config) PriceFor(model string) Price {
	if p, ok := c.Pricing[model]; ok {
		return p
	}
	for k, p := range c.Pricing {
		if strings.HasSuffix(k, "*") && strings.HasPrefix(model, strings.TrimSuffix(k, "*")) {
			return p
		}
	}
	return c.Pricing["default"]
}
