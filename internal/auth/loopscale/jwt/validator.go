// SPDX-License-Identifier: Apache-2.0
// Copyright 2025-2026 The LoopScale Authors

// Package jwt provides JWT validation against Ory Hydra's JWKS endpoint.
// Shared between the cloud auth service and the engine API.
//
// Per ADR-0029, this package enforces RFC 8725bis hardening:
//   - Algorithm whitelist via goJWT.WithValidMethods (defense in depth
//     against algorithm confusion attacks; RFC 8725 §3.1).
//   - typ header validation — only "JWT", "at+jwt", or absent are
//     accepted (RFC 8725bis §2.6 cross-JWT confusion mitigation).
//   - JWKS fetch guarded by an optional consumer-defined Breaker (when
//     configured) so that Hydra outages do not cascade into auth failures —
//     cached keys are served during the circuit-open window
//     (gate PKG-G16 eat-your-own-dogfood; DIP per plan m1-decouple-in-place).
package jwt

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	goJWT "github.com/golang-jwt/jwt/v5"
)

// Breaker is the minimal circuit-breaker contract pkg/jwt depends on (DIP,
// consumer-defined interface per plan m1-decouple-in-place D1). The concrete
// pkg/circuitbreaker.Breaker satisfies it structurally; jwt does NOT import
// circuitbreaker so the contracts surface stays free of engine-internal deps.
//
//   - PreCheck reports whether a call may proceed; a non-nil error means the
//     circuit is open (or otherwise not ready) and the call MUST be skipped.
//   - RecordResult feeds the outcome of the guarded call back to the breaker
//     so it can trip / recover.
type Breaker interface {
	PreCheck() error
	RecordResult(err error)
}

// AllowedAccessAlgs is the whitelist of signing algorithms acceptable
// for access tokens issued by Ory Hydra. Hydra produces RS256 by
// default; widening this list MUST go through ADR-0029 review.
var AllowedAccessAlgs = []string{"RS256"}

// AllowedSessionAlgs is the whitelist of signing algorithms acceptable
// for HMAC session tokens (dashboard → API).
var AllowedSessionAlgs = []string{"HS256"}

// allowedTypValues is the closed set of "typ" header values accepted
// by LoopScale validators per RFC 7519 / RFC 9068. Empty (header absent)
// is also accepted — RFC 7519 does not mandate the header.
var allowedTypValues = map[string]struct{}{
	"":       {}, // header absent is allowed
	"JWT":    {}, // RFC 7519 default
	"at+jwt": {}, // RFC 9068 OAuth access-token profile
}

// validateTypHeader rejects tokens whose "typ" header is set to a
// value outside allowedTypValues. Closes the cross-JWT confusion
// vector (RFC 8725bis §2.6).
func validateTypHeader(token *goJWT.Token) error {
	raw, ok := token.Header["typ"]
	if !ok {
		return nil
	}
	typ, ok := raw.(string)
	if !ok {
		return fmt.Errorf("typ header must be a string, got %T", raw)
	}
	if _, accepted := allowedTypValues[typ]; !accepted {
		return fmt.Errorf("unexpected typ header: %q", typ)
	}
	return nil
}

// Claims represents the JWT claims extracted from a Hydra-issued token.
// Subject is in RegisteredClaims.Subject — do NOT redeclare it here.
type Claims struct {
	goJWT.RegisteredClaims
	Email    string `json:"email,omitempty"`
	Name     string `json:"name,omitempty"`
	TenantID string `json:"tenant_id,omitempty"`
	Role     string `json:"role,omitempty"`
	Scope    string `json:"scope,omitempty"`
	// Hydra puts consent session claims in "ext" (access_token session).
	Ext struct {
		Email    string `json:"email,omitempty"`
		Name     string `json:"name,omitempty"`
		TenantID string `json:"tenant_id,omitempty"`
	} `json:"ext,omitempty"`
}

// SessionClaims are the claims in HMAC session tokens (dashboard → API).
// Shared between cloud service (issuer) and engine API (validator).
type SessionClaims struct {
	goJWT.RegisteredClaims
	Email    string `json:"email,omitempty"`
	Name     string `json:"name,omitempty"`
	TenantID string `json:"tenant_id,omitempty"`
	// AccountID is the Account ("LoopScale customer") the tenant belongs to. M3
	// (account-level tenancy, Phase 2): ADDITIVE and OPTIONAL — older validators
	// ignore it (encoding/json drops unknown keys) and tokens minted before M3
	// (or for a tenant without an account) carry it empty. Forward-compatible.
	AccountID string `json:"account_id,omitempty"`
}

// Validator validates JWT tokens against a JWKS endpoint.
//
// Audience handling per RFC 7519 § 4.1.3: validation succeeds when the
// token's `aud` claim contains ANY of the configured `audiences`. This
// supports services that legitimately accept multiple audiences (e.g.,
// loop-runtime accepts BOTH `loop-runtime` for its own dashboard session
// JWTs AND `loop-runtime` for bootstrap/CLI JWTs proxied via /engine/*
// paths into loop-runtime). Matches AWS Cognito + Auth0 + Okta + Google
// Cloud Identity multi-audience canonical pattern.
type Validator struct {
	jwksURL    string
	issuer     string
	audiences  []string // RFC 7519 § 4.1.3 — accept ANY of these
	httpClient *http.Client

	mu      sync.RWMutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
	ttl     time.Duration

	// cb optionally wraps fetchJWKS calls under circuit breaker
	// protection. When the breaker is Open, fetchJWKS short-circuits
	// without contacting the JWKS endpoint; cached keys are served
	// instead via getKey (graceful degradation).
	cb Breaker
}

// NewValidator creates a JWT validator that fetches JWKS from the given URL.
// The audience parameter enforces strict audience validation on all tokens.
// MEET-2026-04-07-039: B4 fix — audience is mandatory per service.
//
// For services that need to accept multiple audiences (e.g., loop-runtime
// proxying both dashboard endpoints AND /engine/* paths into loop-runtime),
// use NewValidatorWithAudiences instead. This single-audience constructor
// is preserved for backward compatibility and delegates to the multi-aud
// constructor with `[]string{audience}` so all callers go through the
// same internal data path.
func NewValidator(jwksURL, issuer, audience string) *Validator {
	return NewValidatorWithAudiences(jwksURL, issuer, []string{audience})
}

// NewValidatorWithAudiences creates a JWT validator that accepts ANY of
// the provided audiences (RFC 7519 § 4.1.3). Panics if audiences is empty
// (programming error — defense in depth against the B4 "audience mandatory"
// rule from MEET-2026-04-07-039).
func NewValidatorWithAudiences(jwksURL, issuer string, audiences []string) *Validator {
	if len(audiences) == 0 {
		panic("lsjwt.NewValidatorWithAudiences: audiences must be non-empty (MEET-2026-04-07-039 B4)")
	}
	return &Validator{
		jwksURL:   jwksURL,
		issuer:    issuer,
		audiences: append([]string(nil), audiences...), // defensive copy
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		keys: make(map[string]*rsa.PublicKey),
		ttl:  5 * time.Minute,
	}
}

// WithCircuitBreaker attaches a shared circuit breaker that wraps
// JWKS fetches. When the breaker opens (after repeated upstream
// failures), the validator continues serving cached keys until the
// breaker recovers — preventing Hydra outages from cascading into
// blanket auth failures.
//
// Per pkg-sota-faang-grade-plan T1.4 + ADR-0029 + gate PKG-G16.
//
// Returns the Validator so callers can chain construction:
//
//	v := jwt.NewValidator(jwksURL, issuer, audience).WithCircuitBreaker(cb)
func (v *Validator) WithCircuitBreaker(cb Breaker) *Validator {
	v.cb = cb
	return v
}

// errJWKSCircuitOpen is returned by fetchJWKS when the circuit breaker
// short-circuited the fetch. Callers (getKey) interpret this as
// "use whatever cache you have".
var errJWKSCircuitOpen = errors.New("jwks fetch skipped: circuit breaker open")

// Validate parses and validates a JWT token, returning the claims.
//
// Validation steps (in order):
//  1. Parser-level whitelist via WithValidMethods(AllowedAccessAlgs)
//     rejects alg=none and alg!=RS256 BEFORE keyfunc runs.
//  2. typ header validation rejects cross-JWT confusion attacks.
//  3. Signing method type-assertion (defense in depth — should be
//     redundant with step 1 but kept as belt-and-suspenders).
//  4. JWKS lookup by kid + signature verification.
//  5. Issuer / audience / exp claim checks.
func (v *Validator) Validate(tokenString string) (*Claims, error) {
	token, err := goJWT.ParseWithClaims(tokenString, &Claims{}, func(token *goJWT.Token) (interface{}, error) {
		if err := validateTypHeader(token); err != nil {
			return nil, err
		}
		if _, ok := token.Method.(*goJWT.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		kid, ok := token.Header["kid"].(string)
		if !ok {
			return nil, fmt.Errorf("missing kid in token header")
		}

		key, err := v.getKey(kid)
		if err != nil {
			if err := v.fetchJWKS(); err != nil {
				return nil, fmt.Errorf("failed to refresh JWKS: %w", err)
			}
			key, err = v.getKey(kid)
			if err != nil {
				return nil, err
			}
		}
		return key, nil
	},
		goJWT.WithValidMethods(AllowedAccessAlgs),
		goJWT.WithIssuer(v.issuer),
		// Multi-audience handling per RFC 7519 § 4.1.3: signature + issuer +
		// exp are checked here WITHOUT WithAudience (signature verified once),
		// then audience match is asserted manually after parse (accept ANY).
		goJWT.WithExpirationRequired(),
	)

	if err != nil {
		return nil, fmt.Errorf("token validation failed: %w", err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	// RFC 7519 § 4.1.3 — accept ANY of the configured audiences.
	// Claims.Audience is goJWT.ClaimStrings (string OR []string per RFC).
	if !audienceMatchesAny(claims.Audience, v.audiences) {
		return nil, fmt.Errorf("token validation failed: token has invalid audience, want any of %v, got %v", v.audiences, []string(claims.Audience))
	}

	return claims, nil
}

// audienceMatchesAny returns true when the token's `aud` claim (which per
// RFC 7519 § 4.1.3 may be a single string OR array) contains at least ONE
// of the validator's accepted audiences. Empty token audience always fails
// (defense in depth against the MEET-2026-04-07-039 B4 "audience mandatory" rule).
func audienceMatchesAny(tokenAud goJWT.ClaimStrings, accepted []string) bool {
	for _, want := range accepted {
		for _, got := range tokenAud {
			if got == want {
				return true
			}
		}
	}
	return false
}

// SessionTokenIssuer issues and validates HMAC-SHA256 session JWTs.
// §2.2: Supports kid header and dual-key rotation (current + previous secret).
type SessionTokenIssuer struct {
	secret         []byte
	previousSecret []byte // nil if no rotation in progress
	kid            string // first 8 chars of SHA256(secret)
	issuer         string
	audience       string
	lifetime       time.Duration
}

// computeKID generates a key ID from the first 8 chars of the secret's SHA256 hash.
func computeKID(secret []byte) string {
	h := sha256.Sum256(secret)
	return fmt.Sprintf("%x", h[:4]) // 8 hex chars
}

// NewSessionTokenIssuer creates an issuer/validator for HMAC session tokens.
// previousSecret can be empty string if no rotation is in progress.
func NewSessionTokenIssuer(secret, issuer, audience string, lifetime time.Duration) *SessionTokenIssuer {
	s := &SessionTokenIssuer{
		secret:   []byte(secret),
		kid:      computeKID([]byte(secret)),
		issuer:   issuer,
		audience: audience,
		lifetime: lifetime,
	}
	return s
}

// WithPreviousSecret adds a previous secret for zero-downtime rotation.
// Tokens signed with the previous secret are accepted during the rotation window.
func (s *SessionTokenIssuer) WithPreviousSecret(previousSecret string) *SessionTokenIssuer {
	if previousSecret != "" {
		s.previousSecret = []byte(previousSecret)
	}
	return s
}

// Issue creates a signed HMAC JWT for a dashboard user.
// §2.2: Includes kid header for key identification during rotation.
// M3: accountID is the Account the tenant belongs to; pass "" for a tenant
// without an account (legacy / pre-M3) — the claim is then omitted (omitempty).
func (s *SessionTokenIssuer) Issue(sub, email, name, tenantID, accountID string) (string, int64, error) {
	now := time.Now()
	claims := SessionClaims{
		RegisteredClaims: goJWT.RegisteredClaims{
			Subject:   sub,
			Issuer:    s.issuer,
			Audience:  goJWT.ClaimStrings{s.audience},
			IssuedAt:  goJWT.NewNumericDate(now),
			NotBefore: goJWT.NewNumericDate(now),
			ExpiresAt: goJWT.NewNumericDate(now.Add(s.lifetime)),
		},
		Email:     email,
		Name:      name,
		TenantID:  tenantID,
		AccountID: accountID,
	}

	token := goJWT.NewWithClaims(goJWT.SigningMethodHS256, claims)
	token.Header["kid"] = s.kid // §2.2: key identification
	signed, err := token.SignedString(s.secret)
	if err != nil {
		return "", 0, fmt.Errorf("sign session token: %w", err)
	}
	return signed, int64(s.lifetime.Seconds()), nil
}

// ValidateSession parses and validates an HMAC session token.
// §2.2: Tries current secret first, then previous (for rotation window).
func (s *SessionTokenIssuer) ValidateSession(tokenString string) (*SessionClaims, error) {
	// Try current secret
	claims, err := s.validateWithSecret(tokenString, s.secret)
	if err == nil {
		return claims, nil
	}

	// Try previous secret if available (rotation window)
	if s.previousSecret != nil {
		claims, prevErr := s.validateWithSecret(tokenString, s.previousSecret)
		if prevErr == nil {
			return claims, nil
		}
	}

	return nil, fmt.Errorf("invalid session token: %w", err)
}

func (s *SessionTokenIssuer) validateWithSecret(tokenString string, secret []byte) (*SessionClaims, error) {
	token, err := goJWT.ParseWithClaims(tokenString, &SessionClaims{}, func(token *goJWT.Token) (interface{}, error) {
		if err := validateTypHeader(token); err != nil {
			return nil, err
		}
		if _, ok := token.Method.(*goJWT.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return secret, nil
	},
		goJWT.WithValidMethods(AllowedSessionAlgs),
		goJWT.WithIssuer(s.issuer),
		goJWT.WithAudience(s.audience),
		goJWT.WithExpirationRequired(),
	)
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(*SessionClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid session token claims")
	}
	return claims, nil
}

func (v *Validator) getKey(kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	stale := time.Since(v.fetched) > v.ttl || len(v.keys) == 0
	v.mu.RUnlock()
	if stale {
		if err := v.fetchJWKS(); err != nil {
			// If the CB is open, fall through to cached lookup —
			// graceful degradation. Any other fetch failure with
			// an empty cache propagates up as the original error.
			if !errors.Is(err, errJWKSCircuitOpen) {
				v.mu.RLock()
				cacheEmpty := len(v.keys) == 0
				v.mu.RUnlock()
				if cacheEmpty {
					return nil, err
				}
			}
		}
	}
	v.mu.RLock()
	key, ok := v.keys[kid]
	v.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("key %s not found in JWKS", kid)
	}
	return key, nil
}

type jwksResponse struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kid string `json:"kid"`
	Kty string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
	Use string `json:"use"`
}

func (v *Validator) fetchJWKS() error {
	// Fast path: cache still warm, nothing to do.
	v.mu.RLock()
	if time.Since(v.fetched) < v.ttl && len(v.keys) > 0 {
		v.mu.RUnlock()
		return nil
	}
	v.mu.RUnlock()

	// Wrap the actual HTTP fetch in the circuit breaker when configured.
	// DIP (plan m1-decouple-in-place T1.1): jwt depends on the Breaker
	// interface, not the concrete pkg/circuitbreaker. The check-then-report
	// split (PreCheck → fetch → RecordResult) mirrors the breaker's own
	// Execute helper without importing it.
	if v.cb != nil {
		if err := v.cb.PreCheck(); err != nil {
			// Circuit open (or not ready): skip the fetch and signal getKey
			// to serve whatever cache it has via our sentinel.
			return errJWKSCircuitOpen
		}
		err := v.fetchJWKSLocked(context.Background())
		v.cb.RecordResult(err)
		return err
	}
	return v.fetchJWKSLocked(context.Background())
}

// fetchJWKSLocked performs the actual HTTP GET + parse + cache update
// under v.mu. Caller may run via Execute (CB) or directly.
func (v *Validator) fetchJWKSLocked(ctx context.Context) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if time.Since(v.fetched) < v.ttl && len(v.keys) > 0 {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return fmt.Errorf("JWKS request build failed: %w", err)
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("JWKS fetch failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS fetch returned status %d", resp.StatusCode)
	}

	var jwks jwksResponse
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("JWKS decode failed: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey)
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" || k.Use != "sig" {
			continue
		}
		pubKey, err := parseRSAPublicKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pubKey
	}

	v.keys = keys
	v.fetched = time.Now()
	return nil
}

func parseRSAPublicKey(nStr, eStr string) (*rsa.PublicKey, error) {
	nBytes, err := goJWT.NewParser().DecodeSegment(nStr)
	if err != nil {
		return nil, err
	}
	eBytes, err := goJWT.NewParser().DecodeSegment(eStr)
	if err != nil {
		return nil, err
	}

	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(new(big.Int).SetBytes(eBytes).Int64()),
	}, nil
}
