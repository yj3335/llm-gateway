package auth

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims are the gateway-specific JWT claims.
type Claims struct {
	TenantID string `json:"tenant_id"`
	Tier     string `json:"tier,omitempty"`
	RPMLimit int64  `json:"rpm_limit,omitempty"`
	TPMLimit int64  `json:"tpm_limit,omitempty"`
	PIIMode  string `json:"pii_mode,omitempty"`
	jwt.RegisteredClaims
}

// VerifierConfig configures accepted key material. Any combination may be set.
type VerifierConfig struct {
	HMACSecret   []byte
	RSAPublicKey *rsa.PublicKey
	JWKS         *JWKS
	Issuer       string
	Audience     string
}

// Verifier validates bearer JWTs and caches successful verifications so the
// hot path avoids repeated signature checks for the same token.
type Verifier struct {
	cfg    VerifierConfig
	parser *jwt.Parser

	mu    sync.RWMutex
	cache map[[32]byte]cachedClaims
}

type cachedClaims struct {
	p   Principal
	exp time.Time
}

const verifiedCacheMax = 100_000

// NewVerifier builds a Verifier. At least one key source must be configured.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if len(cfg.HMACSecret) == 0 && cfg.RSAPublicKey == nil && cfg.JWKS == nil {
		return nil, errors.New("jwt verifier: no key material configured")
	}
	var methods []string
	if len(cfg.HMACSecret) > 0 {
		methods = append(methods, jwt.SigningMethodHS256.Alg())
	}
	if cfg.RSAPublicKey != nil || cfg.JWKS != nil {
		methods = append(methods, jwt.SigningMethodRS256.Alg())
	}
	opts := []jwt.ParserOption{jwt.WithValidMethods(methods), jwt.WithExpirationRequired(), jwt.WithLeeway(30 * time.Second)}
	if cfg.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(cfg.Issuer))
	}
	if cfg.Audience != "" {
		opts = append(opts, jwt.WithAudience(cfg.Audience))
	}
	return &Verifier{cfg: cfg, parser: jwt.NewParser(opts...), cache: make(map[[32]byte]cachedClaims)}, nil
}

func (v *Verifier) keyFunc(ctx context.Context) jwt.Keyfunc {
	return func(t *jwt.Token) (any, error) {
		switch t.Method.(type) {
		case *jwt.SigningMethodHMAC:
			if len(v.cfg.HMACSecret) == 0 {
				return nil, errors.New("HMAC tokens not accepted")
			}
			return v.cfg.HMACSecret, nil
		case *jwt.SigningMethodRSA:
			if kid, _ := t.Header["kid"].(string); kid != "" && v.cfg.JWKS != nil {
				return v.cfg.JWKS.Key(ctx, kid)
			}
			if v.cfg.RSAPublicKey != nil {
				return v.cfg.RSAPublicKey, nil
			}
			return nil, errors.New("no RSA key available for token")
		}
		return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
	}
}

// Verify validates the token and returns the Principal it encodes.
func (v *Verifier) Verify(ctx context.Context, token string) (Principal, error) {
	key := sha256.Sum256([]byte(token))
	now := time.Now()
	v.mu.RLock()
	c, ok := v.cache[key]
	v.mu.RUnlock()
	if ok && now.Before(c.exp) {
		return c.p, nil
	}

	var claims Claims
	if _, err := v.parser.ParseWithClaims(token, &claims, v.keyFunc(ctx)); err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if claims.TenantID == "" {
		return Principal{}, fmt.Errorf("%w: missing tenant_id claim", ErrInvalidToken)
	}
	p := Principal{
		TenantID: claims.TenantID,
		Tier:     claims.Tier,
		RPMLimit: claims.RPMLimit,
		TPMLimit: claims.TPMLimit,
		PIIMode:  claims.PIIMode,
	}
	exp := now.Add(time.Minute)
	if claims.ExpiresAt != nil && claims.ExpiresAt.Time.Before(exp) {
		exp = claims.ExpiresAt.Time
	}
	v.mu.Lock()
	if len(v.cache) >= verifiedCacheMax {
		for k, e := range v.cache {
			if now.After(e.exp) || len(v.cache) >= verifiedCacheMax {
				delete(v.cache, k)
			}
		}
	}
	v.cache[key] = cachedClaims{p: p, exp: exp}
	v.mu.Unlock()
	return p, nil
}

// MintRequest describes a token to issue.
type MintRequest struct {
	TenantID string        `json:"tenant_id"`
	Tier     string        `json:"tier"`
	RPMLimit int64         `json:"rpm_limit"`
	TPMLimit int64         `json:"tpm_limit"`
	PIIMode  string        `json:"pii_mode"`
	TTL      time.Duration `json:"-"`
	Issuer   string        `json:"-"`
	Audience string        `json:"-"`
}

func (m MintRequest) claims() Claims {
	now := time.Now()
	ttl := m.TTL
	if ttl == 0 {
		ttl = time.Hour
	}
	c := Claims{
		TenantID: m.TenantID, Tier: m.Tier, RPMLimit: m.RPMLimit, TPMLimit: m.TPMLimit, PIIMode: m.PIIMode,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   m.TenantID,
			Issuer:    m.Issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Second)),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	if m.Audience != "" {
		c.Audience = jwt.ClaimStrings{m.Audience}
	}
	return c
}

// MintHS256 issues an HMAC-SHA256 signed token.
func MintHS256(secret []byte, m MintRequest) (string, error) {
	if m.TenantID == "" {
		return "", errors.New("tenant_id required")
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, m.claims()).SignedString(secret)
}

// MintRS256 issues an RS256 signed token with an optional key ID.
func MintRS256(key *rsa.PrivateKey, kid string, m MintRequest) (string, error) {
	if m.TenantID == "" {
		return "", errors.New("tenant_id required")
	}
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, m.claims())
	if kid != "" {
		t.Header["kid"] = kid
	}
	return t.SignedString(key)
}

// ParseRSAPublicKeyPEM parses a PEM-encoded RSA public key.
func ParseRSAPublicKeyPEM(pemStr string) (*rsa.PublicKey, error) {
	return jwt.ParseRSAPublicKeyFromPEM([]byte(pemStr))
}

// JWKS fetches and caches RSA signing keys from an OAuth2/OIDC JWKS endpoint.
type JWKS struct {
	URL             string
	Client          *http.Client
	RefreshInterval time.Duration
	MinRefreshGap   time.Duration

	mu          sync.RWMutex
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time
}

// NewJWKS returns a JWKS client for url.
func NewJWKS(url string) *JWKS {
	return &JWKS{
		URL:             url,
		Client:          &http.Client{Timeout: 5 * time.Second},
		RefreshInterval: 10 * time.Minute,
		MinRefreshGap:   30 * time.Second,
		keys:            map[string]*rsa.PublicKey{},
	}
}

// Key returns the key for kid, refreshing the set when stale or kid unknown.
func (j *JWKS) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	j.mu.RLock()
	k, ok := j.keys[kid]
	stale := time.Since(j.fetchedAt) > j.RefreshInterval
	canRetry := time.Since(j.lastAttempt) > j.MinRefreshGap
	j.mu.RUnlock()
	if ok && !stale {
		return k, nil
	}
	if !ok && !canRetry && !j.fetchedAt.IsZero() {
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	if err := j.Refresh(ctx); err != nil && !ok {
		return nil, err
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	if k, ok := j.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown kid %q", kid)
}

// Refresh downloads the key set.
func (j *JWKS) Refresh(ctx context.Context) error {
	j.mu.Lock()
	j.lastAttempt = time.Now()
	j.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.URL, nil)
	if err != nil {
		return err
	}
	resp, err := j.Client.Do(req)
	if err != nil {
		return fmt.Errorf("jwks fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks fetch: status %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("jwks decode: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nb, err1 := base64.RawURLEncoding.DecodeString(k.N)
		eb, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
	}
	j.mu.Lock()
	j.keys = keys
	j.fetchedAt = time.Now()
	j.mu.Unlock()
	return nil
}

// JWKSDocument renders public keys as a JWKS JSON document (used by tests and
// by deployments that want the gateway to publish its own signing keys).
func JWKSDocument(keys map[string]*rsa.PublicKey) ([]byte, error) {
	type jwk struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Alg string `json:"alg"`
		Use string `json:"use"`
		N   string `json:"n"`
		E   string `json:"e"`
	}
	out := struct {
		Keys []jwk `json:"keys"`
	}{}
	for kid, k := range keys {
		out.Keys = append(out.Keys, jwk{
			Kty: "RSA", Kid: kid, Alg: "RS256", Use: "sig",
			N: base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
			E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
		})
	}
	return json.Marshal(out)
}
