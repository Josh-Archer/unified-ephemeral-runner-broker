// Package webhook delivers signed allocation lifecycle events to configured HTTP endpoints.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/runtime"
)

const (
	headerEvent     = "X-UECB-Event"
	headerDelivery  = "X-UECB-Delivery"
	headerSignature = "X-UECB-Signature"
	headerTimestamp = "X-UECB-Timestamp"
	userAgent       = "uecb-broker-webhooks/1.0"
	defaultSecretKey = "signing_secret"

	EventReady     = "ready"
	EventFailed    = "failed"
	EventExpired   = "expired"
	EventCompleted = "completed"
	EventCanceled  = "canceled"
)

// EventName returns the short lifecycle event name for a state, or empty when
// the state is not a webhook-eligible transition.
func EventName(state model.AllocationState) string {
	switch state {
	case model.StateReady:
		return EventReady
	case model.StateFailed:
		return EventFailed
	case model.StateExpired:
		return EventExpired
	case model.StateCompleted:
		return EventCompleted
	case model.StateCanceled:
		return EventCanceled
	default:
		return ""
	}
}

// Envelope is the JSON body POSTed to each endpoint.
type Envelope struct {
	ID          string                 `json:"id"`
	Event       string                 `json:"event"`
	OccurredAt  time.Time              `json:"occurred_at"`
	Allocation  model.AllocationStatus `json:"allocation"`
}

// HTTPClient is the subset of *http.Client used by the dispatcher.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Dispatcher fans lifecycle events out to configured endpoints with HMAC signing
// and exponential backoff retries. Deliveries are asynchronous and best-effort:
// webhook failures never block allocation state transitions.
type Dispatcher struct {
	cfg     model.WebhooksConfig
	secrets runtime.SecretReader
	client  HTTPClient
	now     func() time.Time
	// wg tracks in-flight deliveries so tests can Wait.
	wg sync.WaitGroup
	// sleep is replaced in tests to avoid real delays.
	sleep func(time.Duration)
}

// New builds a dispatcher. When cfg.Enabled is false or endpoints is empty,
// Notify is a no-op.
func New(cfg model.WebhooksConfig, secrets runtime.SecretReader, client HTTPClient) *Dispatcher {
	if client == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		client = &http.Client{Timeout: timeout}
	}
	if secrets == nil {
		secrets = noopSecrets{}
	}
	return &Dispatcher{
		cfg:     cfg,
		secrets: secrets,
		client:  client,
		now:     time.Now,
		sleep:   time.Sleep,
	}
}

type noopSecrets struct{}

func (noopSecrets) ReadSecret(context.Context, string) (map[string]string, error) {
	return nil, fmt.Errorf("secret reader is not configured")
}

// Wait blocks until all async deliveries started before the call complete.
func (d *Dispatcher) Wait() {
	if d == nil {
		return
	}
	d.wg.Wait()
}

// Notify queues a lifecycle delivery for the allocation's current state.
// Unknown or non-lifecycle states are ignored.
func (d *Dispatcher) Notify(status model.AllocationStatus) {
	if d == nil || !d.cfg.Enabled || len(d.cfg.Endpoints) == 0 {
		return
	}
	event := EventName(status.State)
	if event == "" {
		return
	}
	d.NotifyEvent(event, status)
}

// NotifyEvent queues a named lifecycle event.
func (d *Dispatcher) NotifyEvent(event string, status model.AllocationStatus) {
	if d == nil || !d.cfg.Enabled || len(d.cfg.Endpoints) == 0 {
		return
	}
	event = normalizeEvent(event)
	if event == "" {
		return
	}

	deliveryID := newDeliveryID()
	occurredAt := d.now().UTC()
	envelope := Envelope{
		ID:         deliveryID,
		Event:      "allocation." + event,
		OccurredAt: occurredAt,
		Allocation: status,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		log.Printf("webhook marshal failed allocation=%s event=%s: %v", status.ID, event, err)
		return
	}

	for i, endpoint := range d.cfg.Endpoints {
		if !endpointWantsEvent(endpoint, event) {
			continue
		}
		ep := endpoint
		idx := i
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.deliverWithRetry(idx, ep, event, deliveryID, body, occurredAt)
		}()
	}
}

func (d *Dispatcher) deliverWithRetry(index int, endpoint model.WebhookEndpointConfig, event, deliveryID string, body []byte, occurredAt time.Time) {
	attempts := d.cfg.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	backoff := d.cfg.InitialBackoff
	if backoff <= 0 {
		backoff = 500 * time.Millisecond
	}
	maxBackoff := d.cfg.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 10 * time.Second
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		lastErr = d.deliverOnce(endpoint, event, deliveryID, body, occurredAt)
		if lastErr == nil {
			return
		}
		if attempt == attempts {
			break
		}
		if !isRetryable(lastErr) {
			log.Printf("webhook delivery permanent failure endpoint=%d url=%s allocation event=%s: %v",
				index, endpoint.URL, event, lastErr)
			return
		}
		d.sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
	log.Printf("webhook delivery exhausted retries endpoint=%d url=%s event=%s attempts=%d: %v",
		index, endpoint.URL, event, attempts, lastErr)
}

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(permanentError); ok {
		return false
	}
	return true
}

func (d *Dispatcher) deliverOnce(endpoint model.WebhookEndpointConfig, event, deliveryID string, body []byte, occurredAt time.Time) error {
	secret, err := d.resolveSigningSecret(context.Background(), endpoint)
	if err != nil {
		return permanentError{err: err}
	}

	sig := signBody(secret, body)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, strings.TrimSpace(endpoint.URL), bytes.NewReader(body))
	if err != nil {
		return permanentError{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(headerEvent, "allocation."+event)
	req.Header.Set(headerDelivery, deliveryID)
	req.Header.Set(headerTimestamp, occurredAt.Format(time.RFC3339Nano))
	req.Header.Set(headerSignature, "sha256="+sig)

	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusRequestTimeout ||
		resp.StatusCode == http.StatusTooManyRequests ||
		resp.StatusCode >= 500 {
		return fmt.Errorf("webhook HTTP %d", resp.StatusCode)
	}
	return permanentError{err: fmt.Errorf("webhook HTTP %d", resp.StatusCode)}
}

func (d *Dispatcher) resolveSigningSecret(ctx context.Context, endpoint model.WebhookEndpointConfig) (string, error) {
	if secret := strings.TrimSpace(endpoint.SigningSecret); secret != "" {
		return secret, nil
	}
	ref := strings.TrimSpace(endpoint.SigningSecretRef)
	if ref == "" {
		return "", fmt.Errorf("endpoint has no signing secret")
	}
	data, err := d.secrets.ReadSecret(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("read signing secret %q: %w", ref, err)
	}
	key := strings.TrimSpace(endpoint.SigningSecretKey)
	if key == "" {
		key = defaultSecretKey
	}
	secret := strings.TrimSpace(data[key])
	if secret == "" {
		return "", fmt.Errorf("signing secret %q missing key %q", ref, key)
	}
	return secret, nil
}

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature checks an X-UECB-Signature header against the body and secret.
func VerifySignature(secret, header string, body []byte) bool {
	header = strings.TrimSpace(header)
	header = strings.TrimPrefix(header, "sha256=")
	expected, err := hex.DecodeString(header)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(expected, mac.Sum(nil))
}

func endpointWantsEvent(endpoint model.WebhookEndpointConfig, event string) bool {
	if len(endpoint.Events) == 0 {
		return true
	}
	for _, raw := range endpoint.Events {
		if normalizeEvent(raw) == event {
			return true
		}
	}
	return false
}

func normalizeEvent(event string) string {
	event = strings.ToLower(strings.TrimSpace(event))
	event = strings.TrimPrefix(event, "allocation.")
	switch event {
	case EventReady, EventFailed, EventExpired, EventCompleted, EventCanceled:
		return event
	case "cancelled":
		return EventCanceled
	default:
		return ""
	}
}

func newDeliveryID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("wh-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
