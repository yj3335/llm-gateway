package auth

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"
)

// TokenHandler serves POST /v1/auth/token, minting HS256 test tokens for
// local development, the playground, and benchmarks. It must only be enabled
// in non-production environments (config: auth.dev_token_endpoint).
type TokenHandler struct {
	Secret   []byte
	AdminKey string // if set, X-Admin-Key must match
	Issuer   string
	Audience string
	MaxTTL   time.Duration
	Defaults Defaults
}

type tokenRequest struct {
	MintRequest
	TTLSeconds int64 `json:"ttl_seconds"`
}

type tokenResponse struct {
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type"`
	ExpiresIn   int64     `json:"expires_in"`
	Principal   Principal `json:"principal"`
}

func (h *TokenHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	if h.AdminKey != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Key")), []byte(h.AdminKey)) != 1 {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid X-Admin-Key")
		return
	}
	var req tokenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request", "body must be JSON: "+err.Error())
		return
	}
	if req.TenantID == "" {
		writeErr(w, http.StatusBadRequest, "invalid_request", "tenant_id is required")
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl <= 0 || (h.MaxTTL > 0 && ttl > h.MaxTTL) {
		ttl = h.MaxTTL
	}
	if ttl <= 0 {
		ttl = time.Hour
	}
	p := Principal{TenantID: req.TenantID, Tier: req.Tier, RPMLimit: req.RPMLimit, TPMLimit: req.TPMLimit, PIIMode: req.PIIMode}
	h.Defaults.apply(&p)
	tok, err := MintHS256(h.Secret, MintRequest{
		TenantID: p.TenantID, Tier: p.Tier, RPMLimit: p.RPMLimit, TPMLimit: p.TPMLimit, PIIMode: p.PIIMode,
		TTL: ttl, Issuer: h.Issuer, Audience: h.Audience,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	p.AuthMethod = "jwt"
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tokenResponse{AccessToken: tok, TokenType: "Bearer", ExpiresIn: int64(ttl.Seconds()), Principal: p})
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"type": code, "message": msg}})
}
