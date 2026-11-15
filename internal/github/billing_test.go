package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsBillingExhaustedError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("connection refused"), false},
		{&StatusError{Code: http.StatusNotFound, Message: "not found"}, false},
		{&StatusError{Code: http.StatusPaymentRequired, Message: "payment required"}, true},
		{&StatusError{Code: http.StatusForbidden, Message: "Your spending limit has been reached"}, true},
		{errors.New("The job was not started because recent account payments have failed"), true},
		{errors.New("out of minutes for this month"), true},
	}
	for _, tc := range cases {
		if got := IsBillingExhaustedError(tc.err); got != tc.want {
			t.Errorf("IsBillingExhaustedError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestGetActionsBillingUsageFiltersPublicAndNonMinutes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/Josh-Archer/settings/billing/usage":
			if got := r.Header.Get("Authorization"); got != "Bearer pat-123" {
				t.Fatalf("Authorization = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"usageItems": []map[string]any{
					{"product": "Actions", "unitType": "Minutes", "repositoryName": "DeskQuest", "grossAmount": 4.5, "netAmount": 0},
					{"product": "Actions", "unitType": "Minutes", "repositoryName": "public-repo", "grossAmount": 99.0, "netAmount": 0},
					{"product": "Actions", "unitType": "GigabyteHours", "repositoryName": "DeskQuest", "grossAmount": 2.0, "netAmount": 0},
					{"product": "Actions", "unitType": "Minutes", "repositoryName": "home", "grossAmount": 8.5, "netAmount": 1.0},
				},
			})
		case "/repos/Josh-Archer/DeskQuest", "/repos/Josh-Archer/home":
			_ = json.NewEncoder(w).Encode(map[string]any{"private": true, "visibility": "private"})
		case "/repos/Josh-Archer/public-repo":
			_ = json.NewEncoder(w).Encode(map[string]any{"private": false, "visibility": "public"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(func(context.Context) (string, error) { return "app-tok", nil }, srv.Client())
	c.baseURL = srv.URL

	gross, net, err := c.GetActionsBillingUsage(context.Background(), "Josh-Archer", 2026, 11, "pat-123")
	if err != nil {
		t.Fatal(err)
	}
	if gross != 13.0 || net != 1.0 {
		t.Fatalf("got (%.2f, %.2f), want (13.00, 1.00)", gross, net)
	}
}

func TestMonthlyHostedMinutesSumsGitHubHostedJobsOnly(t *testing.T) {
	t0 := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/Josh-Archer/DeskQuest/actions/runs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"workflow_runs": []map[string]any{
					{"id": 1, "conclusion": "success"},
					{"id": 2, "conclusion": "skipped"},
				},
			})
		case "/repos/Josh-Archer/DeskQuest/actions/runs/1/jobs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs": []map[string]any{
					{
						"runner_name":  "GitHub Actions 1000027117",
						"started_at":   t0.Add(-10 * time.Minute),
						"completed_at": t0.Add(-4 * time.Minute),
					},
					{
						"runner_name":  "uecb-kubernetesshareddesktop-123",
						"started_at":   t0.Add(-20 * time.Minute),
						"completed_at": t0.Add(-5 * time.Minute),
					},
					{
						"runner_name": "GitHub Actions 1000027118",
						"started_at":  t0.Add(-2 * time.Minute),
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient(func(context.Context) (string, error) { return "app-tok", nil }, srv.Client())
	c.baseURL = srv.URL

	mins, err := c.MonthlyHostedMinutes(context.Background(), "Josh-Archer/DeskQuest", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), t0)
	if err != nil {
		t.Fatal(err)
	}
	if mins != 8.0 {
		t.Fatalf("MonthlyHostedMinutes = %.2f, want 8.0", mins)
	}
}
