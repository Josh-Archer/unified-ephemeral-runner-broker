package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrMissingBearerToken = errors.New("missing bearer token")
	ErrInvalidAudience    = errors.New("invalid oidc audience")
	ErrInvalidIssuer      = errors.New("invalid oidc issuer")
	ErrInvalidSubject     = errors.New("missing oidc subject")
)

type OIDCClaims struct {
	Iss string      `json:"iss"`
	Aud interface{} `json:"aud"`
	Sub string      `json:"sub"`
}

func ExtractClaimsUnverified(rawToken string) (OIDCClaims, error) {
	parts := strings.Split(rawToken, ".")
	if len(parts) != 3 {
		return OIDCClaims{}, fmt.Errorf("expected JWT with three parts")
	}

	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return OIDCClaims{}, err
	}

	var claims OIDCClaims
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return OIDCClaims{}, err
	}
	return claims, nil
}

func ValidateOIDCClaims(claims OIDCClaims, expectedAudience string, allowedIssuers []string) error {
	if claims.Sub == "" {
		return ErrInvalidSubject
	}

	if expectedAudience != "" && !audienceMatches(claims.Aud, expectedAudience) {
		return ErrInvalidAudience
	}

	if len(allowedIssuers) > 0 {
		for _, issuer := range allowedIssuers {
			if claims.Iss == issuer {
				return nil
			}
		}
		return ErrInvalidIssuer
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
