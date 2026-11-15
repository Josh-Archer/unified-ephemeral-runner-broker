package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxBillingRunsPages = 3

// IsBillingExhaustedError reports whether err indicates GitHub Actions billing
// or included minutes are exhausted for the account.
func IsBillingExhaustedError(err error) bool {
	if err == nil {
		return false
	}
	var status *StatusError
	if errors.As(err, &status) {
		if status.Code == http.StatusPaymentRequired {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"spending limit",
		"payments have failed",
		"out of minutes",
		"billing limit",
		"account has been locked",
		"actions minutes",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// GetActionsBillingUsage queries GitHub's enhanced billing usage report for a
// personal user account using the provided PAT and returns (grossUSD, netUSD)
// of Actions compute minutes used by non-public repositories this month.
func (c *Client) GetActionsBillingUsage(ctx context.Context, user string, year, month int, token string) (float64, float64, error) {
	user = strings.TrimSpace(user)
	token = strings.TrimSpace(token)
	if user == "" {
		return 0, 0, errors.New("billing user is required")
	}
	if token == "" {
		return 0, 0, errors.New("billing token is required")
	}

	endpoint := fmt.Sprintf("%s/users/%s/settings/billing/usage?year=%d&month=%d",
		strings.TrimRight(c.baseURL, "/"), url.PathEscape(user), year, month)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "unified-ephemeral-runner-broker")

	httpClient := c.http
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	c.apiCalls.Add(1)
	c.recordRateLimit(resp.Header)

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, 0, &StatusError{Code: resp.StatusCode, Message: strings.TrimSpace(string(body))}
	}

	var report struct {
		UsageItems []struct {
			Product        string  `json:"product"`
			UnitType       string  `json:"unitType"`
			RepositoryName string  `json:"repositoryName"`
			GrossAmount    float64 `json:"grossAmount"`
			NetAmount      float64 `json:"netAmount"`
		} `json:"usageItems"`
	}
	if err := json.Unmarshal(body, &report); err != nil {
		return 0, 0, err
	}

	visibility := map[string]bool{}
	var gross, net float64
	for _, item := range report.UsageItems {
		if !strings.EqualFold(strings.TrimSpace(item.Product), "actions") {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(item.UnitType), "minutes") {
			continue
		}
		repo := strings.TrimSpace(item.RepositoryName)
		if repo != "" {
			if !strings.Contains(repo, "/") {
				repo = user + "/" + repo
			}
			private, seen := visibility[repo]
			if !seen {
				priv, err := c.RepositoryIsPrivate(ctx, repo)
				// On lookup error, assume private so we don't undercount.
				private = err != nil || priv
				visibility[repo] = private
			}
			if !private {
				continue
			}
		}
		gross += item.GrossAmount
		net += item.NetAmount
	}
	return gross, net, nil
}

// MonthlyHostedMinutes estimates GitHub-hosted Actions minutes consumed by
// repository since monthStart using the broker's existing GitHub App client.
func (c *Client) MonthlyHostedMinutes(ctx context.Context, repository string, monthStart, now time.Time) (float64, error) {
	base, err := repositoryPath(repository)
	if err != nil {
		return 0, err
	}
	since := monthStart.UTC().Format("2006-01-02")
	var totalMinutes float64

	for page := 1; page <= maxBillingRunsPages; page++ {
		var runsResp struct {
			WorkflowRuns []struct {
				ID         int64  `json:"id"`
				Conclusion string `json:"conclusion"`
			} `json:"workflow_runs"`
		}
		path := fmt.Sprintf("%s/actions/runs?created=>=%s&per_page=100&page=%d", base, url.QueryEscape(since), page)
		if err := c.do(ctx, http.MethodGet, path, nil, &runsResp, http.StatusOK); err != nil {
			return totalMinutes, err
		}
		for _, run := range runsResp.WorkflowRuns {
			if strings.EqualFold(run.Conclusion, "skipped") {
				continue
			}
			var jobsResp struct {
				Jobs []struct {
					RunnerName  string    `json:"runner_name"`
					StartedAt   time.Time `json:"started_at"`
					CompletedAt time.Time `json:"completed_at"`
				} `json:"jobs"`
			}
			jobsPath := fmt.Sprintf("%s/actions/runs/%d/jobs?per_page=100", base, run.ID)
			if err := c.do(ctx, http.MethodGet, jobsPath, nil, &jobsResp, http.StatusOK); err != nil {
				return totalMinutes, err
			}
			for _, job := range jobsResp.Jobs {
				if !strings.HasPrefix(job.RunnerName, "GitHub Actions") || job.StartedAt.IsZero() {
					continue
				}
				end := job.CompletedAt
				if end.IsZero() {
					end = now
				}
				if d := end.Sub(job.StartedAt); d > 0 {
					totalMinutes += d.Minutes()
				}
			}
		}
		if len(runsResp.WorkflowRuns) < 100 {
			break
		}
	}
	return totalMinutes, nil
}
