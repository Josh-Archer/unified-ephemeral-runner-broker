package promclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQueryInstantParsesScalar(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("unexpected auth header: %q", got)
		}
		if got := r.URL.Query().Get("query"); got != "up" {
			t.Fatalf("unexpected query: %q", got)
		}
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"scalar","result":[1712345678.123,"12.5"]}}`)
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL, BearerToken: "token"}
	value, err := client.QueryInstant(context.Background(), "up")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if value != 12.5 {
		t.Fatalf("unexpected value: %v", value)
	}
}

func TestQueryInstantParsesVector(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"job":"runner"},"value":[1712345678.123,"7"]}]}}`)
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL}
	value, err := client.QueryInstant(context.Background(), "up")
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if value != 7 {
		t.Fatalf("unexpected value: %v", value)
	}
}

func TestQueryInstantReturnsErrorPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"error","errorType":"bad_data","error":"boom"}`)
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL}
	_, err := client.QueryInstant(context.Background(), "up")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error payload, got %v", err)
	}
}

func TestQueryInstantAppliesTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"scalar","result":[1712345678.123,"1"]}}`)
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL, Timeout: 5 * time.Millisecond}
	_, err := client.QueryInstant(context.Background(), "up")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}
