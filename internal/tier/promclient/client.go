package promclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

type Client struct {
	BaseURL     string
	BearerToken string
	Timeout     time.Duration
	HTTPDoer    Doer
}

func (c *Client) QueryInstant(ctx context.Context, query string) (float64, error) {
	if strings.TrimSpace(c.BaseURL) == "" {
		return 0, fmt.Errorf("base url is required")
	}

	reqCtx := ctx
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	endpoint, err := url.Parse(strings.TrimRight(c.BaseURL, "/") + "/api/v1/query")
	if err != nil {
		return 0, err
	}
	values := endpoint.Query()
	values.Set("query", query)
	endpoint.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return 0, err
	}
	if token := strings.TrimSpace(c.BearerToken); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.doer().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var payload response
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, err
	}
	if payload.Status != "success" {
		if payload.Error != "" {
			return 0, fmt.Errorf("prometheus query failed: %s (%s)", payload.Error, payload.ErrorType)
		}
		return 0, fmt.Errorf("prometheus query failed: %s", resp.Status)
	}

	return payload.value()
}

func (c *Client) doer() Doer {
	if c.HTTPDoer != nil {
		return c.HTTPDoer
	}
	return &http.Client{Timeout: c.Timeout}
}

type response struct {
	Status    string       `json:"status"`
	ErrorType string       `json:"errorType"`
	Error     string       `json:"error"`
	Data      responseData `json:"data"`
}

type responseData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

func (r response) value() (float64, error) {
	switch r.Data.ResultType {
	case "scalar":
		var result []any
		if err := json.Unmarshal(r.Data.Result, &result); err != nil {
			return 0, err
		}
		if len(result) != 2 {
			return 0, fmt.Errorf("unexpected scalar result shape")
		}
		value, ok := result[1].(string)
		if !ok {
			return 0, fmt.Errorf("unexpected scalar value type")
		}
		return strconv.ParseFloat(value, 64)
	case "vector":
		var result []struct {
			Value []json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(r.Data.Result, &result); err != nil {
			return 0, err
		}
		if len(result) != 1 || len(result[0].Value) != 2 {
			return 0, fmt.Errorf("expected a single-sample vector")
		}
		var value string
		if err := json.Unmarshal(result[0].Value[1], &value); err != nil {
			return 0, err
		}
		return strconv.ParseFloat(value, 64)
	default:
		return 0, fmt.Errorf("unsupported result type %q", r.Data.ResultType)
	}
}
