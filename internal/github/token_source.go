package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// AppIDKey is the GitHub App ID in github.auth.secretRef.
	AppIDKey = "app_id"
	// InstallationIDKey is the App installation ID in github.auth.secretRef.
	InstallationIDKey = "installation_id"
	// PrivateKeyKey is the PEM-encoded RS256 private key in github.auth.secretRef.
	PrivateKeyKey = "private_key"

	githubAppAuthMode = "github-app"

	// Installation tokens last one hour. Refresh this long before expiry so
	// in-flight requests are not surprised by a near-expiry token.
	installationRefreshLead = 5 * time.Minute
	defaultInstallationTTL  = time.Hour
)

// TokenSource yields a GitHub bearer token. Client methods call Token for
// each request; a source may cache and refresh tokens itself.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

type tokenFuncSource struct {
	fn TokenFunc
}

func (s tokenFuncSource) Token(ctx context.Context) (string, error) {
	if s.fn == nil {
		return "", errors.New("github client is not configured")
	}
	return s.fn(ctx)
}

// SecretTokenSource loads GitHub credentials from github.auth.secretRef.
// When mode is github-app it mints an App JWT and exchanges it for an
// installation token if a private_key is present together with an app ID
// and installation ID. Those IDs come from github.auth.appId and
// github.auth.installationId, or from secret keys app_id and
// installation_id when those keys are set. Cached installation tokens are
// refreshed about five minutes before they expire. If App credentials are
// incomplete it returns github_token. Token and key material are never logged.
type SecretTokenSource struct {
	reader         SecretReader
	name           string
	mode           string
	appID          string
	installationID string
	baseURL        string
	http           *http.Client
	now            func() time.Time
	logf           func(string, ...any)

	mu         sync.Mutex
	cached     string
	expiry     time.Time
	loggedKind string
}

// AuthFromSecret builds a TokenSource for github.auth.secretRef and mode.
// appID and installationID are used when the secret does not contain
// those keys.
func AuthFromSecret(reader SecretReader, name, mode, appID, installationID string) *SecretTokenSource {
	return &SecretTokenSource{
		reader:         reader,
		name:           strings.TrimSpace(name),
		mode:           strings.TrimSpace(mode),
		appID:          strings.TrimSpace(appID),
		installationID: strings.TrimSpace(installationID),
		baseURL:        defaultAPIURL,
		http:           &http.Client{Timeout: 15 * time.Second},
		now:            time.Now,
		logf:           log.Printf,
	}
}

// WithBaseURL points token exchange at another API root (tests, GHES).
func (s *SecretTokenSource) WithBaseURL(base string) *SecretTokenSource {
	if s != nil {
		s.baseURL = strings.TrimRight(base, "/")
	}
	return s
}

// Invalidate drops a cached installation token so the next Token call
// reloads the secret and mints or falls back again.
func (s *SecretTokenSource) Invalidate() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cached = ""
	s.expiry = time.Time{}
	s.mu.Unlock()
}

// Token returns a current installation token or the fallback PAT.
func (s *SecretTokenSource) Token(ctx context.Context) (string, error) {
	if s == nil {
		return "", errors.New("github client is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.clock()
	if s.cached != "" && now.Add(installationRefreshLead).Before(s.expiry) {
		return s.cached, nil
	}
	token, expiry, kind, err := s.refreshLocked(ctx)
	if err != nil {
		if s.cached != "" && now.Before(s.expiry) {
			return s.cached, nil
		}
		return "", err
	}
	if kind == "app" {
		s.cached = token
		s.expiry = expiry
	} else {
		// Do not cache a PAT, so a later secret update can switch to App keys
		// without waiting for a previous installation token to expire.
		s.cached = ""
		s.expiry = time.Time{}
	}
	return token, nil
}

func (s *SecretTokenSource) refreshLocked(ctx context.Context) (token string, expiry time.Time, kind string, err error) {
	if s.reader == nil || s.name == "" {
		return "", time.Time{}, "", errors.New("github.auth.secretRef is not configured")
	}
	data, err := s.reader.ReadSecret(ctx, s.name)
	if err != nil {
		return "", time.Time{}, "", fmt.Errorf("read github token secret %s: %w", s.name, err)
	}
	if githubAppModeEnabled(s.mode) {
		appID := firstNonEmpty(data[AppIDKey], s.appID)
		instID := firstNonEmpty(data[InstallationIDKey], s.installationID)
		pemKey := strings.TrimSpace(data[PrivateKeyKey])
		if appID != "" && instID != "" && pemKey != "" {
			token, expiry, err = s.mintInstallationToken(ctx, appID, instID, pemKey)
			if err != nil {
				return "", time.Time{}, "", err
			}
			s.announce("app installation " + instID)
			return token, expiry, "app", nil
		}
	}
	token = strings.TrimSpace(data[DefaultTokenKey])
	if token == "" {
		if githubAppModeEnabled(s.mode) {
			return "", time.Time{}, "", fmt.Errorf("github token secret %s has neither App credentials (%s, %s, %s) nor %q", s.name, AppIDKey, InstallationIDKey, PrivateKeyKey, DefaultTokenKey)
		}
		return "", time.Time{}, "", fmt.Errorf("github token secret %s has no %q", s.name, DefaultTokenKey)
	}
	s.announce("token (fallback)")
	return token, time.Time{}, "token", nil
}

func (s *SecretTokenSource) mintInstallationToken(ctx context.Context, appID, installationID, pemKey string) (string, time.Time, error) {
	key, err := parseRSAPrivateKey([]byte(pemKey))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parse github app private_key: %w", err)
	}
	jwt, err := mintAppJWT(appID, key, s.clock())
	if err != nil {
		return "", time.Time{}, fmt.Errorf("mint github app jwt: %w", err)
	}
	endpoint := s.apiURL() + "/app/installations/" + url.PathEscape(installationID) + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("create installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &msg)
		if msg.Message == "" {
			msg.Message = "unexpected response"
		}
		return "", time.Time{}, fmt.Errorf("create installation token: github api status %d: %s", resp.StatusCode, msg.Message)
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, errors.New("create installation token: decode response")
	}
	if strings.TrimSpace(out.Token) == "" {
		return "", time.Time{}, errors.New("create installation token: empty token")
	}
	expiry := s.clock().Add(defaultInstallationTTL)
	if parsed, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
		expiry = parsed
	}
	return out.Token, expiry, nil
}

func (s *SecretTokenSource) announce(kind string) {
	if s.loggedKind == kind {
		return
	}
	s.loggedKind = kind
	if s.logf != nil {
		s.logf("github auth: %s", kind)
	}
}

func (s *SecretTokenSource) clock() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *SecretTokenSource) apiURL() string {
	if s != nil && s.baseURL != "" {
		return strings.TrimRight(s.baseURL, "/")
	}
	return defaultAPIURL
}

func (s *SecretTokenSource) httpClient() *http.Client {
	if s != nil && s.http != nil {
		return s.http
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func githubAppModeEnabled(mode string) bool {
	m := strings.TrimSpace(mode)
	return m == "" || strings.EqualFold(m, githubAppAuthMode)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
