package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

var secret = []byte("test-secret")

func newHMACVerifier(t *testing.T) *Verifier {
	t.Helper()
	v, err := NewVerifier(VerifierConfig{HMACSecret: secret, Issuer: "llm-gateway", Audience: "llm-gateway"})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mint(t *testing.T, m MintRequest) string {
	t.Helper()
	m.Issuer, m.Audience = "llm-gateway", "llm-gateway"
	tok, err := MintHS256(secret, m)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestHS256RoundTrip(t *testing.T) {
	v := newHMACVerifier(t)
	tok := mint(t, MintRequest{TenantID: "acme", Tier: "enterprise", RPMLimit: 1000, TPMLimit: 50000, PIIMode: "BLOCK", TTL: time.Minute})
	p, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if p.TenantID != "acme" || p.Tier != "enterprise" || p.RPMLimit != 1000 || p.TPMLimit != 50000 || p.PIIMode != "BLOCK" {
		t.Fatalf("unexpected principal %+v", p)
	}
	// Second verification is served from the verified-token cache.
	if p2, err := v.Verify(context.Background(), tok); err != nil || p2 != p {
		t.Fatalf("cached verify mismatch: %+v %v", p2, err)
	}
}

func TestRejectsBadTokens(t *testing.T) {
	v := newHMACVerifier(t)
	cases := map[string]string{
		"expired": mint(t, MintRequest{TenantID: "acme", TTL: -2 * time.Minute}),
		"wrong-key": func() string {
			s, _ := MintHS256([]byte("other"), MintRequest{TenantID: "acme", Issuer: "llm-gateway", Audience: "llm-gateway"})
			return s
		}(),
		"wrong-iss": func() string {
			s, _ := MintHS256(secret, MintRequest{TenantID: "acme", Issuer: "evil", Audience: "llm-gateway"})
			return s
		}(),
		"garbage": "not.a.jwt",
		"alg-none": func() string {
			s, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"tenant_id": "acme", "exp": time.Now().Add(time.Hour).Unix()}).SignedString(jwt.UnsafeAllowNoneSignatureType)
			return s
		}(),
		"no-tenant": func() string {
			s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": "llm-gateway", "aud": "llm-gateway", "exp": time.Now().Add(time.Hour).Unix()}).SignedString(secret)
			return s
		}(),
		"missing-exp": func() string {
			s, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"tenant_id": "acme", "iss": "llm-gateway", "aud": "llm-gateway"}).SignedString(secret)
			return s
		}(),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("expected ErrInvalidToken, got %v", err)
			}
		})
	}
}

func TestRS256StaticAndJWKS(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := rsa.GenerateKey(rand.Reader, 2048)

	// Static public key.
	v, err := NewVerifier(VerifierConfig{RSAPublicKey: &key.PublicKey})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := MintRS256(key, "", MintRequest{TenantID: "rsa-tenant", TTL: time.Minute})
	if p, err := v.Verify(context.Background(), tok); err != nil || p.TenantID != "rsa-tenant" {
		t.Fatalf("static RS256: %+v %v", p, err)
	}
	// HS256 must be rejected when only RSA is configured (alg confusion).
	hs, _ := MintHS256(secret, MintRequest{TenantID: "x", TTL: time.Minute})
	if _, err := v.Verify(context.Background(), hs); err == nil {
		t.Fatal("HS256 accepted by RSA-only verifier")
	}

	// JWKS with key rotation via kid.
	var fetches atomic.Int32
	doc, _ := JWKSDocument(map[string]*rsa.PublicKey{"k1": &key.PublicKey})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		_, _ = w.Write(doc)
	}))
	defer srv.Close()
	jwks := NewJWKS(srv.URL)
	jv, _ := NewVerifier(VerifierConfig{JWKS: jwks})
	tok, _ = MintRS256(key, "k1", MintRequest{TenantID: "oauth-tenant", TTL: time.Minute})
	if p, err := jv.Verify(context.Background(), tok); err != nil || p.TenantID != "oauth-tenant" {
		t.Fatalf("jwks RS256: %+v %v", p, err)
	}
	bad, _ := MintRS256(other, "k1", MintRequest{TenantID: "oauth-tenant", TTL: time.Minute})
	if _, err := jv.Verify(context.Background(), bad); err == nil {
		t.Fatal("token signed with wrong key accepted")
	}
	unknown, _ := MintRS256(key, "k-unknown", MintRequest{TenantID: "oauth-tenant", TTL: time.Minute})
	if _, err := jv.Verify(context.Background(), unknown); err == nil {
		t.Fatal("unknown kid accepted")
	}
	if fetches.Load() < 1 {
		t.Fatal("JWKS never fetched")
	}
}

type countingStore struct {
	inner KeyStore
	calls atomic.Int32
}

func (c *countingStore) Lookup(ctx context.Context, k string) (Principal, error) {
	c.calls.Add(1)
	return c.inner.Lookup(ctx, k)
}

func TestCachedKeyStoreLayers(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	static := NewStaticKeyStore()
	static.Add("gw-key-1", Principal{TenantID: "t1", RPMLimit: 10, TPMLimit: 100, PIIMode: "REDACT"})
	backing := &countingStore{inner: static}

	c := NewCachedKeyStore(backing, rdb, time.Minute)
	for i := 0; i < 5; i++ {
		p, err := c.Lookup(context.Background(), "gw-key-1")
		if err != nil || p.TenantID != "t1" {
			t.Fatalf("lookup: %+v %v", p, err)
		}
	}
	if got := backing.calls.Load(); got != 1 {
		t.Fatalf("expected 1 backing lookup with L1 cache, got %d", got)
	}
	if !mr.Exists("llmgw:apikey:" + HashAPIKey("gw-key-1")) {
		t.Fatal("expected Redis L2 entry")
	}

	// A fresh replica (empty L1) is served from Redis without hitting the DB.
	c2 := NewCachedKeyStore(backing, rdb, time.Minute)
	if _, err := c2.Lookup(context.Background(), "gw-key-1"); err != nil {
		t.Fatal(err)
	}
	if got := backing.calls.Load(); got != 1 {
		t.Fatalf("expected Redis hit, backing calls=%d", got)
	}

	// Negative caching.
	for i := 0; i < 3; i++ {
		if _, err := c.Lookup(context.Background(), "nope"); !errors.Is(err, ErrInvalidAPIKey) {
			t.Fatalf("expected invalid key, got %v", err)
		}
	}
	if got := backing.calls.Load(); got != 2 {
		t.Fatalf("expected negative caching, backing calls=%d", got)
	}

	c.Invalidate(context.Background(), "gw-key-1")
	if mr.Exists("llmgw:apikey:" + HashAPIKey("gw-key-1")) {
		t.Fatal("invalidate did not clear Redis")
	}
}

func TestAuthenticatorMiddleware(t *testing.T) {
	static := NewStaticKeyStore()
	static.Add("gw-abc", Principal{TenantID: "key-tenant"})
	a := &Authenticator{JWT: newHMACVerifier(t), Keys: static, Defaults: Defaults{RPM: 60, TPM: 1000, PIIMode: "REDACT"}}

	var got Principal
	h := a.Middleware(func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, err.Error(), http.StatusUnauthorized)
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = FromContext(r.Context())
	}))

	do := func(hdr map[string]string) int {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	if code := do(nil); code != http.StatusUnauthorized {
		t.Fatalf("no creds: %d", code)
	}
	if code := do(map[string]string{"Authorization": "Basic Zm9vOmJhcg=="}); code != http.StatusUnauthorized {
		t.Fatalf("basic auth: %d", code)
	}
	if code := do(map[string]string{"X-API-Key": "wrong"}); code != http.StatusUnauthorized {
		t.Fatalf("bad key: %d", code)
	}
	if code := do(map[string]string{"X-API-Key": "gw-abc"}); code != http.StatusOK || got.TenantID != "key-tenant" || got.AuthMethod != "api_key" || got.RPMLimit != 60 || got.PIIMode != "REDACT" {
		t.Fatalf("api key auth: %d %+v", code, got)
	}
	tok := mint(t, MintRequest{TenantID: "jwt-tenant", PIIMode: "block", TTL: time.Minute})
	if code := do(map[string]string{"Authorization": "Bearer " + tok}); code != http.StatusOK || got.TenantID != "jwt-tenant" || got.AuthMethod != "jwt" || got.PIIMode != "BLOCK" || got.TPMLimit != 1000 {
		t.Fatalf("jwt auth: %d %+v", code, got)
	}
}

func TestTokenHandler(t *testing.T) {
	h := &TokenHandler{Secret: secret, AdminKey: "admin", Issuer: "llm-gateway", Audience: "llm-gateway", MaxTTL: time.Hour, Defaults: Defaults{RPM: 60, TPM: 1000}}
	body := `{"tenant_id":"bench","rpm_limit":5000,"pii_mode":"REDACT","ttl_seconds":600}`

	r := httptest.NewRequest(http.MethodPost, "/v1/auth/token", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without admin key, got %d", w.Code)
	}

	r = httptest.NewRequest(http.MethodPost, "/v1/auth/token", bytes.NewBufferString(body))
	r.Header.Set("X-Admin-Key", "admin")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("mint: %d %s", w.Code, w.Body)
	}
	var resp tokenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ExpiresIn != 600 || resp.Principal.TPMLimit != 1000 {
		t.Fatalf("unexpected response %+v", resp)
	}
	p, err := newHMACVerifier(t).Verify(context.Background(), resp.AccessToken)
	if err != nil || p.TenantID != "bench" || p.RPMLimit != 5000 {
		t.Fatalf("minted token invalid: %+v %v", p, err)
	}
}

func TestNormalizePIIMode(t *testing.T) {
	if NormalizePIIMode("block", "") != PIIModeBlock || NormalizePIIMode("", "BLOCK") != PIIModeBlock || NormalizePIIMode("weird", "") != PIIModeRedact {
		t.Fatal("NormalizePIIMode mismatch")
	}
}
