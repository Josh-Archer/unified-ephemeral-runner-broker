package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/vmlifecycle"
)

// VMReservationSchema is opt-in, NOT part of Postgres.migrate. No runtime
// caller provisions these tables. Deploying this schema requires separate review.
const VMReservationSchema = `
CREATE TABLE IF NOT EXISTS uecb_vm_guard (
 id SMALLINT PRIMARY KEY CHECK (id = 1), next_fence BIGINT NOT NULL CHECK (next_fence > 0)
);
INSERT INTO uecb_vm_guard(id,next_fence) VALUES(1,1) ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS uecb_vm_reservations (
 id TEXT PRIMARY KEY, payload JSONB NOT NULL
);
CREATE TABLE IF NOT EXISTS uecb_vm_orphans (
 allocation TEXT NOT NULL, fence NUMERIC(20,0) NOT NULL, resource TEXT NOT NULL,
 PRIMARY KEY(allocation,fence,resource)
);`

// VMReservations uses PostgreSQL row locking for one global VM slot. It is
// unconnected to allocation/provisioning. Existing Postgres instances can share
// their pool, but schema creation is deliberately not automatic.
type VMReservations struct{ db *sql.DB }

func NewVMReservations(db *sql.DB) (*VMReservations, error) {
	if db == nil {
		return nil, errors.New("VM reservations require a database")
	}
	return &VMReservations{db: db}, nil
}
func (p *Postgres) VMReservations() *VMReservations { return &VMReservations{db: p.db} }

// Reserve assigns a monotonic generation while holding the global guard. A
// repeated live reservation with identical parameters returns the original.
// Released IDs are permanent tombstones and cannot be reused. Expiry alone
// never releases capacity. Database errors never mean spare capacity.
func (s *VMReservations) Reserve(ctx context.Context, id string, expires, now time.Time) (vmlifecycle.Record, error) {
	expires = canonicalTime(expires)
	now = canonicalTime(now)
	candidate := vmlifecycle.Record{Key: vmlifecycle.Key{Allocation: id, Fence: 1}, Phase: vmlifecycle.Reserved, ExpiresAt: expires}
	if now.IsZero() || !expires.After(now) {
		return vmlifecycle.Record{}, vmlifecycle.ErrInvalid
	}
	if _, e := vmlifecycle.Plan(candidate, vmlifecycle.Observation{}, now, false); e != nil {
		return vmlifecycle.Record{}, e
	}
	tx, e := s.begin(ctx)
	if e != nil {
		return vmlifecycle.Record{}, e
	}
	defer tx.Rollback()
	old, found, e := readVM(ctx, tx, id)
	if e != nil {
		return vmlifecycle.Record{}, e
	}
	if found {
		if !old.HoldsCapacity() || !old.ExpiresAt.Equal(expires) {
			return vmlifecycle.Record{}, ErrConflict
		}
		if e = tx.Commit(); e != nil {
			return vmlifecycle.Record{}, e
		}
		return old, nil
	}
	var orphans int
	if e = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM uecb_vm_orphans`).Scan(&orphans); e != nil {
		return vmlifecycle.Record{}, e
	}
	if orphans != 0 {
		return vmlifecycle.Record{}, ErrNoCapacity
	}
	rows, e := tx.QueryContext(ctx, `SELECT id,payload FROM uecb_vm_reservations`)
	if e != nil {
		return vmlifecycle.Record{}, e
	}
	held := false
	for rows.Next() {
		var key string
		var payload []byte
		if e = rows.Scan(&key, &payload); e != nil {
			rows.Close()
			return vmlifecycle.Record{}, e
		}
		r, err := decodeVM(key, payload)
		if err != nil {
			rows.Close()
			return vmlifecycle.Record{}, err
		}
		held = held || r.HoldsCapacity()
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return vmlifecycle.Record{}, e
	}
	if held {
		return vmlifecycle.Record{}, ErrNoCapacity
	}
	var fence int64
	if e = tx.QueryRowContext(ctx, `SELECT next_fence FROM uecb_vm_guard WHERE id=1`).Scan(&fence); e != nil {
		return vmlifecycle.Record{}, e
	}
	if fence <= 0 || fence == math.MaxInt64 {
		return vmlifecycle.Record{}, errors.New("VM fencing generation exhausted")
	}
	candidate.Key.Fence = uint64(fence)
	if _, e = tx.ExecContext(ctx, `UPDATE uecb_vm_guard SET next_fence=next_fence+1 WHERE id=1`); e != nil {
		return vmlifecycle.Record{}, e
	}
	payload, e := json.Marshal(candidate)
	if e != nil {
		return vmlifecycle.Record{}, e
	}
	if _, e = tx.ExecContext(ctx, `INSERT INTO uecb_vm_reservations(id,payload) VALUES($1,$2)`, id, payload); e != nil {
		return vmlifecycle.Record{}, e
	}
	if e = tx.Commit(); e != nil {
		return vmlifecycle.Record{}, e
	}
	return candidate, nil
}

// Transition checks the complete durable record, plans and commits in one
// transaction. Only after a successful return may the caller execute Action.
// Commit errors return no action; recover by re-reading, never replay Launch.
// Observation authenticity and provider fencing remain the caller's obligation.
func (s *VMReservations) Transition(ctx context.Context, expected vmlifecycle.Record, o vmlifecycle.Observation, now time.Time, cancel bool) (vmlifecycle.Decision, error) {
	tx, e := s.begin(ctx)
	if e != nil {
		return vmlifecycle.Decision{}, e
	}
	defer tx.Rollback()
	r, found, e := readVM(ctx, tx, expected.Key.Allocation)
	if e != nil {
		return vmlifecycle.Decision{}, e
	}
	expected.ExpiresAt = canonicalTime(expected.ExpiresAt)
	if !found || r != expected {
		return vmlifecycle.Decision{}, ErrConflict
	}
	d, e := vmlifecycle.Plan(r, o, now, cancel)
	if e != nil {
		return vmlifecycle.Decision{}, e
	}
	if d.Action == vmlifecycle.Launch {
		var count int
		if e = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM uecb_vm_orphans`).Scan(&count); e != nil {
			return vmlifecycle.Decision{}, e
		}
		if count != 0 {
			return vmlifecycle.Decision{}, ErrNoCapacity
		}
	}
	payload, e := json.Marshal(d.Next)
	if e != nil {
		return vmlifecycle.Decision{}, e
	}
	if _, e = tx.ExecContext(ctx, `UPDATE uecb_vm_reservations SET payload=$2 WHERE id=$1`, r.Key.Allocation, payload); e != nil {
		return vmlifecycle.Decision{}, e
	}
	if e = tx.Commit(); e != nil {
		return vmlifecycle.Decision{}, e
	}
	return d, nil
}

// Quarantine durably records an owned or unknown orphan, blocking ALL new
// reservations. It never deletes a VM, rewrites an existing lease or releases
// capacity. Clearing quarantine is intentionally not implemented here.
// Quarantine blocks subsequent first-launch decisions, NOT an already committed
// Launch action held by a worker. Runtime revocation must coordinate with the
// provider before claiming outstanding actions cannot execute. A last-minute
// database read cannot close that race.
func (s *VMReservations) Quarantine(ctx context.Context, key vmlifecycle.Key, resource string) error {
	if key.Allocation == "" || key.Fence == 0 || resource == "" {
		return vmlifecycle.ErrInvalid
	}
	tx, e := s.begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `INSERT INTO uecb_vm_orphans(allocation,fence,resource) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, key.Allocation, fmt.Sprint(key.Fence), resource); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *VMReservations) Get(ctx context.Context, id string) (vmlifecycle.Record, bool, error) {
	return readVM(ctx, s.db, id)
}

// List returns the durable reconciliation inventory, including tombstones. Any
// malformed row or query failure aborts the entire read, never a partial list.
// It is a snapshot only, not admission or capacity reservation.
func (s *VMReservations) List(ctx context.Context) ([]vmlifecycle.Record, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT id,payload FROM uecb_vm_reservations ORDER BY id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []vmlifecycle.Record{}
	for rows.Next() {
		var id string
		var payload []byte
		if e = rows.Scan(&id, &payload); e != nil {
			return nil, e
		}
		r, err := decodeVM(id, payload)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	return result, nil
}

type vmQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readVM(ctx context.Context, q vmQuerier, id string) (vmlifecycle.Record, bool, error) {
	var payload []byte
	e := q.QueryRowContext(ctx, `SELECT payload FROM uecb_vm_reservations WHERE id=$1`, id).Scan(&payload)
	if errors.Is(e, sql.ErrNoRows) {
		return vmlifecycle.Record{}, false, nil
	}
	if e != nil {
		return vmlifecycle.Record{}, false, e
	}
	r, e := decodeVM(id, payload)
	return r, e == nil, e
}
func decodeVM(id string, payload []byte) (vmlifecycle.Record, error) {
	var r vmlifecycle.Record
	d := json.NewDecoder(bytes.NewReader(payload))
	d.DisallowUnknownFields()
	if e := d.Decode(&r); e != nil {
		return r, e
	}
	if r.Key.Allocation != id || r.Key.Fence == 0 || r.ExpiresAt.IsZero() {
		return r, vmlifecycle.ErrInvalid
	}
	switch r.Phase {
	case vmlifecycle.Reserved, vmlifecycle.LaunchPending, vmlifecycle.Running, vmlifecycle.CleanupPending, vmlifecycle.Released:
	default:
		return r, vmlifecycle.ErrInvalid
	}
	if (r.Phase == vmlifecycle.Reserved || r.Phase == vmlifecycle.Released) && r.ResourceID != "" {
		return r, vmlifecycle.ErrInvalid
	}
	if r.Phase == vmlifecycle.Running && r.ResourceID == "" {
		return r, vmlifecycle.ErrInvalid
	}
	r.ExpiresAt = canonicalTime(r.ExpiresAt)
	return r, nil
}
func canonicalTime(t time.Time) time.Time { return t.Round(0).UTC() }
func (s *VMReservations) begin(ctx context.Context) (*sql.Tx, error) {
	tx, e := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return nil, e
	}
	var fence int64
	if e = tx.QueryRowContext(ctx, `SELECT next_fence FROM uecb_vm_guard WHERE id=1 FOR UPDATE`).Scan(&fence); e != nil {
		tx.Rollback()
		return nil, e
	}
	return tx, nil
}
