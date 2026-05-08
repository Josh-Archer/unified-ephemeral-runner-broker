package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	NameAWS   = "aws"
	NameAzure = "azure"
	NameGCP   = "gcp"

	ModeFreeTier      = "free-tier"
	ModeCostUsage     = "cost-usage"
	ModeCredits       = "credits"
	ModeBudget        = "budget"
	ModeConsumption   = "consumption"
	ModeBalance       = "balance"
	ModeBillingExport = "billing-export"
)

type Request struct {
	Provider string
	Mode     string
	URL      string
	Token    string
	Source   string
}

type Client interface {
	Snapshot(context.Context, Request) (Snapshot, error)
}

type Snapshot struct {
	Source          string
	Limit           float64
	Used            float64
	BurnRate        float64
	RemainingCredit float64
	WindowEnd       time.Time
	UpdatedAt       time.Time
	Err             string
}

type HTTPClient struct {
	client *http.Client
}

func NewHTTPClient(timeout time.Duration) *HTTPClient {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HTTPClient{client: &http.Client{Timeout: timeout}}
}

func (c *HTTPClient) Snapshot(ctx context.Context, request Request) (Snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, request.URL, nil)
	if err != nil {
		return Snapshot{}, err
	}
	if token := strings.TrimSpace(request.Token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		return Snapshot{}, fmt.Errorf("provider query failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return DecodeSnapshot(resp.Body, request.Source)
}

func DecodeSnapshot(reader io.Reader, source string) (Snapshot, error) {
	var payload struct {
		Source          string    `json:"source"`
		Limit           float64   `json:"limit"`
		Used            float64   `json:"used"`
		BurnRate        float64   `json:"burn_rate"`
		RemainingCredit float64   `json:"remaining_credit"`
		WindowEnd       time.Time `json:"window_end"`
		UpdatedAt       time.Time `json:"updated_at"`
		Error           string    `json:"error"`
	}
	if err := json.NewDecoder(reader).Decode(&payload); err != nil {
		return Snapshot{}, err
	}
	if payload.Source == "" {
		payload.Source = source
	}
	return Snapshot{
		Source:          payload.Source,
		Limit:           payload.Limit,
		Used:            payload.Used,
		BurnRate:        payload.BurnRate,
		RemainingCredit: payload.RemainingCredit,
		WindowEnd:       payload.WindowEnd,
		UpdatedAt:       payload.UpdatedAt,
		Err:             payload.Error,
	}, nil
}

func EncodeSnapshot(snapshot Snapshot) io.Reader {
	payload := struct {
		Source          string    `json:"source"`
		Limit           float64   `json:"limit"`
		Used            float64   `json:"used"`
		BurnRate        float64   `json:"burn_rate"`
		RemainingCredit float64   `json:"remaining_credit"`
		WindowEnd       time.Time `json:"window_end"`
		UpdatedAt       time.Time `json:"updated_at"`
		Error           string    `json:"error,omitempty"`
	}{
		Source:          snapshot.Source,
		Limit:           snapshot.Limit,
		Used:            snapshot.Used,
		BurnRate:        snapshot.BurnRate,
		RemainingCredit: snapshot.RemainingCredit,
		WindowEnd:       snapshot.WindowEnd,
		UpdatedAt:       snapshot.UpdatedAt,
		Error:           snapshot.Err,
	}
	data, _ := json.Marshal(payload)
	return bytes.NewReader(data)
}
