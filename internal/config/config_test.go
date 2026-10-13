package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestLoadRepoConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "configs", "gateway.yaml"))
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

func TestEnvOverrides(t *testing.T) {
	t.Setenv("LLMGW_REDIS_ADDR", "redis:6379")
	t.Setenv("LLMGW_MOCKLLM_URL", "http://mockllm:8090/")
	t.Setenv("LLMGW_CORS_ORIGINS", "https://a.example, https://b.example")
	cfg, err := Load(filepath.Join("..", "..", "configs", "gateway.yaml"))
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
}

// The Kubernetes ConfigMap embeds a full gateway config; it must stay valid.
func TestK8sConfigMapIsValid(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "configmap.yaml"))
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
