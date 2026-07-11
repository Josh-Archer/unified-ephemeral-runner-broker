package api

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const oidcCacheTTL = time.Hour

var (
	ErrMissingBearerToken = errors.New("missing bearer token")
	ErrInvalidBearerToken = errors.New("invalid bearer token")
	ErrInvalidAudience    = errors.New("invalid oidc audience")
	ErrInvalidIssuer      = errors.New("invalid oidc issuer")
	ErrInvalidSubject     = errors.New("missing oidc subject")
	ErrInvalidToken       = errors.New("invalid oidc token")
	ErrInvalidSignature   = errors.New("invalid oidc token signature")
	ErrTokenExpired       = errors.New("expired oidc token")
	ErrTokenNotValidYet   = errors.New("oidc token not valid yet")
)

type OIDCClaims struct {
	Iss string      `json:"iss"`
	Aud interface{} `json:"aud"`
	Sub string      `json:"sub"`
	Exp int64       `json:"exp"`
	Nbf int64       `json:"nbf,omitempty"`
}

type OIDCVerifier struct {
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex
	discovery map[string]cachedDiscovery
	keys      map[string]cachedJWKS
}

type cachedDiscovery struct {
	jwksURI string
	expires time.Time
}

type cachedJWKS struct {
	keys    []jwkKey
	expires time.Time
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ,omitempty"`
}

type discoveryDocument struct {
	JWKSURI string `json:"jwks_uri"`
}

type jwksDocument struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kty string `json:"kty"`
	Use string `json:"use,omitempty"`
	Kid string `json:"kid"`
	Alg string `json:"alg,omitempty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func NewOIDCVerifier() *OIDCVerifier {
	return &OIDCVerifier{
		client:    http.DefaultClient,
		now:       time.Now,
		discovery: make(map[string]cachedDiscovery),
		keys:      make(map[string]cachedJWKS),
	}
}

func (v *OIDCVerifier) VerifyToken(ctx context.Context, rawToken string, expectedAudience string, allowedIssuers []string) (OIDCClaims, error) {
	header, claims, signingInput, signature, err := parseJWT(rawToken)
	if err != nil {
		return OIDCClaims{}, err
	}
	if header.Alg != "RS256" || header.Kid == "" {
		return OIDCClaims{}, ErrInvalidToken
	}
	if err := ValidateOIDCClaims(claims, expectedAudience, allowedIssuers); err != nil {
		return OIDCClaims{}, err
	}
	if err := validateOIDCTimeClaims(claims, v.now()); err != nil {
		return OIDCClaims{}, err
	}

	jwksURI, err := v.jwksURI(ctx, claims.Iss)
	if err != nil {
		return OIDCClaims{}, err
	}
	key, err := v.signingKey(ctx, jwksURI, header.Kid, false)
	if err != nil {
		return OIDCClaims{}, err
	}
	if err := verifyRS256(signingInput, signature, key); err != nil {
		key, refreshErr := v.signingKey(ctx, jwksURI, header.Kid, true)
		if refreshErr != nil {
			return OIDCClaims{}, refreshErr
		}
		if retryErr := verifyRS256(signingInput, signature, key); retryErr != nil {
			return OIDCClaims{}, retryErr
		}
	}

	return claims, nil
}

func parseJWT(rawToken string) (jwtHeader, OIDCClaims, []byte, []byte, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return jwtHeader{}, OIDCClaims{}, nil, nil, fmt.Errorf("%w: expected JWT with three parts", ErrInvalidToken)
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return jwtHeader{}, OIDCClaims{}, nil, nil, fmt.Errorf("%w: decode header: %v", ErrInvalidToken, err)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return jwtHeader{}, OIDCClaims{}, nil, nil, fmt.Errorf("%w: decode header: %v", ErrInvalidToken, err)
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtHeader{}, OIDCClaims{}, nil, nil, fmt.Errorf("%w: decode claims: %v", ErrInvalidToken, err)
	}
	var claims OIDCClaims
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return jwtHeader{}, OIDCClaims{}, nil, nil, fmt.Errorf("%w: decode claims: %v", ErrInvalidToken, err)
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return jwtHeader{}, OIDCClaims{}, nil, nil, fmt.Errorf("%w: decode signature: %v", ErrInvalidToken, err)
	}

	return header, claims, []byte(parts[0] + "." + parts[1]), signature, nil
}

func ValidateOIDCClaims(claims OIDCClaims, expectedAudience string, allowedIssuers []string) error {
	if claims.Sub == "" {
		return ErrInvalidSubject
	}

	if expectedAudience != "" && !audienceMatches(claims.Aud, expectedAudience) {
		return ErrInvalidAudience
	}

	if len(allowedIssuers) == 0 {
		return ErrInvalidIssuer
	}
	for _, issuer := range allowedIssuers {
		if claims.Iss == issuer {
			return nil
		}
	}
	return ErrInvalidIssuer
}

func validateOIDCTimeClaims(claims OIDCClaims, now time.Time) error {
	if claims.Exp == 0 || !now.Before(time.Unix(claims.Exp, 0)) {
		return ErrTokenExpired
	}
	if claims.Nbf != 0 && now.Before(time.Unix(claims.Nbf, 0)) {
		return ErrTokenNotValidYet
	}
	return nil
}

func audienceMatches(value interface{}, expected string) bool {
	switch typed := value.(type) {
	case string:
		return typed == expected
	case []interface{}:
		for _, item := range typed {
			if itemString, ok := item.(string); ok && itemString == expected {
				return true
			}
		}
	}
	return false
}

func (v *OIDCVerifier) jwksURI(ctx context.Context, issuer string) (string, error) {
	now := v.now()

	v.mu.Lock()
	if cached, ok := v.discovery[issuer]; ok && now.Before(cached.expires) {
		v.mu.Unlock()
		return cached.jwksURI, nil
	}
	v.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", err
	}
	res, err := v.client.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc discovery status %d", res.StatusCode)
	}

	var doc discoveryDocument
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		return "", err
	}
	if doc.JWKSURI == "" {
		return "", fmt.Errorf("%w: discovery document missing jwks_uri", ErrInvalidToken)
	}

	v.mu.Lock()
	v.discovery[issuer] = cachedDiscovery{jwksURI: doc.JWKSURI, expires: now.Add(oidcCacheTTL)}
	v.mu.Unlock()

	return doc.JWKSURI, nil
}

func (v *OIDCVerifier) signingKey(ctx context.Context, jwksURI, kid string, forceRefresh bool) (*rsa.PublicKey, error) {
	keys, err := v.jwks(ctx, jwksURI, forceRefresh)
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		if key.Kid != kid {
			continue
		}
		if key.Kty != "RSA" || (key.Use != "" && key.Use != "sig") || (key.Alg != "" && key.Alg != "RS256") {
			return nil, ErrInvalidToken
		}
		return rsaPublicKey(key)
	}
	if !forceRefresh {
		return v.signingKey(ctx, jwksURI, kid, true)
	}
	return nil, fmt.Errorf("%w: signing key not found", ErrInvalidSignature)
}

func (v *OIDCVerifier) jwks(ctx context.Context, jwksURI string, forceRefresh bool) ([]jwkKey, error) {
	now := v.now()

	v.mu.Lock()
	if cached, ok := v.keys[jwksURI]; ok && !forceRefresh && now.Before(cached.expires) {
		v.mu.Unlock()
		return cached.keys, nil
	}
	v.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return nil, err
	}
	res, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks status %d", res.StatusCode)
	}

	var doc jwksDocument
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		return nil, err
	}
	if len(doc.Keys) == 0 {
		return nil, fmt.Errorf("%w: jwks has no keys", ErrInvalidToken)
	}

	v.mu.Lock()
	v.keys[jwksURI] = cachedJWKS{keys: doc.Keys, expires: now.Add(oidcCacheTTL)}
	v.mu.Unlock()

	return doc.Keys, nil
}

func rsaPublicKey(key jwkKey) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(key.N)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid rsa modulus", ErrInvalidToken)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(key.E)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid rsa exponent", ErrInvalidToken)
	}
	exponent := 0
	for _, b := range eBytes {
		exponent = exponent<<8 + int(b)
	}
	if exponent == 0 {
		return nil, fmt.Errorf("%w: invalid rsa exponent", ErrInvalidToken)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: exponent}, nil
}

func verifyRS256(signingInput, signature []byte, key *rsa.PublicKey) error {
	digest := sha256.Sum256(signingInput)
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return ErrInvalidSignature
	}
	return nil
}
