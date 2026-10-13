// Package auth implements multi-tenant authentication for the gateway:
// JWT / OAuth2 bearer tokens (HS256, RS256 static key, or RS256 via JWKS)
// and X-API-Key lookup through a layered L1 memory -> Redis -> PostgreSQL store.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// PII handling modes a tenant can be configured with.
const (
	PIIModeRedact = "REDACT"
	PIIModeBlock  = "BLOCK"
)

var (
	ErrMissingCredentials = errors.New("missing credentials: provide Authorization: Bearer <JWT> or X-API-Key")
	ErrInvalidToken       = errors.New("invalid or expired bearer token")
	ErrInvalidAPIKey      = errors.New("invalid API key")
)

// Principal is the authenticated tenant identity attached to a request.
type Principal struct {
	TenantID   string `json:"tenant_id"`
	Tier       string `json:"tier"`
	RPMLimit   int64  `json:"rpm_limit"`
	TPMLimit   int64  `json:"tpm_limit"`
	PIIMode    string `json:"pii_mode"`
	AuthMethod string `json:"auth_method"`
}

// Defaults fills unset quota/policy fields.
type Defaults struct {
	RPM     int64
	TPM     int64
	PIIMode string
}

func (d Defaults) apply(p *Principal) {
	if p.RPMLimit <= 0 {
		p.RPMLimit = d.RPM
	}
	if p.TPMLimit <= 0 {
		p.TPMLimit = d.TPM
	}
	p.PIIMode = NormalizePIIMode(p.PIIMode, d.PIIMode)
	if p.Tier == "" {
		p.Tier = "standard"
	}
}

// NormalizePIIMode returns REDACT or BLOCK, using fallback for unknown values.
func NormalizePIIMode(mode, fallback string) string {
	switch strings.ToUpper(strings.TrimSpace(mode)) {
	case PIIModeBlock:
		return PIIModeBlock
	case PIIModeRedact:
		return PIIModeRedact
	}
	if strings.ToUpper(fallback) == PIIModeBlock {
		return PIIModeBlock
	}
	return PIIModeRedact
}

// HashAPIKey returns the hex SHA-256 used to store and look up API keys.
// Raw keys are never persisted.
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

type ctxKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal stored in ctx.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Authenticator resolves request credentials to a Principal.
type Authenticator struct {
	JWT      *Verifier // may be nil when only API keys are accepted
	Keys     KeyStore  // may be nil when only JWTs are accepted
	Defaults Defaults
}

// Authenticate inspects the Authorization and X-API-Key headers.
func (a *Authenticator) Authenticate(r *http.Request) (Principal, error) {
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		if a.Keys == nil {
			return Principal{}, ErrInvalidAPIKey
		}
		p, err := a.Keys.Lookup(r.Context(), key)
		if err != nil {
			return Principal{}, err
		}
		p.AuthMethod = "api_key"
		a.Defaults.apply(&p)
		return p, nil
	}
	authz := r.Header.Get("Authorization")
	if authz == "" {
		return Principal{}, ErrMissingCredentials
	}
	scheme, token, ok := strings.Cut(authz, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return Principal{}, ErrMissingCredentials
	}
	if a.JWT == nil {
		return Principal{}, ErrInvalidToken
	}
	p, err := a.JWT.Verify(r.Context(), strings.TrimSpace(token))
	if err != nil {
		return Principal{}, err
	}
	p.AuthMethod = "jwt"
	a.Defaults.apply(&p)
	return p, nil
}

// Middleware authenticates requests and stores the Principal in the context.
// onError writes the failure response.
func (a *Authenticator) Middleware(onError func(http.ResponseWriter, *http.Request, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := a.Authenticate(r)
			if err != nil {
				onError(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}
