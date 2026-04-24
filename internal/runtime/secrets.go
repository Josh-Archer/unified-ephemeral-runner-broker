package runtime

import (
	"encoding/base64"
	"encoding/json"
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

type SecretReader interface {
	ReadSecret(ctx context.Context, name string) (map[string]string, error)
}

type noopChecker struct{}

func (noopChecker) Check(context.Context) error {
	return nil
}

type noopSecretReader struct{}

func (noopSecretReader) ReadSecret(context.Context, string) (map[string]string, error) {
	return nil, errors.New("kubernetes secret reader is not configured")
}

type kubernetesSecretClient struct {
	namespace string
	baseURL   string
	client    *http.Client
	token     string
}

type SecretRefChecker struct {
	client *kubernetesSecretClient
	refs   []string
}

func NewSecretReaderFromEnv() (SecretReader, error) {
	namespace := strings.TrimSpace(os.Getenv("UECB_POD_NAMESPACE"))
	if namespace == "" {
		return noopSecretReader{}, nil
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

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				RootCAs:    pool,
			},
		},
	}

	return &kubernetesSecretClient{
		namespace: namespace,
		baseURL:   fmt.Sprintf("https://%s:%s", host, port),
		client:    client,
		token:     strings.TrimSpace(string(tokenBytes)),
	}, nil
}

func NewSecretRefCheckerFromEnv(cfg model.BrokerConfig) (HealthChecker, error) {
	client, err := NewSecretReaderFromEnv()
	if err != nil {
		return nil, err
	}

	kubeClient, ok := client.(*kubernetesSecretClient)
	if !ok {
		return noopChecker{}, nil
	}

	return &SecretRefChecker{
		client: kubeClient,
		refs:   requiredSecretRefs(cfg),
	}, nil
}

func (c *SecretRefChecker) Check(ctx context.Context) error {
	missing := make([]string, 0)
	for _, ref := range c.refs {
		ok, err := c.client.secretExists(ctx, ref)
		if err != nil {
			return err
		}
		if !ok {
			missing = append(missing, ref)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing required kubernetes secrets in namespace %s: %s", c.client.namespace, strings.Join(missing, ", "))
	}

	return nil
}

func (c *kubernetesSecretClient) ReadSecret(ctx context.Context, name string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.secretURL(name), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query kubernetes secret %s: %w", name, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		payload, err := decodeSecretResponse(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("decode kubernetes secret %s: %w", name, err)
		}
		return payload, nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("kubernetes secret %s not found", name)
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return nil, fmt.Errorf("query kubernetes secret %s: unexpected status %d: %s", name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
}

func (c *kubernetesSecretClient) secretExists(ctx context.Context, name string) (bool, error) {
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

func (c *kubernetesSecretClient) secretURL(name string) string {
	escapedNamespace := url.PathEscape(c.namespace)
	escapedName := url.PathEscape(name)
	return fmt.Sprintf("%s/api/v1/namespaces/%s/secrets/%s", c.baseURL, escapedNamespace, escapedName)
}

func decodeSecretResponse(reader io.Reader) (map[string]string, error) {
	var payload struct {
		Data map[string]string `json:"data"`
	}
	if err := json.NewDecoder(reader).Decode(&payload); err != nil {
		return nil, err
	}

	decoded := make(map[string]string, len(payload.Data))
	for key, value := range payload.Data {
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, fmt.Errorf("decode key %q: %w", key, err)
		}
		decoded[key] = string(raw)
	}
	return decoded, nil
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
