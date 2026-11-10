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
	"errors"
	"fmt"
	"strconv"
	"time"
)

const (
	// GitHub requires exp no more than 10 minutes after iat. iat is set 60
	// seconds in the past to tolerate clock drift, so exp is now+9m.
	appJWTTTL        = 10 * time.Minute
	appJWTIssuedSkew = 60 * time.Second
)

func mintAppJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	if key == nil {
		return "", errors.New("private key is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	iat := now.Add(-appJWTIssuedSkew).Unix()
	exp := now.Add(-appJWTIssuedSkew).Add(appJWTTTL).Unix()

	var iss any = appID
	if n, err := strconv.ParseInt(appID, 10, 64); err == nil {
		iss = n
	}

	header, err := encodeJWTPart(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := encodeJWTPart(map[string]any{"iat": iat, "exp": exp, "iss": iss})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + claims
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func encodeJWTPart(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func parseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("private_key is not PEM")
	}
	_ = rest
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("private_key must be PKCS#1 or PKCS#8 RSA PEM: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private_key is not an RSA key")
	}
	return key, nil
}
