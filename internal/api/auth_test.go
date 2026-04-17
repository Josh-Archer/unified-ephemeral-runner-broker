package api

import (
	"encoding/base64"
	"fmt"
	"testing"
)

func buildToken(payload string) string {
	return fmt.Sprintf("header.%s.signature", base64.RawURLEncoding.EncodeToString([]byte(payload)))
}

func TestExtractClaimsUnverified(t *testing.T) {
	token := buildToken(`{"iss":"https://token.actions.githubusercontent.com","aud":"uecb-broker","sub":"repo:example/repo:ref"}`)

	claims, err := ExtractClaimsUnverified(token)
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}

	if claims.Iss != "https://token.actions.githubusercontent.com" || claims.Sub == "" {
		t.Fatalf("unexpected claims: %#v", claims)
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
