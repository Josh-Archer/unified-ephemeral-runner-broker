package api

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
	dto "github.com/prometheus/client_model/go"
)

// These tests run against the Postgres service in .github/workflows/ci.yml.
// They intentionally use the production Postgres inbox implementation.
func postgresWebhookHarness(t *testing.T) (*webhookHarness, *store.Postgres, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("UECB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("UECB_TEST_POSTGRES_DSN is not set")
	}
	pg, err := store.NewPostgres(model.StateStoreConfig{Type: "postgres", DSN: dsn})
	if err != nil {
		t.Fatalf("open postgres inbox: %v", err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open postgres test connection: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := newWebhookHarness(t, true)
	h.inbox = pg
	h.server.SetGitHubWebhook(GitHubWebhookOptions{
		Secret:   func(context.Context) (string, error) { return testWebhookSecret, nil },
		IsLeader: func(context.Context) (bool, error) { return true, nil },
		Inbox:    pg,
	})
	return h, pg, db
}

func postgresDeliveryID() string { return fmt.Sprintf("pg-inbox-%d", time.Now().UnixNano()) }

func TestPostgresWebhookInboxLeaseExpiryDuringAllocationIsIdempotent(t *testing.T) {
	h, pg, db := postgresWebhookHarness(t)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DELETE FROM uecb_allocations WHERE workflow_job_id = $1`, 81001); err != nil {
		t.Fatal(err)
	}
	// Use the shared Postgres allocation store from two service instances to
	// exercise the same cross-replica visibility as overlapping lease holders.
	h.service = NewServiceWithStore(h.service.cfg, h.service.registry, nil, pg)
	h.server.service = h.service
	secondService := NewServiceWithStore(h.service.cfg, h.service.registry, nil, pg)
	id := postgresDeliveryID()
	body := []byte(jobBody("queued", 81001, "", "self-hosted", "uecb-full"))
	if err := pg.EnqueueWebhook(ctx, id, body); err != nil {
		t.Fatal(err)
	}
	first, err := pg.ClaimWebhooks(ctx, 1, 30*time.Second)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim = %v, %v", first, err)
	}
	// Expire the lease while the first claimant is still processing, as can
	// happen when backend allocation exceeds the 30 second lease.
	if _, err := db.ExecContext(ctx, `UPDATE uecb_webhook_inbox SET locked_until = NOW() - INTERVAL '1 second' WHERE id = $1`, first[0].ID); err != nil {
		t.Fatal(err)
	}
	second, err := pg.ClaimWebhooks(ctx, 1, 30*time.Second)
	if err != nil || len(second) != 1 || second[0].DeliveryID != id || second[0].Attempts != 2 {
		t.Fatalf("reclaimed delivery = %v, %v", second, err)
	}

	// The overlapping delivery processors must converge on the same job
	// allocation even though Postgres has issued two leases.
	pool, ok := h.service.MatchWebhookPool([]string{"self-hosted", "uecb-full"})
	if !ok {
		t.Fatal("webhook pool did not match")
	}
	type allocationResult struct {
		service *Service
		status  model.AllocationStatus
		created bool
		err     error
	}
	results := make(chan allocationResult, 2)
	for _, service := range []*Service{h.service, secondService} {
		go func(service *Service) {
			status, created, err := service.AllocateForWorkflowJob(ctx, pool, 81001, "example-org/example-repo")
			results <- allocationResult{service: service, status: status, created: created, err: err}
		}(service)
	}
	firstResult, secondResult := <-results, <-results
	for _, result := range []*allocationResult{&firstResult, &secondResult} {
		if result.err == nil {
			continue
		}
		result.status, result.created, result.err = result.service.AllocateForWorkflowJob(ctx, pool, 81001, "example-org/example-repo")
	}
	if firstResult.err != nil || secondResult.err != nil || firstResult.created == secondResult.created || firstResult.status.ID == "" || firstResult.status.ID != secondResult.status.ID {
		t.Fatalf("overlapping allocations = %+v, %+v", firstResult, secondResult)
	}
	var jobAllocationCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM uecb_allocations WHERE workflow_job_id = $1`, 81001).Scan(&jobAllocationCount); err != nil {
		t.Fatal(err)
	}
	if jobAllocationCount != 1 {
		t.Fatalf("allocations for workflow job = %d, want 1", jobAllocationCount)
	}

	var lockedUntil, dbNow time.Time
	if err := db.QueryRowContext(ctx, `SELECT locked_until, NOW() FROM uecb_webhook_inbox WHERE id = $1`, first[0].ID).Scan(&lockedUntil, &dbNow); err != nil {
		t.Fatal(err)
	}
	if lockedUntil.Before(dbNow.Add(29*time.Second)) || lockedUntil.After(dbNow.Add(31*time.Second)) {
		t.Fatalf("lease deadline %s is not 30s from database time %s", lockedUntil, dbNow)
	}
	if err := pg.AckWebhook(ctx, second[0].ID); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresWebhookInboxCompletedBeforeQueuedIsRetried(t *testing.T) {
	h, pg, db := postgresWebhookHarness(t)
	ctx := context.Background()
	completedID, queuedID := postgresDeliveryID(), postgresDeliveryID()
	if err := pg.EnqueueWebhook(ctx, completedID, []byte(jobBody("completed", 81002, "success", "self-hosted", "uecb-full"))); err != nil {
		t.Fatal(err)
	}
	if err := pg.EnqueueWebhook(ctx, queuedID, []byte(jobBody("queued", 81002, "", "self-hosted", "uecb-full"))); err != nil {
		t.Fatal(err)
	}
	if n, err := h.server.DrainGitHubWebhookInbox(ctx); err != nil || n != 1 {
		t.Fatalf("first drain = %d, %v; want queued delivery only", n, err)
	}
	if allocation, ok := h.service.findByWorkflowJobID(81002); !ok || allocation.State == model.StateCompleted {
		t.Fatalf("allocation after first drain = %+v, found=%v", allocation, ok)
	}
	if _, err := db.ExecContext(ctx, `UPDATE uecb_webhook_inbox SET locked_until = NOW() - INTERVAL '1 second' WHERE delivery_id = $1`, completedID); err != nil {
		t.Fatal(err)
	}
	if n, err := h.server.DrainGitHubWebhookInbox(ctx); err != nil || n != 1 {
		t.Fatalf("completion retry = %d, %v", n, err)
	}
	if allocation, ok := h.service.findByWorkflowJobID(81002); !ok || allocation.State != model.StateCompleted {
		t.Fatalf("allocation after completion retry = %+v, found=%v", allocation, ok)
	}
}

func TestPostgresWebhookInboxExpiredDeliveryIsReported(t *testing.T) {
	h, pg, db := postgresWebhookHarness(t)
	ctx := context.Background()
	id := postgresDeliveryID()
	if err := pg.EnqueueWebhook(ctx, id, []byte(jobBody("queued", 81003, "", "self-hosted", "uecb-full"))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE uecb_webhook_inbox SET received_at = NOW() - INTERVAL '25 hours' WHERE delivery_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	var before dto.Metric
	if err := h.server.githubWebhook.metrics.expired.Write(&before); err != nil {
		t.Fatal(err)
	}
	if n, err := h.server.DrainGitHubWebhookInbox(ctx); err != nil || n != 1 {
		t.Fatalf("expired delivery drain = %d, %v", n, err)
	}
	var after dto.Metric
	if err := h.server.githubWebhook.metrics.expired.Write(&after); err != nil {
		t.Fatal(err)
	}
	if after.GetCounter().GetValue() != before.GetCounter().GetValue()+1 {
		t.Fatalf("expired metric = %v, want increment from %v", after.GetCounter().GetValue(), before.GetCounter().GetValue())
	}
}
