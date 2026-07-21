package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

const (
	TypeMemory   = "memory"
	TypeFile     = "file"
	TypePostgres = "postgres"

	LeaderLeaseName = "broker-background"
)

var (
	// ErrNoCapacity is returned when SaveIfCapacity would exceed max runners.
	ErrNoCapacity = errors.New("no capacity")
	// ErrConflict is returned when a compare-and-swap state transition loses.
	ErrConflict = errors.New("state conflict")
)

// Store persists allocation records. Implementations may be process-local
// (memory, file) or shared across replicas (postgres).
type Store interface {
	Save(model.AllocationStatus) error
	Delete(string) error
	Get(string) (model.AllocationStatus, bool)
	List() []model.AllocationStatus
	MarkState(string, model.AllocationState, time.Time, string) (model.AllocationStatus, bool)

	// CompareAndMarkState updates only when the current state equals expectedFrom.
	// Returns false when the allocation is missing or the state does not match.
	CompareAndMarkState(id string, expectedFrom model.AllocationState, to model.AllocationState, now time.Time, message string) (model.AllocationStatus, bool)

	// SaveIfCapacity saves status only when the number of scheduler-accounted
	// allocations for the same pool/backend (reserved|ready|warm), excluding this
	// allocation id, is strictly below maxRunners. When tenantQuota > 0 the same
	// rule is applied to the tenant's accounted allocations across all backends
	// in the pool. Returns ErrNoCapacity when either limit would be exceeded.
	SaveIfCapacity(status model.AllocationStatus, maxRunners int, tenantQuota int) error

	// CountActive returns the number of scheduler-accounted allocations for pool/backend.
	CountActive(pool model.PoolName, backend model.BackendName) int

	// CountTenantActive returns accounted allocations for a tenant in a pool (all backends).
	CountTenantActive(pool model.PoolName, tenant string) int

	// Ping reports store readiness. Memory/file always succeed.
	Ping(context.Context) error

	// Close releases resources. Safe to call multiple times.
	Close() error
}

// LeaderElector coordinates single-writer background work across replicas.
type LeaderElector interface {
	// TryAcquireLeadership attempts to hold the named lease until ttl elapses.
	// identity should be stable per process (pod name or hostname).
	TryAcquireLeadership(ctx context.Context, name, identity string, ttl time.Duration) (bool, error)
	ReleaseLeadership(ctx context.Context, name, identity string) error
}

// AdmissionStateStore persists circuit-breaker and rate-limit runtime state.
type AdmissionStateStore interface {
	LoadAdmissionState(ctx context.Context) (AdmissionStateDocument, error)
	SaveAdmissionState(ctx context.Context, doc AdmissionStateDocument) error
}

// AdmissionStateDocument is the JSON document shared across replicas.
type AdmissionStateDocument struct {
	Circuits map[string]AdmissionCircuitState `json:"circuits,omitempty"`
	Limits   map[string]AdmissionRateLimit    `json:"limits,omitempty"`
}

// AdmissionCircuitState is a serializable circuit snapshot.
type AdmissionCircuitState struct {
	State                string      `json:"state"`
	Failures             []time.Time `json:"failures,omitempty"`
	OpenedAt             time.Time   `json:"openedAt,omitempty"`
	NextProbeAt          time.Time   `json:"nextProbeAt,omitempty"`
	HalfOpenInFlight     int         `json:"halfOpenInFlight,omitempty"`
	RecoverySuccesses    int         `json:"recoverySuccesses,omitempty"`
	LastTransitionReason string      `json:"lastTransitionReason,omitempty"`
}

// AdmissionRateLimit is a serializable rate-limit window.
type AdmissionRateLimit struct {
	WindowStart time.Time `json:"windowStart"`
	Used        int       `json:"used"`
}

// AdmissionKey builds a stable map key for pool/backend admission state.
func AdmissionKey(pool model.PoolName, backend model.BackendName) string {
	return string(pool) + "/" + string(backend)
}

// IsShared reports whether the store type is multi-replica safe.
func IsShared(storeType string) bool {
	return normalizeType(storeType) == TypePostgres
}

// IsProcessLocal reports whether the store type is single-replica only.
func IsProcessLocal(storeType string) bool {
	switch normalizeType(storeType) {
	case TypeMemory, TypeFile, "":
		return true
	default:
		return false
	}
}

func normalizeType(storeType string) string {
	return strings.ToLower(strings.TrimSpace(storeType))
}

// NewFromConfig constructs a store from broker configuration.
func NewFromConfig(cfg model.StateStoreConfig) (Store, error) {
	switch normalizeType(cfg.Type) {
	case "", TypeMemory:
		return NewMemory(), nil
	case TypeFile:
		return NewFile(cfg.Path)
	case TypePostgres:
		return NewPostgres(cfg)
	default:
		return nil, fmt.Errorf("unsupported broker.stateStore.type %q", cfg.Type)
	}
}

// AsLeaderElector returns a LeaderElector when the store supports it.
func AsLeaderElector(s Store) LeaderElector {
	if elector, ok := s.(LeaderElector); ok {
		return elector
	}
	return alwaysLeader{}
}

// AsAdmissionStateStore returns shared admission persistence when available.
func AsAdmissionStateStore(s Store) AdmissionStateStore {
	if admission, ok := s.(AdmissionStateStore); ok {
		return admission
	}
	return nil
}

type alwaysLeader struct{}

func (alwaysLeader) TryAcquireLeadership(context.Context, string, string, time.Duration) (bool, error) {
	return true, nil
}

func (alwaysLeader) ReleaseLeadership(context.Context, string, string) error {
	return nil
}

func isSchedulerAccountedState(state model.AllocationState) bool {
	return state == model.StateReady || state == model.StateReserved || state == model.StateWarm
}

func normalizeTenant(tenant string) string {
	tenant = strings.TrimSpace(tenant)
	if tenant == "" {
		return "default"
	}
	return tenant
}

func countActiveLocked(allocations map[string]model.AllocationStatus, pool model.PoolName, backend model.BackendName, excludeID string) int {
	count := 0
	for id, status := range allocations {
		if excludeID != "" && id == excludeID {
			continue
		}
		if status.Pool != pool || status.SelectedBackend != backend {
			continue
		}
		if isSchedulerAccountedState(status.State) {
			count++
		}
	}
	return count
}

func countTenantActiveLocked(allocations map[string]model.AllocationStatus, pool model.PoolName, tenant string, excludeID string) int {
	tenant = normalizeTenant(tenant)
	count := 0
	for id, status := range allocations {
		if excludeID != "" && id == excludeID {
			continue
		}
		if status.Pool != pool {
			continue
		}
		if !isSchedulerAccountedState(status.State) {
			continue
		}
		if normalizeTenant(status.Tenant) == tenant {
			count++
		}
	}
	return count
}

func applyMarkState(status model.AllocationStatus, state model.AllocationState, now time.Time, message string) model.AllocationStatus {
	status.State = state
	if message != "" {
		status.Error = message
	}
	if state == model.StateExpired || state == model.StateQuarantined {
		status.ExpiresAt = now
	}
	return status
}

func capacityAllowed(allocations map[string]model.AllocationStatus, status model.AllocationStatus, maxRunners int, tenantQuota int) error {
	if maxRunners > 0 {
		active := countActiveLocked(allocations, status.Pool, status.SelectedBackend, status.ID)
		if active >= maxRunners {
			return ErrNoCapacity
		}
	}
	if tenantQuota > 0 {
		tenantActive := countTenantActiveLocked(allocations, status.Pool, status.Tenant, status.ID)
		if tenantActive >= tenantQuota {
			return ErrNoCapacity
		}
	}
	return nil
}
