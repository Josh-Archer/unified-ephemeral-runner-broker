package runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func TestRequiredSecretRefsSkipsDisabledBackends(t *testing.T) {
	cfg := config.Default()
	pool := cfg.Pools[1]
	lambdaCfg := pool.Backends[model.BackendLambda]
	lambdaCfg.Enabled = true
	pool.Backends[model.BackendLambda] = lambdaCfg
	cfg.Pools[1] = pool

	refs := requiredSecretRefs(cfg)
	got := strings.Join(refs, ",")
	want := "uecb-github-app,uecb-lambda"
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}

func TestSecretRefCheckerReportsMissingSecret(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()

	checker := &SecretRefChecker{
		namespace: "arc-systems",
		baseURL:   server.URL,
		client:    server.Client(),
		refs:      []string{"uecb-github-app"},
		token:     "test",
	}

	err := checker.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "uecb-github-app") {
		t.Fatalf("expected missing secret error, got %v", err)
	}
}

func TestNewSecretRefCheckerFromEnvReturnsNoopWithoutNamespace(t *testing.T) {
	t.Setenv("UECB_POD_NAMESPACE", "")
	checker, err := NewSecretRefCheckerFromEnv(config.Default())
	if err != nil {
		t.Fatalf("expected noop checker, got error %v", err)
	}
	if _, ok := checker.(noopChecker); !ok {
		t.Fatalf("expected noopChecker, got %T", checker)
	}
}
