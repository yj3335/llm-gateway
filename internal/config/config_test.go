package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLoadMockConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "configs", "gateway.mock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 4 || len(cfg.Routes) == 0 || len(cfg.Tenants) == 0 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if p := cfg.PriceFor("claude-3-5-haiku"); p.Input != 0.0008 {
		t.Fatalf("wildcard pricing: %+v", p)
	}
	if p := cfg.PriceFor("unknown"); p != cfg.Pricing["default"] {
		t.Fatalf("default pricing: %+v", p)
	}
}

// The default config runs entirely on local Ollama: no paid API keys.
func TestLoadOllamaConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "configs", "gateway.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("want 2 Ollama providers, got %d", len(cfg.Providers))
	}
	for _, p := range cfg.Providers {
		if p.Type != "ollama" || p.BaseURL != "http://localhost:11434" || p.APIKeyEnv != "" {
			t.Fatalf("provider %s must be keyless local Ollama: %+v", p.Name, p)
		}
		if p.ExtraBody["reasoning_effort"] != "none" {
			t.Fatalf("provider %s must disable thinking (reasoning_effort: none)", p.Name)
		}
	}
	if cfg.Providers[0].DefaultModel != "qwen3.5:9b" || cfg.Providers[1].DefaultModel != "gemma4:e4b" {
		t.Fatalf("unexpected models: %s, %s", cfg.Providers[0].DefaultModel, cfg.Providers[1].DefaultModel)
	}
	last := cfg.Routes[len(cfg.Routes)-1]
	if last.Match != "*" || last.Targets[0].Provider != "ollama-qwen" || last.Targets[1].Provider != "ollama-gemma" {
		t.Fatalf("default route must be qwen -> gemma: %+v", last)
	}
	c := cfg.Cache
	if c.Embedder != "ollama" || c.EmbeddingModel != "nomic-embed-text" || c.EmbeddingURL != "http://localhost:11434/api/embeddings" {
		t.Fatalf("cache must use nomic-embed-text via Ollama: %+v", c)
	}
	if c.SimilarityThreshold != 0.92 || c.FallbackSimilarityThreshold != 0.86 {
		t.Fatalf("thresholds: %v / %v", c.SimilarityThreshold, c.FallbackSimilarityThreshold)
	}

	t.Setenv("LLMGW_OLLAMA_URL", "http://host.docker.internal:11434/")
	cfg, err = Load(filepath.Join("..", "..", "configs", "gateway.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers[0].BaseURL != "http://host.docker.internal:11434" || cfg.Cache.EmbeddingURL != "http://host.docker.internal:11434/api/embeddings" {
		t.Fatalf("LLMGW_OLLAMA_URL not applied: %s %s", cfg.Providers[0].BaseURL, cfg.Cache.EmbeddingURL)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("LLMGW_REDIS_ADDR", "redis:6379")
	t.Setenv("LLMGW_MOCKLLM_URL", "http://mockllm:8090/")
	t.Setenv("LLMGW_CORS_ORIGINS", "https://a.example, https://b.example")
	cfg, err := Load(filepath.Join("..", "..", "configs", "gateway.mock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Redis.Addr != "redis:6379" || cfg.Providers[0].BaseURL != "http://mockllm:8090" || len(cfg.Server.CORSOrigins) != 2 {
		t.Fatalf("env overrides not applied: %s %s %v", cfg.Redis.Addr, cfg.Providers[0].BaseURL, cfg.Server.CORSOrigins)
	}
}

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	_ = os.WriteFile(bad, []byte("providers:\n  - {name: x, type: bogus, base_url: http://x}\n"), 0o600)
	if _, err := Load(bad); err == nil {
		t.Fatal("expected unknown provider type error")
	}
	_ = os.WriteFile(bad, []byte("providers:\n  - {name: x, type: openai, base_url: http://x}\nroutes:\n  - {match: '*', targets: [{provider: y}]}\n"), 0o600)
	if _, err := Load(bad); err == nil {
		t.Fatal("expected unknown route provider error")
	}
	_ = os.WriteFile(bad, []byte("cache:\n  embedder: ollama\n"), 0o600)
	if _, err := Load(bad); err == nil {
		t.Fatal("expected error: ollama embedder without embedding_url/model")
	}
	_ = os.WriteFile(bad, []byte("cache:\n  embedder: magic\n"), 0o600)
	if _, err := Load(bad); err == nil {
		t.Fatal("expected unknown embedder error")
	}
}

// The Kubernetes ConfigMap embeds a full gateway config; it must stay valid.
func TestK8sConfigMapIsValid(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "base", "configmap.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "gateway.yaml")
	if err := os.WriteFile(p, []byte(cm.Data["gateway.yaml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLMGW_JWT_SECRET", "k8s-secret")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("k8s configmap config invalid: %v", err)
	}
	if cfg.Server.EnableChaosHeaders || cfg.Auth.DevTokenEndpoint {
		t.Fatal("production manifest must disable chaos headers and the dev token endpoint")
	}
}
