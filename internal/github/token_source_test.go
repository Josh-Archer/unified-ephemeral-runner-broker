package github

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type liveSecrets struct {
	mu   sync.Mutex
	data map[string]string
}

func (s *liveSecrets) ReadSecret(_ context.Context, name string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name != "gh" {
		return nil, fmt.Errorf("not found")
	}
	out := make(map[string]string, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out, nil
}

func (s *liveSecrets) set(data map[string]string) {
	s.mu.Lock()
	s.data = data
	s.mu.Unlock()
}

type tokenServer struct {
	now         time.Time
	key         *rsa.PrivateKey
	mints       atomic.Int32
	apiCalls    atomic.Int32
	hold        chan struct{}
	started     chan struct{}
	startedOnce sync.Once
}

func (ts *tokenServer) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens") {
			if ts.hold != nil {
				ts.startedOnce.Do(func() {
					if ts.started != nil {
						close(ts.started)
					}
				})
				<-ts.hold
			}
			auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			verifyTestJWT(t, auth, ts.key)
			n := ts.mints.Add(1)
			inst := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/app/installations/"), "/access_tokens")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"inst-%s-%d","expires_at":"%s"}`, inst, n, ts.now.Add(time.Hour).UTC().Format(time.RFC3339))
			return
		}
		n := ts.apiCalls.Add(1)
		switch r.URL.Path {
		case "/repos/owner/repo/actions/runs/1":
			if r.Header.Get("Authorization") == "Bearer inst-180390269-1" && n == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
				return
			}
			if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer inst-") {
				t.Errorf("authorization = %q", got)
			}
			_, _ = w.Write([]byte(`{"head_repository":{"full_name":"owner/repo"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	})
}

func verifyTestJWT(t *testing.T, token string, key *rsa.PrivateKey) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt parts = %d", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatalf("claims json: %v", err)
	}
	if claims["iss"] == nil {
		t.Fatal("jwt missing iss")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("sig: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("jwt signature: %v", err)
	}
}

func newTestSource(t *testing.T, reader SecretReader, appID, installationID string, srv *httptest.Server, now time.Time) (*SecretTokenSource, *[]string) {
	t.Helper()
	var logs []string
	src := AuthFromSecret(reader, "gh", "github-app", appID, installationID).WithBaseURL(srv.URL)
	src.now = func() time.Time { return now }
	src.logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	return src, &logs
}

func assertNoSecrets(t *testing.T, logs []string, secrets ...string) {
	t.Helper()
	joined := strings.Join(logs, "\n")
	for _, secret := range secrets {
		if secret != "" && strings.Contains(joined, secret) {
			t.Fatalf("logs leaked secret material")
		}
	}
	if strings.Contains(joined, "BEGIN") || strings.Contains(joined, "PRIVATE KEY") {
		t.Fatalf("logs leaked key material")
	}
}

func TestAuthFromSecretUsesConfigIDsAndPrivateKey(t *testing.T) {
	key, pemKey := testRSAKey(t)
	now := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	ts := &tokenServer{now: now, key: key}
	srv := httptest.NewServer(ts.handler(t))
	defer srv.Close()

	src, logs := newTestSource(t, secrets{"gh": {PrivateKeyKey: pemKey}}, "5691181", "180390269", srv, now)
	tok, err := src.Token(context.Background())
	if err != nil || tok != "inst-180390269-1" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if ts.mints.Load() != 1 {
		t.Fatalf("mints = %d", ts.mints.Load())
	}
	if got := strings.Join(*logs, "\n"); got != "github auth: app installation 180390269" {
		t.Fatalf("logs = %q", got)
	}
	assertNoSecrets(t, *logs, pemKey, tok)
}

func TestAuthFromSecretPrefersSecretIDsOverConfig(t *testing.T) {
	key, pemKey := testRSAKey(t)
	now := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	ts := &tokenServer{now: now, key: key}
	srv := httptest.NewServer(ts.handler(t))
	defer srv.Close()

	src, _ := newTestSource(t, secrets{"gh": {
		AppIDKey:          "111",
		InstallationIDKey: "222",
		PrivateKeyKey:     pemKey,
	}}, "5691181", "180390269", srv, now)
	tok, err := src.Token(context.Background())
	if err != nil || tok != "inst-222-1" {
		t.Fatalf("token = %q, %v", tok, err)
	}
}

func TestAuthFromSecretCachesAndRefreshesInstallationToken(t *testing.T) {
	key, pemKey := testRSAKey(t)
	now := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	ts := &tokenServer{now: now, key: key}
	srv := httptest.NewServer(ts.handler(t))
	defer srv.Close()

	current := now
	src, _ := newTestSource(t, secrets{"gh": {PrivateKeyKey: pemKey}}, "5691181", "180390269", srv, now)
	src.now = func() time.Time { return current }

	first, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := src.Token(context.Background())
	if err != nil || second != first || ts.mints.Load() != 1 {
		t.Fatalf("cached token = %q (first %q) mints=%d err=%v", second, first, ts.mints.Load(), err)
	}
	current = now.Add(56 * time.Minute)
	third, err := src.Token(context.Background())
	if err != nil || third == first || ts.mints.Load() != 2 {
		t.Fatalf("refreshed token = %q (first %q) mints=%d err=%v", third, first, ts.mints.Load(), err)
	}
}

func TestAuthFromSecretFallbackToPAT(t *testing.T) {
	now := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	ts := &tokenServer{now: now}
	srv := httptest.NewServer(ts.handler(t))
	defer srv.Close()

	src, logs := newTestSource(t, secrets{"gh": {DefaultTokenKey: "pat-fallback-value"}}, "5691181", "180390269", srv, now)
	tok, err := src.Token(context.Background())
	if err != nil || tok != "pat-fallback-value" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if ts.mints.Load() != 0 {
		t.Fatalf("unexpected mint")
	}
	if got := strings.Join(*logs, "\n"); got != "github auth: token (fallback)" {
		t.Fatalf("logs = %q", got)
	}
	assertNoSecrets(t, *logs, "pat-fallback-value")
}

func TestAuthFromSecretReloadsSecretAfterInvalidate(t *testing.T) {
	key, pemKey := testRSAKey(t)
	now := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	ts := &tokenServer{now: now, key: key}
	srv := httptest.NewServer(ts.handler(t))
	defer srv.Close()

	store := &liveSecrets{data: map[string]string{PrivateKeyKey: pemKey}}
	src, logs := newTestSource(t, store, "5691181", "180390269", srv, now)
	first, err := src.Token(context.Background())
	if err != nil || first != "inst-180390269-1" {
		t.Fatalf("first = %q, %v", first, err)
	}
	store.set(map[string]string{DefaultTokenKey: "pat-after-reload"})
	cached, err := src.Token(context.Background())
	if err != nil || cached != first {
		t.Fatalf("expected cache before invalidate, got %q, %v", cached, err)
	}
	src.Invalidate()
	second, err := src.Token(context.Background())
	if err != nil || second != "pat-after-reload" {
		t.Fatalf("after reload = %q, %v", second, err)
	}
	if got := strings.Join(*logs, "\n"); !strings.Contains(got, "github auth: app installation 180390269") || !strings.Contains(got, "github auth: token (fallback)") {
		t.Fatalf("logs = %q", got)
	}
	assertNoSecrets(t, *logs, pemKey, first, "pat-after-reload")
}

func TestAuthFromSecretSingleFlightRefresh(t *testing.T) {
	key, pemKey := testRSAKey(t)
	now := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	ts := &tokenServer{now: now, key: key, hold: make(chan struct{}), started: make(chan struct{})}
	srv := httptest.NewServer(ts.handler(t))
	defer srv.Close()

	src, _ := newTestSource(t, secrets{"gh": {PrivateKeyKey: pemKey}}, "1", "99", srv, now)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := src.Token(context.Background())
			if err != nil || tok != "inst-99-1" {
				errs <- fmt.Errorf("token = %q, %v", tok, err)
			}
		}()
	}
	select {
	case <-ts.started:
	case <-time.After(2 * time.Second):
		t.Fatal("mint did not start")
	}
	close(ts.hold)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if ts.mints.Load() != 1 {
		t.Fatalf("mints = %d, want 1", ts.mints.Load())
	}
}

func TestClientRetriesOnceAfter401(t *testing.T) {
	key, pemKey := testRSAKey(t)
	now := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	ts := &tokenServer{now: now, key: key}
	srv := httptest.NewServer(ts.handler(t))
	defer srv.Close()

	src, _ := newTestSource(t, secrets{"gh": {PrivateKeyKey: pemKey}}, "5691181", "180390269", srv, now)
	client := NewClientWithSource(src, nil).WithBaseURL(srv.URL)
	run, err := client.GetWorkflowRun(context.Background(), "owner/repo", 1)
	if err != nil || run.HeadRepository != "owner/repo" {
		t.Fatalf("run = %+v, %v", run, err)
	}
	if ts.mints.Load() != 2 {
		t.Fatalf("mints = %d, want 2", ts.mints.Load())
	}
	if ts.apiCalls.Load() != 2 {
		t.Fatalf("api calls = %d, want 2", ts.apiCalls.Load())
	}
}

func TestAuthFromSecretMissingCredentials(t *testing.T) {
	src := AuthFromSecret(secrets{"gh": {}}, "gh", "github-app", "", "")
	_, err := src.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "neither App credentials") {
		t.Fatalf("err = %v", err)
	}
}
