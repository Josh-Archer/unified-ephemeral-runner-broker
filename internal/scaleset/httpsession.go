package scaleset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

const userAgent = "unified-ephemeral-runner-broker scaleset-listener"

// ErrTokenExpired is returned when the message-queue bearer token is rejected.
var ErrTokenExpired = errors.New("scaleset: message queue token expired")

// ErrAcquireNotConfigured is returned by HTTPSession.AcquireJobs. Acquiring
// jobs needs the actions service URL from the admin client, which is not
// wired until JIT runner registration lands.
var ErrAcquireNotConfigured = errors.New("scaleset: acquire jobs is not configured")

// HTTPSession long-polls one message queue URL. It does not call the GitHub
// queued-run or workflow-job list APIs.
type HTTPSession struct {
	http    *http.Client
	mu      sync.Mutex
	session Session
	// Refresh replaces the session after one 401. Nil means a 401 fails the poll.
	Refresh func(ctx context.Context, expired Session) (Session, error)
}

// NewHTTPSession returns a session client. The caller's http.Client is not modified.
func NewHTTPSession(session Session, httpClient *http.Client) *HTTPSession {
	return &HTTPSession{http: httpClient, session: session}
}

// Session returns a copy of the current session.
func (c *HTTPSession) Session() Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

func (c *HTTPSession) setSession(session Session) {
	c.mu.Lock()
	c.session = session
	c.mu.Unlock()
}

// GetMessage long-polls the message queue. HTTP 202 is (nil, nil).
// A 401 refreshes the session once when Refresh is set.
func (c *HTTPSession) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*Message, error) {
	msg, err := c.getMessage(ctx, c.Session(), lastMessageID, maxCapacity)
	if err == nil || !errors.Is(err, ErrTokenExpired) || c.Refresh == nil {
		return msg, err
	}
	refreshed, refreshErr := c.Refresh(ctx, c.Session())
	if refreshErr != nil {
		return nil, refreshErr
	}
	c.setSession(refreshed)
	return c.getMessage(ctx, c.Session(), lastMessageID, maxCapacity)
}

func (c *HTTPSession) getMessage(ctx context.Context, session Session, lastMessageID, maxCapacity int) (*Message, error) {
	u, err := url.Parse(session.MessageQueueURL)
	if err != nil {
		return nil, errors.New("message queue url is invalid")
	}
	if lastMessageID > 0 {
		q := u.Query()
		q.Set("lastMessageId", strconv.Itoa(lastMessageID))
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json; api-version=6.0-preview")
	req.Header.Set("Authorization", "Bearer "+session.MessageQueueAccessToken)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(HeaderScaleSetMaxCapacity, strconv.Itoa(maxCapacity))

	resp, err := c.pollClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusAccepted:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, nil
	case http.StatusOK:
		msg, err := ParseMessage(resp.Body)
		if err != nil {
			return nil, err
		}
		return msg, nil
	case http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, ErrTokenExpired
	default:
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("scale set message poll: unexpected status %d", resp.StatusCode)
	}
}

// DeleteMessage acknowledges a message. A 401 refreshes once when Refresh is set.
func (c *HTTPSession) DeleteMessage(ctx context.Context, messageID int) error {
	err := c.deleteMessage(ctx, c.Session(), messageID)
	if err == nil || !errors.Is(err, ErrTokenExpired) || c.Refresh == nil {
		return err
	}
	refreshed, refreshErr := c.Refresh(ctx, c.Session())
	if refreshErr != nil {
		return refreshErr
	}
	c.setSession(refreshed)
	return c.deleteMessage(ctx, c.Session(), messageID)
}

func (c *HTTPSession) deleteMessage(ctx context.Context, session Session, messageID int) error {
	u, err := url.Parse(session.MessageQueueURL)
	if err != nil {
		return errors.New("message queue url is invalid")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strconv.Itoa(messageID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+session.MessageQueueAccessToken)
	req.Header.Set("User-Agent", userAgent)
	client := c.http
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusUnauthorized:
		return ErrTokenExpired
	default:
		return fmt.Errorf("scale set message ack: unexpected status %d", resp.StatusCode)
	}
}

// AcquireJobs is not available on a message-queue session alone.
func (c *HTTPSession) AcquireJobs(context.Context, []int64) ([]int64, error) {
	return nil, ErrAcquireNotConfigured
}

func (c *HTTPSession) pollClient() *http.Client {
	base := c.http
	if base == nil {
		return &http.Client{Timeout: MessagePollTimeout}
	}
	if base.Timeout > 0 && base.Timeout < MessagePollTimeout {
		clone := *base
		clone.Timeout = MessagePollTimeout
		return &clone
	}
	return base
}

// ParseSession decodes one secret value. Errors name the missing field and
// do not include the secret value.
func ParseSession(raw string) (Session, error) {
	var session Session
	if err := json.Unmarshal([]byte(raw), &session); err != nil {
		return Session{}, errors.New("session is not valid JSON")
	}
	session.SessionID = strings.TrimSpace(session.SessionID)
	session.MessageQueueURL = strings.TrimSpace(session.MessageQueueURL)
	session.MessageQueueAccessToken = strings.TrimSpace(session.MessageQueueAccessToken)
	if session.SessionID == "" {
		return Session{}, errors.New("sessionId is required")
	}
	if session.MessageQueueURL == "" {
		return Session{}, errors.New("messageQueueUrl is required")
	}
	parsed, err := url.ParseRequestURI(session.MessageQueueURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return Session{}, errors.New("messageQueueUrl is invalid")
	}
	if session.MessageQueueAccessToken == "" {
		return Session{}, errors.New("messageQueueAccessToken is required")
	}
	if session.Statistics == nil {
		return Session{}, errors.New("statistics is required")
	}
	return session, nil
}

// HTTPClientsFromSecret builds one message-queue client per configured pool.
// Secret keys are scale set names. refresh, when set, is called after a 401
// and must return the same key shape. Extra secret keys are ignored.
func HTTPClientsFromSecret(pools []model.PoolConfig, secret map[string]string, httpClient *http.Client, refresh func(ctx context.Context) (map[string]string, error)) (map[string]Client, error) {
	out := map[string]Client{}
	for _, pool := range pools {
		name := strings.TrimSpace(pool.ScaleSet.Name)
		if name == "" || pool.ScaleSet.ID <= 0 {
			continue
		}
		raw, ok := secret[name]
		if !ok {
			return nil, fmt.Errorf("scale set %q: session secret has no matching key", name)
		}
		session, err := ParseSession(raw)
		if err != nil {
			return nil, fmt.Errorf("scale set %q: %w", name, err)
		}
		client := NewHTTPSession(session, httpClient)
		if refresh != nil {
			scaleName := name
			client.Refresh = func(ctx context.Context, _ Session) (Session, error) {
				data, err := refresh(ctx)
				if err != nil {
					return Session{}, err
				}
				next, ok := data[scaleName]
				if !ok {
					return Session{}, fmt.Errorf("scale set %q: session secret has no matching key", scaleName)
				}
				return ParseSession(next)
			}
		}
		out[name] = client
	}
	if len(out) == 0 {
		return nil, errors.New("scale set listener enabled but no pool has scaleSet.name and id")
	}
	return out, nil
}
