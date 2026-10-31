package runtime

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxKubernetesResponse bounds how much of a Kubernetes API response is read.
const maxKubernetesResponse = 4 << 20

// KubernetesAPI is a minimal Kubernetes REST client for the broker's own
// namespace, authenticated with the pod's service account.
type KubernetesAPI struct {
	namespace string
	baseURL   string
	token     string
	client    *http.Client
}

// NewKubernetesAPI builds a client for baseURL (tests and out-of-cluster use).
func NewKubernetesAPI(baseURL, token, namespace string, client *http.Client) *KubernetesAPI {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &KubernetesAPI{
		namespace: namespace,
		baseURL:   strings.TrimRight(baseURL, "/"),
		token:     token,
		client:    client,
	}
}

// NewKubernetesAPIFromEnv returns an in-cluster client, or nil when
// UECB_POD_NAMESPACE is unset (the broker is not running in Kubernetes).
func NewKubernetesAPIFromEnv() (*KubernetesAPI, error) {
	namespace := strings.TrimSpace(os.Getenv("UECB_POD_NAMESPACE"))
	if namespace == "" {
		return nil, nil
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
	return NewKubernetesAPI(fmt.Sprintf("https://%s:%s", host, port), strings.TrimSpace(string(tokenBytes)), namespace, client), nil
}

// Namespace is the namespace the broker runs in.
func (k *KubernetesAPI) Namespace() string {
	return k.namespace
}

// Do sends body (JSON-encoded when non-nil) to path and returns the status code
// and response body. Non-2xx statuses are returned without an error so callers
// can treat 404 and 409 as expected outcomes.
func (k *KubernetesAPI) Do(ctx context.Context, method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encode kubernetes request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.baseURL+path, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if k.token != "" {
		req.Header.Set("Authorization", "Bearer "+k.token)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("kubernetes %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxKubernetesResponse))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read kubernetes %s %s: %w", method, path, err)
	}
	return resp.StatusCode, data, nil
}
