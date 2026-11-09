package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSetRepositoryVariableUpdatesThenCreates(t *testing.T) {
	var calls []string
	exists := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["name"] != "UECB_SHARED_RUNS_ON" || body["value"] != "ubuntu-latest" {
			t.Errorf("body = %v", body)
		}
		switch {
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/o/r/actions/variables/UECB_SHARED_RUNS_ON":
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/actions/variables":
			exists = true
			w.WriteHeader(http.StatusCreated)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer server.Close()
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)

	for i := 0; i < 2; i++ {
		if err := client.SetRepositoryVariable(context.Background(), "o/r", "UECB_SHARED_RUNS_ON", "ubuntu-latest"); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"PATCH /repos/o/r/actions/variables/UECB_SHARED_RUNS_ON",
		"POST /repos/o/r/actions/variables",
		"PATCH /repos/o/r/actions/variables/UECB_SHARED_RUNS_ON",
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v", calls)
		}
	}
}

func TestSetRepositoryVariableReportsPermissionErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
	}))
	defer server.Close()
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)
	if err := client.SetRepositoryVariable(context.Background(), "o/r", "V", "x"); err == nil {
		t.Fatal("want error")
	}
}

func TestRepositoryIsPrivate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/public":
			_, _ = w.Write([]byte(`{"private":false,"visibility":"public"}`))
		case "/repos/o/private":
			_, _ = w.Write([]byte(`{"private":true,"visibility":"private"}`))
		case "/repos/o/internal":
			_, _ = w.Write([]byte(`{"private":false,"visibility":"internal"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClient(func(context.Context) (string, error) { return "tok", nil }, nil).WithBaseURL(server.URL)
	for repo, want := range map[string]bool{"o/public": false, "o/private": true, "o/internal": true} {
		got, err := client.RepositoryIsPrivate(context.Background(), repo)
		if err != nil || got != want {
			t.Fatalf("%s: private=%v err=%v, want %v", repo, got, err, want)
		}
	}
	if _, err := client.RepositoryIsPrivate(context.Background(), "o/missing"); err == nil {
		t.Fatal("want error for missing repository")
	}
}
