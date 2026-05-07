package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPClientNormalizesSnapshotResponse(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("missing bearer token: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"limit":100,"used":80,"remaining_credit":7,"updated_at":"` + now.Format(time.RFC3339) + `"}`))
	}))
	defer server.Close()

	snapshot, err := NewHTTPClient(0).Snapshot(context.Background(), Request{
		Provider: NameAWS,
		Mode:     ModeFreeTier,
		URL:      server.URL,
		Token:    "token",
		Source:   "aws-main",
	})
	if err != nil {
		t.Fatalf("snapshot failed: %v", err)
	}
	if snapshot.Source != "aws-main" || snapshot.Limit != 100 || snapshot.Used != 80 || snapshot.RemainingCredit != 7 {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestEncodeDecodeSnapshotRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	want := Snapshot{
		Source:          "gcp-main",
		Limit:           10,
		Used:            2,
		RemainingCredit: 4,
		UpdatedAt:       now,
	}
	got, err := DecodeSnapshot(EncodeSnapshot(want), "")
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if got.Source != want.Source || got.Limit != want.Limit || got.Used != want.Used || got.RemainingCredit != want.RemainingCredit {
		t.Fatalf("unexpected round trip: %+v", got)
	}
}
