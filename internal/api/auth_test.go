package api

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestVerifyTokenAcceptsSignedGitHubOIDCToken(t *testing.T) {
	key := mustRSAKey(t)
	issuer := newOIDCTestIssuer(t, key, "test-key")
	now := time.Unix(2000, 0)
	token := signedOIDCToken(t, key, "test-key", OIDCClaims{
		Iss: issuer.URL,
		Aud: "uecb-broker",
		Sub: "repo:example/repo:ref",
		Exp: now.Add(time.Hour).Unix(),
		Nbf: now.Add(-time.Minute).Unix(),
	})

	verifier := NewOIDCVerifier()
	verifier.now = func() time.Time { return now }

	claims, err := verifier.VerifyToken(context.Background(), token, "uecb-broker", []string{issuer.URL})
	if err != nil {
		t.Fatalf("expected signed token to verify: %v", err)
	}
	if claims.Sub != "repo:example/repo:ref" {
		t.Fatalf("unexpected subject %q", claims.Sub)
	}
}

func TestVerifyTokenRejectsForgedThreePartToken(t *testing.T) {
	key := mustRSAKey(t)
	issuer := newOIDCTestIssuer(t, key, "test-key")
	now := time.Unix(2000, 0)
	payload := fmt.Sprintf(`{"iss":%q,"aud":"uecb-broker","sub":"repo:example/repo:ref","exp":%d}`, issuer.URL, now.Add(time.Hour).Unix())
	token := fmt.Sprintf("header.%s.signature", base64.RawURLEncoding.EncodeToString([]byte(payload)))

	verifier := NewOIDCVerifier()
	verifier.now = func() time.Time { return now }

	if _, err := verifier.VerifyToken(context.Background(), token, "uecb-broker", []string{issuer.URL}); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expected invalid token error for forged header, got %v", err)
	}
}

func TestVerifyTokenRejectsTamperedSignature(t *testing.T) {
	key := mustRSAKey(t)
	issuer := newOIDCTestIssuer(t, key, "test-key")
	now := time.Unix(2000, 0)
	token := signedOIDCToken(t, key, "test-key", OIDCClaims{
		Iss: issuer.URL,
		Aud: "uecb-broker",
		Sub: "repo:example/repo:ref",
		Exp: now.Add(time.Hour).Unix(),
	})
	token += "tamper"

	verifier := NewOIDCVerifier()
	verifier.now = func() time.Time { return now }

	if _, err := verifier.VerifyToken(context.Background(), token, "uecb-broker", []string{issuer.URL}); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("expected invalid signature error, got %v", err)
	}
}

func TestVerifyTokenRejectsExpiredToken(t *testing.T) {
	key := mustRSAKey(t)
	issuer := newOIDCTestIssuer(t, key, "test-key")
	now := time.Unix(2000, 0)
	token := signedOIDCToken(t, key, "test-key", OIDCClaims{
		Iss: issuer.URL,
		Aud: "uecb-broker",
		Sub: "repo:example/repo:ref",
		Exp: now.Add(-time.Second).Unix(),
	})

	verifier := NewOIDCVerifier()
	verifier.now = func() time.Time { return now }

	if _, err := verifier.VerifyToken(context.Background(), token, "uecb-broker", []string{issuer.URL}); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expected expired token error, got %v", err)
	}
}

func TestValidateOIDCClaims(t *testing.T) {
	claims := OIDCClaims{
		Iss: "https://token.actions.githubusercontent.com",
		Aud: "uecb-broker",
		Sub: "repo:example/repo:ref",
	}

	if err := ValidateOIDCClaims(claims, "uecb-broker", []string{"https://token.actions.githubusercontent.com"}); err != nil {
		t.Fatalf("expected claims to validate: %v", err)
	}

	if err := ValidateOIDCClaims(claims, "different", []string{"https://token.actions.githubusercontent.com"}); err != ErrInvalidAudience {
		t.Fatalf("expected invalid audience error, got %v", err)
	}
}

func TestOIDCClaimsEffectiveRepositoryAndOwner(t *testing.T) {
	claims := OIDCClaims{
		Sub:             "repo:acme/widgets:ref:refs/heads/main",
		Repository:      "acme/widgets",
		RepositoryOwner: "acme",
	}
	if claims.EffectiveRepository() != "acme/widgets" {
		t.Fatalf("unexpected repository %q", claims.EffectiveRepository())
	}
	if claims.EffectiveOwner() != "acme" {
		t.Fatalf("unexpected owner %q", claims.EffectiveOwner())
	}

	derived := OIDCClaims{Sub: "repo:acme/widgets:environment:prod"}
	if derived.EffectiveRepository() != "acme/widgets" {
		t.Fatalf("expected repo derived from sub, got %q", derived.EffectiveRepository())
	}
	if derived.EffectiveOwner() != "acme" {
		t.Fatalf("expected owner derived from sub, got %q", derived.EffectiveOwner())
	}
}

func TestAuthorizeOIDCPolicy(t *testing.T) {
	claims := OIDCClaims{
		Sub:             "repo:acme/widgets:ref:refs/heads/main",
		Repository:      "acme/widgets",
		RepositoryOwner: "acme",
	}

	if err := AuthorizeOIDCPolicy(claims, model.OIDCPolicyConfig{}); err != nil {
		t.Fatalf("empty policy should allow: %v", err)
	}
	if err := AuthorizeOIDCPolicy(claims, model.OIDCPolicyConfig{
		AllowedRepositories: []string{"acme/widgets"},
	}); err != nil {
		t.Fatalf("exact repository allowlist should allow: %v", err)
	}
	if err := AuthorizeOIDCPolicy(claims, model.OIDCPolicyConfig{
		AllowedRepositories: []string{"acme/*"},
	}); err != nil {
		t.Fatalf("wildcard repository allowlist should allow: %v", err)
	}
	if err := AuthorizeOIDCPolicy(claims, model.OIDCPolicyConfig{
		AllowedOwners: []string{"acme"},
	}); err != nil {
		t.Fatalf("owner allowlist should allow: %v", err)
	}
	if err := AuthorizeOIDCPolicy(claims, model.OIDCPolicyConfig{
		AllowedRepositories: []string{"other/repo"},
		AllowedOwners:       []string{"other-org"},
	}); !errors.Is(err, ErrOIDCPolicyDenied) {
		t.Fatalf("expected policy denial, got %v", err)
	}
	// Union: owner match is enough even when repository list does not match.
	if err := AuthorizeOIDCPolicy(claims, model.OIDCPolicyConfig{
		AllowedRepositories: []string{"other/repo"},
		AllowedOwners:       []string{"acme"},
	}); err != nil {
		t.Fatalf("owner match should allow via union: %v", err)
	}
}

func TestOwnershipAllows(t *testing.T) {
	status := model.AllocationStatus{
		Subject:    "repo:acme/widgets:ref:refs/heads/main",
		Repository: "acme/widgets",
	}
	if !OwnershipAllows(OIDCClaims{Sub: status.Subject}, status) {
		t.Fatal("same subject should be allowed")
	}
	if OwnershipAllows(OIDCClaims{Sub: "repo:other/repo:ref:refs/heads/main", Repository: "other/repo"}, status) {
		t.Fatal("cross-tenant subject should be denied")
	}
	if !OwnershipAllows(OIDCClaims{Sub: "repo:acme/widgets:pull_request", Repository: "acme/widgets"}, status) {
		t.Fatal("same repository should be allowed as ownership fallback")
	}
	if !OwnershipAllows(OIDCClaims{Sub: "anyone"}, model.AllocationStatus{}) {
		t.Fatal("unbound allocation should allow any caller")
	}
	if OwnershipAllows(OIDCClaims{}, status) {
		t.Fatal("empty subject should not access bound allocation")
	}
}

func mustRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func newOIDCTestIssuer(t *testing.T, key *rsa.PrivateKey, kid string) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"jwks_uri": server.URL + "/.well-known/jwks",
		})
	})
	mux.HandleFunc("/.well-known/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwksDocument{
			Keys: []jwkKey{{
				Kty: "RSA",
				Use: "sig",
				Kid: kid,
				Alg: "RS256",
				N:   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
				E:   base64.RawURLEncoding.EncodeToString(bigEndianInt(key.PublicKey.E)),
			}},
		})
	})

	return server
}

func signedOIDCToken(t *testing.T, key *rsa.PrivateKey, kid string, claims OIDCClaims) string {
	t.Helper()

	headerBytes, err := json.Marshal(jwtHeader{Alg: "RS256", Kid: kid, Typ: "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}

	header := base64.RawURLEncoding.EncodeToString(headerBytes)
	payload := base64.RawURLEncoding.EncodeToString(claimsBytes)
	signingInput := []byte(header + "." + payload)
	digest := sha256.Sum256(signingInput)
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}

	return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func bigEndianInt(value int) []byte {
	if value == 0 {
		return []byte{0}
	}
	var encoded []byte
	for value > 0 {
		encoded = append([]byte{byte(value)}, encoded...)
		value >>= 8
	}
	return encoded
}
