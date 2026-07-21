package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Postgres is a multi-replica shared state store backed by PostgreSQL.
type Postgres struct {
	db *sql.DB
}

// NewPostgres opens a PostgreSQL store and ensures schema exists.
func NewPostgres(cfg model.StateStoreConfig) (*Postgres, error) {
	dsn, err := resolvePostgresDSN(cfg)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres state store: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres state store: %w", err)
	}

	store := &Postgres{db: db}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func resolvePostgresDSN(cfg model.StateStoreConfig) (string, error) {
	if dsn := strings.TrimSpace(cfg.DSN); dsn != "" {
		return dsn, nil
	}
	envName := strings.TrimSpace(cfg.DSNEnv)
	if envName == "" {
		envName = "UECB_STATE_STORE_DSN"
	}
	if dsn := strings.TrimSpace(os.Getenv(envName)); dsn != "" {
		return dsn, nil
	}
	return "", fmt.Errorf("broker.stateStore.dsn or env %s is required when type is postgres", envName)
}

func (p *Postgres) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS uecb_allocations (
  id TEXT PRIMARY KEY,
  pool TEXT NOT NULL,
  backend TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL,
  tenant TEXT NOT NULL DEFAULT 'default',
  payload JSONB NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS uecb_allocations_active_idx
  ON uecb_allocations (pool, backend, state);
CREATE INDEX IF NOT EXISTS uecb_allocations_tenant_idx
  ON uecb_allocations (pool, tenant, state);

CREATE TABLE IF NOT EXISTS uecb_leader_leases (
  name TEXT PRIMARY KEY,
  holder TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS uecb_admission_state (
  id SMALLINT PRIMARY KEY CHECK (id = 1),
  payload JSONB NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`
	if _, err := p.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate postgres state store: %w", err)
	}
	return nil
}

func (p *Postgres) Save(status model.AllocationStatus) error {
	payload, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("encode allocation: %w", err)
	}
	tenant := normalizeTenant(status.Tenant)
	_, err = p.db.Exec(`
INSERT INTO uecb_allocations (id, pool, backend, state, tenant, payload, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (id) DO UPDATE SET
  pool = EXCLUDED.pool,
  backend = EXCLUDED.backend,
  state = EXCLUDED.state,
  tenant = EXCLUDED.tenant,
  payload = EXCLUDED.payload,
  updated_at = NOW()
`, status.ID, string(status.Pool), string(status.SelectedBackend), string(status.State), tenant, payload)
	if err != nil {
		return fmt.Errorf("save allocation: %w", err)
	}
	return nil
}

func (p *Postgres) Delete(id string) error {
	_, err := p.db.Exec(`DELETE FROM uecb_allocations WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete allocation: %w", err)
	}
	return nil
}

func (p *Postgres) Get(id string) (model.AllocationStatus, bool) {
	var payload []byte
	err := p.db.QueryRow(`SELECT payload FROM uecb_allocations WHERE id = $1`, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return model.AllocationStatus{}, false
	}
	if err != nil {
		return model.AllocationStatus{}, false
	}
	var status model.AllocationStatus
	if err := json.Unmarshal(payload, &status); err != nil {
		return model.AllocationStatus{}, false
	}
	return status, true
}

func (p *Postgres) List() []model.AllocationStatus {
	rows, err := p.db.Query(`SELECT payload FROM uecb_allocations`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	result := make([]model.AllocationStatus, 0)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			continue
		}
		var status model.AllocationStatus
		if err := json.Unmarshal(payload, &status); err != nil {
			continue
		}
		result = append(result, status)
	}
	return result
}

func (p *Postgres) MarkState(id string, state model.AllocationState, now time.Time, message string) (model.AllocationStatus, bool) {
	tx, err := p.db.Begin()
	if err != nil {
		return model.AllocationStatus{}, false
	}
	defer func() { _ = tx.Rollback() }()

	status, ok, err := p.getForUpdate(tx, id)
	if err != nil || !ok {
		return model.AllocationStatus{}, false
	}
	status = applyMarkState(status, state, now, message)
	if err := p.saveTx(tx, status); err != nil {
		return model.AllocationStatus{}, false
	}
	if err := tx.Commit(); err != nil {
		return model.AllocationStatus{}, false
	}
	return status, true
}

func (p *Postgres) CompareAndMarkState(id string, expectedFrom model.AllocationState, to model.AllocationState, now time.Time, message string) (model.AllocationStatus, bool) {
	tx, err := p.db.Begin()
	if err != nil {
		return model.AllocationStatus{}, false
	}
	defer func() { _ = tx.Rollback() }()

	status, ok, err := p.getForUpdate(tx, id)
	if err != nil || !ok || status.State != expectedFrom {
		return model.AllocationStatus{}, false
	}
	status = applyMarkState(status, to, now, message)
	if err := p.saveTx(tx, status); err != nil {
		return model.AllocationStatus{}, false
	}
	if err := tx.Commit(); err != nil {
		return model.AllocationStatus{}, false
	}
	return status, true
}

func (p *Postgres) SaveIfCapacity(status model.AllocationStatus, maxRunners int, tenantQuota int) error {
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if maxRunners > 0 {
		var active int
		err = tx.QueryRow(`
SELECT COUNT(*) FROM uecb_allocations
WHERE pool = $1 AND backend = $2 AND state IN ('reserved', 'ready', 'warm') AND id <> $3
`, string(status.Pool), string(status.SelectedBackend), status.ID).Scan(&active)
		if err != nil {
			return fmt.Errorf("count active allocations: %w", err)
		}
		if active >= maxRunners {
			return ErrNoCapacity
		}
	}
	if tenantQuota > 0 {
		var tenantActive int
		err = tx.QueryRow(`
SELECT COUNT(*) FROM uecb_allocations
WHERE pool = $1 AND tenant = $2 AND state IN ('reserved', 'ready', 'warm') AND id <> $3
`, string(status.Pool), normalizeTenant(status.Tenant), status.ID).Scan(&tenantActive)
		if err != nil {
			return fmt.Errorf("count tenant allocations: %w", err)
		}
		if tenantActive >= tenantQuota {
			return ErrNoCapacity
		}
	}
	if err := p.saveTx(tx, status); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *Postgres) CountActive(pool model.PoolName, backend model.BackendName) int {
	var active int
	err := p.db.QueryRow(`
SELECT COUNT(*) FROM uecb_allocations
WHERE pool = $1 AND backend = $2 AND state IN ('reserved', 'ready', 'warm')
`, string(pool), string(backend)).Scan(&active)
	if err != nil {
		return 0
	}
	return active
}

func (p *Postgres) CountTenantActive(pool model.PoolName, tenant string) int {
	var active int
	err := p.db.QueryRow(`
SELECT COUNT(*) FROM uecb_allocations
WHERE pool = $1 AND tenant = $2 AND state IN ('reserved', 'ready', 'warm')
`, string(pool), normalizeTenant(tenant)).Scan(&active)
	if err != nil {
		return 0
	}
	return active
}

func (p *Postgres) Ping(ctx context.Context) error {
	return p.db.PingContext(ctx)
}

func (p *Postgres) Close() error {
	if p.db == nil {
		return nil
	}
	return p.db.Close()
}

func (p *Postgres) TryAcquireLeadership(ctx context.Context, name, identity string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = 15 * time.Second
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var holder string
	var expiresAt time.Time
	err = tx.QueryRowContext(ctx, `
SELECT holder, expires_at FROM uecb_leader_leases WHERE name = $1 FOR UPDATE
`, name).Scan(&holder, &expiresAt)
	now := time.Now()
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `
INSERT INTO uecb_leader_leases (name, holder, expires_at) VALUES ($1, $2, $3)
`, name, identity, now.Add(ttl))
		if err != nil {
			return false, err
		}
		return true, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	if expiresAt.After(now) && holder != identity {
		return false, nil
	}
	_, err = tx.ExecContext(ctx, `
UPDATE uecb_leader_leases SET holder = $2, expires_at = $3 WHERE name = $1
`, name, identity, now.Add(ttl))
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (p *Postgres) ReleaseLeadership(ctx context.Context, name, identity string) error {
	_, err := p.db.ExecContext(ctx, `
DELETE FROM uecb_leader_leases WHERE name = $1 AND holder = $2
`, name, identity)
	return err
}

func (p *Postgres) LoadAdmissionState(ctx context.Context) (AdmissionStateDocument, error) {
	var payload []byte
	err := p.db.QueryRowContext(ctx, `SELECT payload FROM uecb_admission_state WHERE id = 1`).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return AdmissionStateDocument{
			Circuits: map[string]AdmissionCircuitState{},
			Limits:   map[string]AdmissionRateLimit{},
		}, nil
	}
	if err != nil {
		return AdmissionStateDocument{}, err
	}
	var doc AdmissionStateDocument
	if err := json.Unmarshal(payload, &doc); err != nil {
		return AdmissionStateDocument{}, err
	}
	if doc.Circuits == nil {
		doc.Circuits = map[string]AdmissionCircuitState{}
	}
	if doc.Limits == nil {
		doc.Limits = map[string]AdmissionRateLimit{}
	}
	return doc, nil
}

func (p *Postgres) SaveAdmissionState(ctx context.Context, doc AdmissionStateDocument) error {
	if doc.Circuits == nil {
		doc.Circuits = map[string]AdmissionCircuitState{}
	}
	if doc.Limits == nil {
		doc.Limits = map[string]AdmissionRateLimit{}
	}
	payload, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, err = p.db.ExecContext(ctx, `
INSERT INTO uecb_admission_state (id, payload, updated_at) VALUES (1, $1, NOW())
ON CONFLICT (id) DO UPDATE SET payload = EXCLUDED.payload, updated_at = NOW()
`, payload)
	return err
}

func (p *Postgres) getForUpdate(tx *sql.Tx, id string) (model.AllocationStatus, bool, error) {
	var payload []byte
	err := tx.QueryRow(`SELECT payload FROM uecb_allocations WHERE id = $1 FOR UPDATE`, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return model.AllocationStatus{}, false, nil
	}
	if err != nil {
		return model.AllocationStatus{}, false, err
	}
	var status model.AllocationStatus
	if err := json.Unmarshal(payload, &status); err != nil {
		return model.AllocationStatus{}, false, err
	}
	return status, true, nil
}

func (p *Postgres) saveTx(tx *sql.Tx, status model.AllocationStatus) error {
	payload, err := json.Marshal(status)
	if err != nil {
		return err
	}
	tenant := normalizeTenant(status.Tenant)
	_, err = tx.Exec(`
INSERT INTO uecb_allocations (id, pool, backend, state, tenant, payload, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW())
ON CONFLICT (id) DO UPDATE SET
  pool = EXCLUDED.pool,
  backend = EXCLUDED.backend,
  state = EXCLUDED.state,
  tenant = EXCLUDED.tenant,
  payload = EXCLUDED.payload,
  updated_at = NOW()
`, status.ID, string(status.Pool), string(status.SelectedBackend), string(status.State), tenant, payload)
	return err
}
