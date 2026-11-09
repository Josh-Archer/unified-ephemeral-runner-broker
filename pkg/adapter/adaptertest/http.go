package adaptertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/pkg/adapter/dispatch"
)

// HTTPOptions configures RunHTTPConformance.
// Zero path values use the dispatch package defaults.
type HTTPOptions struct {
	// Token is sent as Authorization: Bearer. When set, the kit also checks
	// that a missing or wrong token is rejected with 401.
	Token string
	// Backend is sent as X-UECB-Backend and in the dispatch JSON body.
	Backend string
	Pool    string
	// Warm sends launch_mode "warm".
	Warm                 bool
	DispatchPath         string
	CleanupPath          string
	HealthPath           string
	CapacityPath         string
	JobTimeout           time.Duration
	MaxProbeReservations int
	RequireExhaustion    bool
}

func (o HTTPOptions) normalized() HTTPOptions {
	if strings.TrimSpace(o.Backend) == "" {
		o.Backend = "conformance"
	}
	if strings.TrimSpace(o.Pool) == "" {
		o.Pool = "lite"
	}
	if strings.TrimSpace(o.DispatchPath) == "" {
		o.DispatchPath = dispatch.PathDispatch
	}
	if strings.TrimSpace(o.CleanupPath) == "" {
		o.CleanupPath = dispatch.PathCleanup
	}
	if strings.TrimSpace(o.HealthPath) == "" {
		o.HealthPath = dispatch.PathHealth
	}
	if strings.TrimSpace(o.CapacityPath) == "" {
		o.CapacityPath = dispatch.PathCapacity
	}
	if o.JobTimeout <= 0 {
		o.JobTimeout = 15 * time.Minute
	}
	if o.MaxProbeReservations <= 0 {
		o.MaxProbeReservations = DefaultMaxProbeReservations
	}
	return o
}

// RunHTTPConformance checks an external-dispatch HTTP controller against the
// v1 contract. baseURL is the controller origin. The controller must have
// free slots at the start, and each case releases the slots it takes.
// Cases run sequentially against the same server.
func RunHTTPConformance(t *testing.T, baseURL string, opts HTTPOptions) {
	t.Helper()
	opts = opts.normalized()
	client := &http.Client{Timeout: 15 * time.Second}

	t.Run("health", func(t *testing.T) {
		t.Helper()
		if err := checkHTTPHealth(client, baseURL, opts); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("capacity", func(t *testing.T) {
		t.Helper()
		if _, err := checkHTTPCapacity(client, baseURL, opts); err != nil {
			t.Fatal(err)
		}
	})
	if opts.Token != "" {
		t.Run("auth", func(t *testing.T) {
			t.Helper()
			if err := checkHTTPAuth(client, baseURL, opts); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("dispatch-cleanup", func(t *testing.T) {
		t.Helper()
		if err := checkHTTPDispatchCleanup(client, baseURL, opts); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("idempotent-dispatch", func(t *testing.T) {
		t.Helper()
		if err := checkHTTPIdempotentDispatch(client, baseURL, opts); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cleanup-unknown", func(t *testing.T) {
		t.Helper()
		if err := checkHTTPCleanupUnknown(client, baseURL, opts); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("capacity-accounting", func(t *testing.T) {
		t.Helper()
		if err := checkHTTPAccounting(client, baseURL, opts); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("capacity-exhausted", func(t *testing.T) {
		t.Helper()
		err := checkHTTPExhaustion(client, baseURL, opts)
		var skip skipSignal
		if errors.As(err, &skip) {
			t.Skip(skip.Error())
		}
		if err != nil {
			t.Fatal(err)
		}
	})
}

type httpCapacity struct {
	MaxRunners     int  `json:"max_runners"`
	ActiveRunners  int  `json:"active_runners"`
	PendingRunners int  `json:"pending_runners"`
	WarmRunners    int  `json:"warm_runners"`
	FreeSlots      *int `json:"free_slots"`
}

func (c httpCapacity) used() int {
	return c.ActiveRunners + c.PendingRunners + c.WarmRunners
}

func (c httpCapacity) validate() error {
	if c.ActiveRunners < 0 || c.PendingRunners < 0 || c.WarmRunners < 0 {
		return fmt.Errorf("capacity counters must not be negative: %+v", c)
	}
	if c.MaxRunners <= 0 && (c.FreeSlots == nil || *c.FreeSlots <= 0) {
		return fmt.Errorf("capacity must report max_runners > 0 or free_slots > 0: %+v", c)
	}
	return nil
}

func checkHTTPHealth(client *http.Client, baseURL string, opts HTTPOptions) error {
	resp, body, err := doJSON(client, http.MethodGet, endpoint(baseURL, opts.HealthPath), opts.Token, opts.Backend, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health status %d: %s", resp.StatusCode, trimBody(body))
	}
	return nil
}

func checkHTTPCapacity(client *http.Client, baseURL string, opts HTTPOptions) (httpCapacity, error) {
	resp, body, err := doJSON(client, http.MethodGet, endpoint(baseURL, opts.CapacityPath), opts.Token, opts.Backend, nil)
	if err != nil {
		return httpCapacity{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return httpCapacity{}, fmt.Errorf("capacity status %d: %s", resp.StatusCode, trimBody(body))
	}
	var payload httpCapacity
	if err := json.Unmarshal(body, &payload); err != nil {
		return httpCapacity{}, fmt.Errorf("decode capacity: %w", err)
	}
	if err := payload.validate(); err != nil {
		return httpCapacity{}, err
	}
	return payload, nil
}

func checkHTTPAuth(client *http.Client, baseURL string, opts HTTPOptions) error {
	resp, _, err := doJSON(client, http.MethodGet, endpoint(baseURL, opts.HealthPath), "", opts.Backend, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("missing token status = %d, want 401", resp.StatusCode)
	}
	resp, _, err = doJSON(client, http.MethodGet, endpoint(baseURL, opts.HealthPath), opts.Token+"-wrong", opts.Backend, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("wrong token status = %d, want 401", resp.StatusCode)
	}
	return nil
}

func checkHTTPDispatchCleanup(client *http.Client, baseURL string, opts HTTPOptions) error {
	const id = "http-life-1"
	label, err := postDispatch(client, baseURL, opts, id)
	if err != nil {
		return err
	}
	if err := postCleanup(client, baseURL, opts, id, label, "completed"); err != nil {
		return err
	}
	return postCleanup(client, baseURL, opts, id, label, "completed")
}

func checkHTTPIdempotentDispatch(client *http.Client, baseURL string, opts HTTPOptions) error {
	const id = "http-idem-1"
	before, err := checkHTTPCapacity(client, baseURL, opts)
	if err != nil {
		return err
	}
	first, err := postDispatch(client, baseURL, opts, id)
	if err != nil {
		return err
	}
	second, err := postDispatch(client, baseURL, opts, id)
	if err != nil {
		return fmt.Errorf("repeat dispatch: %w", err)
	}
	if second != first {
		return fmt.Errorf("repeat dispatch label = %q, want %q", second, first)
	}
	mid, err := checkHTTPCapacity(client, baseURL, opts)
	if err != nil {
		return err
	}
	if got, want := mid.used(), before.used()+1; got != want {
		return fmt.Errorf("repeat dispatch used slots = %d, want %d", got, want)
	}
	return postCleanup(client, baseURL, opts, id, first, "canceled")
}

func checkHTTPCleanupUnknown(client *http.Client, baseURL string, opts HTTPOptions) error {
	return postCleanup(client, baseURL, opts, "http-does-not-exist", "missing", "canceled")
}

func checkHTTPAccounting(client *http.Client, baseURL string, opts HTTPOptions) error {
	const id = "http-acct-1"
	before, err := checkHTTPCapacity(client, baseURL, opts)
	if err != nil {
		return err
	}
	label, err := postDispatch(client, baseURL, opts, id)
	if err != nil {
		return err
	}
	mid, err := checkHTTPCapacity(client, baseURL, opts)
	if err != nil {
		return err
	}
	if got, want := mid.used(), before.used()+1; got != want {
		return fmt.Errorf("capacity used = %d after dispatch, want %d", got, want)
	}
	if err := postCleanup(client, baseURL, opts, id, label, "completed"); err != nil {
		return err
	}
	after, err := checkHTTPCapacity(client, baseURL, opts)
	if err != nil {
		return err
	}
	if got, want := after.used(), before.used(); got != want {
		return fmt.Errorf("capacity used = %d after cleanup, want %d", got, want)
	}
	return nil
}

func checkHTTPExhaustion(client *http.Client, baseURL string, opts HTTPOptions) error {
	status, err := checkHTTPCapacity(client, baseURL, opts)
	if err != nil {
		return err
	}
	free := status.MaxRunners - status.used()
	if status.FreeSlots != nil && (status.MaxRunners <= 0 || *status.FreeSlots < free) {
		free = *status.FreeSlots
	}
	if free < 0 {
		free = 0
	}
	if free > opts.MaxProbeReservations {
		message := fmt.Sprintf("free slots %d exceed MaxProbeReservations %d; shrink the test controller or raise the cap", free, opts.MaxProbeReservations)
		if opts.RequireExhaustion {
			return errors.New(message)
		}
		return skipSignal{reason: message}
	}

	type held struct {
		id    string
		label string
	}
	taken := make([]held, 0, free)
	for i := 0; i < free; i++ {
		id := fmt.Sprintf("http-fill-%d", i)
		label, err := postDispatch(client, baseURL, opts, id)
		if err != nil {
			return fmt.Errorf("dispatch %d of %d free slots: %w", i+1, free, err)
		}
		taken = append(taken, held{id: id, label: label})
	}
	if err := postDispatchExpectCapacity(client, baseURL, opts, "http-overflow"); err != nil {
		return err
	}
	filled, err := checkHTTPCapacity(client, baseURL, opts)
	if err != nil {
		return err
	}
	if got, want := filled.used(), status.used()+free; got != want {
		return fmt.Errorf("used slots = %d after filling %d free slots, want %d", got, free, want)
	}
	for _, slot := range taken {
		if err := postCleanup(client, baseURL, opts, slot.id, slot.label, "canceled"); err != nil {
			return err
		}
	}
	return nil
}

func postDispatch(client *http.Client, baseURL string, opts HTTPOptions, id string) (string, error) {
	resp, body, err := doJSON(client, http.MethodPost, endpoint(baseURL, opts.DispatchPath), opts.Token, opts.Backend, dispatchBody(opts, id))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("dispatch %s status %d: %s", id, resp.StatusCode, trimBody(body))
	}
	var payload struct {
		RunnerLabel string `json:"runner_label"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode dispatch %s: %w", id, err)
	}
	if strings.TrimSpace(payload.RunnerLabel) == "" {
		return "", fmt.Errorf("dispatch %s returned an empty runner_label", id)
	}
	return payload.RunnerLabel, nil
}

func postDispatchExpectCapacity(client *http.Client, baseURL string, opts HTTPOptions, id string) error {
	resp, body, err := doJSON(client, http.MethodPost, endpoint(baseURL, opts.DispatchPath), opts.Token, opts.Backend, dispatchBody(opts, id))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("dispatch past capacity status = %d, want 409: %s", resp.StatusCode, trimBody(body))
	}
	return nil
}

func postCleanup(client *http.Client, baseURL string, opts HTTPOptions, id, label, state string) error {
	payload := dispatch.CleanupRequest{
		Action:       "cleanup",
		Backend:      opts.Backend,
		AllocationID: id,
		Pool:         opts.Pool,
		RunnerLabel:  label,
		State:        state,
		Error:        "conformance",
	}
	resp, body, err := doJSON(client, http.MethodPost, endpoint(baseURL, opts.CleanupPath), opts.Token, opts.Backend, payload)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	return fmt.Errorf("cleanup %s status %d: %s", id, resp.StatusCode, trimBody(body))
}

func dispatchBody(opts HTTPOptions, id string) dispatch.Request {
	mode := "cold"
	if opts.Warm {
		mode = "warm"
	}
	label := "uecb-conformance-" + id
	return dispatch.Request{
		Action:            "dispatch",
		Backend:           opts.Backend,
		AllocationID:      id,
		Pool:              opts.Pool,
		RunnerLabel:       label,
		RunnerName:        label,
		RunnerLabels:      []string{"self-hosted", "linux", "x64", label},
		RequestedLabels:   []string{"linux", "x64"},
		LaunchMode:        mode,
		JobTimeout:        opts.JobTimeout.String(),
		JobTimeoutSeconds: int64(opts.JobTimeout / time.Second),
		GitHub: dispatch.GitHub{
			ScopeType:  "repository",
			Owner:      "example",
			Repository: "example/repo",
			TargetURL:  "https://github.com/example/repo",
		},
	}
}

func doJSON(client *http.Client, method, url, token, backend string, payload any) (*http.Response, []byte, error) {
	var reader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, reader)
	if err != nil {
		return nil, nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(backend) != "" {
		req.Header.Set(dispatch.HeaderBackend, backend)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp, nil, err
	}
	return resp, body, nil
}

func endpoint(baseURL, path string) string {
	base := strings.TrimRight(baseURL, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func trimBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 512 {
		return text[:512]
	}
	return text
}
