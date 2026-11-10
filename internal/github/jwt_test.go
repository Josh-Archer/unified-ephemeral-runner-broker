package github

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func testRSAKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return key, string(pemBytes)
}

func pkcs8PEM(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func decodeJWTPart(t *testing.T, part string, out any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatalf("decode jwt part: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal jwt part: %v", err)
	}
}

func TestMintAppJWTClaimsAndSignature(t *testing.T) {
	key, _ := testRSAKey(t)
	now := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	token, err := mintAppJWT("424242", key, now)
	if err != nil {
		t.Fatalf("mintAppJWT: %v", err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt parts = %d", len(parts))
	}

	header := map[string]any{}
	decodeJWTPart(t, parts[0], &header)
	if header["alg"] != "RS256" || header["typ"] != "JWT" {
		t.Fatalf("header = %#v", header)
	}

	claims := map[string]any{}
	decodeJWTPart(t, parts[1], &claims)
	if claims["iss"] != float64(424242) {
		t.Fatalf("iss = %#v", claims["iss"])
	}
	iat := int64(claims["iat"].(float64))
	exp := int64(claims["exp"].(float64))
	wantIat := now.Add(-60 * time.Second).Unix()
	if iat != wantIat {
		t.Fatalf("iat = %d, want %d", iat, wantIat)
	}
	if exp-iat != int64((10 * time.Minute).Seconds()) {
		t.Fatalf("exp-iat = %d, want 600", exp-iat)
	}
	if exp > now.Add(10*time.Minute).Unix() {
		t.Fatalf("exp %d is more than 10m after now", exp)
	}

	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature verify: %v", err)
	}
}

func TestParseRSAPrivateKeyPKCS1AndPKCS8(t *testing.T) {
	key, pkcs1 := testRSAKey(t)
	got, err := parseRSAPrivateKey([]byte(pkcs1))
	if err != nil || got.N.Cmp(key.N) != 0 {
		t.Fatalf("pkcs1: got %v err %v", got != nil, err)
	}
	got, err = parseRSAPrivateKey([]byte(pkcs8PEM(t, key)))
	if err != nil || got.N.Cmp(key.N) != 0 {
		t.Fatalf("pkcs8: got %v err %v", got != nil, err)
	}
	if _, err := parseRSAPrivateKey([]byte("not-a-key")); err == nil {
		t.Fatal("expected PEM parse error")
	}
}
