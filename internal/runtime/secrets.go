package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

const (
	serviceAccountRoot = "/var/run/secrets/kubernetes.io/serviceaccount"
	tokenFileName      = "token"
	caFileName         = "ca.crt"
)

var errMissingNamespace = errors.New("UECB_POD_NAMESPACE is not set")

type HealthChecker interface {
	Check(ctx context.Context) error
}

type noopChecker struct{}

func (noopChecker) Check(context.Context) error {
	return nil
}

type SecretRefChecker struct {
	namespace string
	baseURL   string
	client    *http.Client
	refs      []string
	token     string
}

func NewSecretRefCheckerFromEnv(cfg model.BrokerConfig) (HealthChecker, error) {
	namespace := strings.TrimSpace(os.Getenv("UECB_POD_NAMESPACE"))
	if namespace == "" {
		return noopChecker{}, nil
	}

	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	if host == "" || port == "" {
		return nil, fmt.Errorf("kubernetes service host/port are not set")
	}

	tokenBytes, err := os.ReadFile(filepath.Join(serviceAccountRoot, tokenFileName))
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}

	caBytes, err := os.ReadFile(filepath.Join(serviceAccountRoot, caFileName))
	if err != nil {
		return nil, fmt.Errorf("read service account CA: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, errors.New("append kubernetes service account CA cert")
	}

	refs := requiredSecretRefs(cfg)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    pool,
			},
		},
	}

	return &SecretRefChecker{
		namespace: namespace,
		baseURL:   fmt.Sprintf("https://%s:%s", host, port),
		client:    client,
		refs:      refs,
		token:     strings.TrimSpace(string(tokenBytes)),
	}, nil
}

func (c *SecretRefChecker) Check(ctx context.Context) error {
	missing := make([]string, 0)
	for _, ref := range c.refs {
		ok, err := c.secretExists(ctx, ref)
		if err != nil {
			return err
		}
		if !ok {
			missing = append(missing, ref)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing required kubernetes secrets in namespace %s: %s", c.namespace, strings.Join(missing, ", "))
	}

	return nil
}

func (c *SecretRefChecker) secretExists(ctx context.Context, name string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.secretURL(name), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("query kubernetes secret %s: %w", name, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("query kubernetes secret %s: unexpected status %d", name, resp.StatusCode)
	}
}

func (c *SecretRefChecker) secretURL(name string) string {
	escapedNamespace := url.PathEscape(c.namespace)
	escapedName := url.PathEscape(name)
	return fmt.Sprintf("%s/api/v1/namespaces/%s/secrets/%s", c.baseURL, escapedNamespace, escapedName)
}

func requiredSecretRefs(cfg model.BrokerConfig) []string {
	refs := map[string]struct{}{}

	if ref := strings.TrimSpace(cfg.GitHub.Auth.SecretRef); ref != "" {
		refs[ref] = struct{}{}
	}

	for _, pool := range cfg.Pools {
		for _, backend := range pool.Backends {
			if !backend.Enabled {
				continue
			}
			if ref := strings.TrimSpace(backend.SecretRef); ref != "" {
				refs[ref] = struct{}{}
			}
		}
	}

	ordered := make([]string, 0, len(refs))
	for ref := range refs {
		ordered = append(ordered, ref)
	}
	sort.Strings(ordered)
	return ordered
}
