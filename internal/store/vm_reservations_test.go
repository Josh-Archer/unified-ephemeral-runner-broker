package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/vmlifecycle"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Integration only: explicit test DSN, unique disposable schema, no production
// migration path. CI supplies the existing ephemeral postgres:16 service.
func vmDB(t *testing.T) (*VMReservations, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("UECB_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("UECB_TEST_POSTGRES_DSN not set")
	}
	cfg, e := pgx.ParseConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	admin := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { admin.Close() })
	schema := fmt.Sprintf("vm_test_%d", time.Now().UnixNano())
	if _, e = admin.Exec(`CREATE SCHEMA ` + schema); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	cfg.RuntimeParams["search_path"] = schema
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { db.Close() })
	if _, e = db.Exec(VMReservationSchema); e != nil {
		t.Fatal(e)
	}
	s, _ := NewVMReservations(db)
	return s, db
}
func mustVM(t *testing.T, s *VMReservations, id string) (vmlifecycle.Record, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	r, e := s.Reserve(context.Background(), id, now.Add(time.Hour), now)
	if e != nil {
		t.Fatal(e)
	}
	return r, now
}
func vmObs(r vmlifecycle.Record, p vmlifecycle.Presence, id string) vmlifecycle.Observation {
	return vmlifecycle.Observation{Key: r.Key, Presence: p, ResourceID: id}
}
func TestPostgresVMReservationsGlobalCapAndReplay(t *testing.T) {
	s, _ := vmDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	expiry := now.Add(time.Hour)
	var wg sync.WaitGroup
	results := make(chan error, 16)
	wins := make(chan vmlifecycle.Record, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, e := s.Reserve(ctx, fmt.Sprintf("a-%d", i), expiry, now)
			results <- e
			if e == nil {
				wins <- r
			}
		}(i)
	}
	wg.Wait()
	close(results)
	close(wins)
	count := 0
	for e := range results {
		if e == nil {
			count++
		} else if !errors.Is(e, ErrNoCapacity) {
			t.Fatal(e)
		}
	}
	if count != 1 {
		t.Fatal(count)
	}
	r := <-wins
	again, e := s.Reserve(ctx, r.Key.Allocation, expiry, now)
	if e != nil || again != r {
		t.Fatal(again, e)
	}
	if _, e = s.Reserve(ctx, r.Key.Allocation, expiry.Add(time.Second), now); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	// TTL is not a capacity release, even across a fresh store wrapper.
	fresh, _ := NewVMReservations(s.db)
	if _, e = fresh.Reserve(ctx, "after-expiry", expiry.Add(time.Hour), expiry); !errors.Is(e, ErrNoCapacity) {
		t.Fatal(e)
	}
	d, e := s.Transition(ctx, r, vmObs(r, vmlifecycle.FencedAbsent, ""), expiry, true)
	if e != nil || d.Next.HoldsCapacity() {
		t.Fatal(d, e)
	}
	if _, e = s.Reserve(ctx, r.Key.Allocation, expiry.Add(time.Hour), expiry); !errors.Is(e, ErrConflict) {
		t.Fatal("tombstone reused", e)
	}
	next, e := fresh.Reserve(ctx, "new-id", expiry.Add(time.Hour), expiry)
	if e != nil || next.Key.Fence <= r.Key.Fence {
		t.Fatal(next, e)
	}
}
func TestPostgresVMReservationsCASCrashAndCleanup(t *testing.T) {
	s, db := vmDB(t)
	ctx := context.Background()
	r, now := mustVM(t, s, "cas")
	second, _ := NewVMReservations(db)
	out := make(chan error, 2)
	actions := make(chan vmlifecycle.Action, 2)
	for _, client := range []*VMReservations{s, second} {
		go func(c *VMReservations) {
			d, e := c.Transition(ctx, r, vmlifecycle.Observation{}, now, false)
			out <- e
			if e == nil {
				actions <- d.Action
			}
		}(client)
	}
	wins := 0
	for i := 0; i < 2; i++ {
		e := <-out
		if e == nil {
			wins++
		} else if !errors.Is(e, ErrConflict) {
			t.Fatal(e)
		}
	}
	if wins != 1 || <-actions != vmlifecycle.Launch {
		t.Fatal(wins)
	}
	// Simulate crash/lost successful response: reopen/read rather than replaying
	// the stale Launch decision. No actual provider is invoked by these tests.
	inventory, err := second.List(ctx)
	if err != nil || len(inventory) != 1 || inventory[0].Phase != vmlifecycle.LaunchPending {
		t.Fatal(inventory, err)
	}
	pending, ok, e := second.Get(ctx, "cas")
	if e != nil || !ok || pending.Phase != vmlifecycle.LaunchPending {
		t.Fatal(pending, e)
	}
	d, e := second.Transition(ctx, pending, vmlifecycle.Observation{}, now, false)
	if e != nil || d.Action != vmlifecycle.Inspect {
		t.Fatal(d, e)
	}
	pending = d.Next
	d, e = s.Transition(ctx, pending, vmObs(pending, vmlifecycle.Absent, ""), now, false)
	if e != nil || d.Action != vmlifecycle.Revoke || !d.Next.HoldsCapacity() {
		t.Fatal(d, e)
	}
	cleanup := d.Next
	if _, e = s.Reserve(ctx, "blocked", now.Add(time.Hour), now); !errors.Is(e, ErrNoCapacity) {
		t.Fatal(e)
	}
	// Same-snapshot repeated cleanup action CAS is rejected, including no-op
	// state changes: the planner's durable revision changes each action.
	d, e = s.Transition(ctx, cleanup, vmObs(cleanup, vmlifecycle.Absent, ""), now, false)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = second.Transition(ctx, cleanup, vmObs(cleanup, vmlifecycle.Absent, ""), now, false); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if _, e = second.Transition(ctx, d.Next, vmObs(d.Next, vmlifecycle.FencedAbsent, ""), now, false); e != nil {
		t.Fatal(e)
	}
}
func TestPostgresVMReservationsQuarantineAndCorruption(t *testing.T) {
	s, db := vmDB(t)
	ctx := context.Background()
	r, now := mustVM(t, s, "owned")
	if e := s.Quarantine(ctx, vmlifecycle.Key{Allocation: "unknown", Fence: 42}, "orphan"); e != nil {
		t.Fatal(e)
	}
	if e := s.Quarantine(ctx, vmlifecycle.Key{Allocation: "unknown", Fence: 42}, "orphan"); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Transition(ctx, r, vmlifecycle.Observation{}, now, false); !errors.Is(e, ErrNoCapacity) {
		t.Fatal("quarantine allowed launch", e)
	}
	if _, e := s.Transition(ctx, r, vmObs(r, vmlifecycle.FencedAbsent, ""), now, true); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Reserve(ctx, "still-blocked", now.Add(time.Hour), now); !errors.Is(e, ErrNoCapacity) {
		t.Fatal(e)
	}
	var count int
	if e := db.QueryRow(`SELECT COUNT(*) FROM uecb_vm_orphans`).Scan(&count); e != nil || count != 1 {
		t.Fatal(count, e)
	}
	// A corrupt ledger is an error, never interpreted as a free slot.
	if _, e := db.Exec(`UPDATE uecb_vm_reservations SET payload='{}'`); e != nil {
		t.Fatal(e)
	}
	if _, e := s.List(ctx); e == nil {
		t.Fatal("partial inventory accepted")
	}
	if _, _, e := s.Get(ctx, "owned"); e == nil {
		t.Fatal("accepted corruption")
	}
}
func TestPostgresVMReservationsRollbackAndCommitFailure(t *testing.T) {
	s, db := vmDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_, e := db.Exec(`CREATE FUNCTION fail_vm_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END; $$;
 CREATE CONSTRAINT TRIGGER reject_vm_write AFTER INSERT OR UPDATE ON uecb_vm_reservations DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_vm_write();`)
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.Reserve(ctx, "commit-fail", now.Add(time.Hour), now)
	if e == nil || r != (vmlifecycle.Record{}) {
		t.Fatal(r, e)
	}
	var fence int64
	db.QueryRow(`SELECT next_fence FROM uecb_vm_guard WHERE id=1`).Scan(&fence)
	if fence != 1 {
		t.Fatal("fence not rolled back", fence)
	}
	db.Exec(`DROP TRIGGER reject_vm_write ON uecb_vm_reservations`)
	r, e = s.Reserve(ctx, "committed", now.Add(time.Hour), now)
	if e != nil {
		t.Fatal(e)
	}
	db.Exec(`CREATE CONSTRAINT TRIGGER reject_vm_write AFTER INSERT OR UPDATE ON uecb_vm_reservations DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_vm_write()`)
	d, e := s.Transition(ctx, r, vmlifecycle.Observation{}, now, false)
	if e == nil || d != (vmlifecycle.Decision{}) {
		t.Fatal("returned action before commit", d, e)
	}
	got, _, e := s.Get(ctx, r.Key.Allocation)
	if e != nil || got != r {
		t.Fatal(got, e)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, e = s.Transition(canceled, r, vmlifecycle.Observation{}, now, false); e == nil {
		t.Fatal("canceled transaction")
	}
}
func TestVMLedgerDecodeRejectsInvalidRecords(t *testing.T) {
	for _, payload := range []string{`{}`, `{"unexpected":1}`, `{"Key":{"Allocation":"a","Fence":1},"Phase":"bogus","ExpiresAt":"2026-01-01T00:00:00Z"}`} {
		if _, e := decodeVM("a", []byte(payload)); e == nil {
			t.Fatal(payload)
		}
	}
	now := time.Now()
	r := vmlifecycle.Record{Key: vmlifecycle.Key{Allocation: "a", Fence: 1}, Phase: vmlifecycle.Reserved, ExpiresAt: now}
	b, _ := json.Marshal(r)
	got, e := decodeVM("a", b)
	if e != nil || !got.ExpiresAt.Equal(now) {
		t.Fatal(got, e)
	}
	if _, e := NewVMReservations(nil); e == nil {
		t.Fatal("nil pool")
	}
}

// This interleaving documents the boundary, not a safety guarantee: committing
// quarantine cannot retract a Launch action that was already handed to a worker.
func TestPostgresVMReservationsQuarantineAfterCommittedLaunch(t *testing.T) {
	s, _ := vmDB(t)
	ctx := context.Background()
	r, now := mustVM(t, s, "outstanding")
	d, e := s.Transition(ctx, r, vmlifecycle.Observation{}, now, false)
	if e != nil || d.Action != vmlifecycle.Launch {
		t.Fatal(d, e)
	}
	if e = s.Quarantine(ctx, vmlifecycle.Key{Allocation: "orphan", Fence: 42}, "vm-other"); e != nil {
		t.Fatal(e)
	}
	// The local worker still holds the immutable committed action. A runtime
	// provider revocation protocol is required before claiming it cannot launch.
	if d.Action != vmlifecycle.Launch {
		t.Fatal("test no longer exercises outstanding action")
	}
	current, ok, e := s.Get(ctx, r.Key.Allocation)
	if e != nil || !ok || current != d.Next {
		t.Fatal(current, e)
	}
	next, e := s.Transition(ctx, current, vmlifecycle.Observation{}, now, false)
	if e != nil || next.Action != vmlifecycle.Inspect {
		t.Fatal(next, e)
	}
}
