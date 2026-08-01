package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

const MetadataCapabilitiesKey = "capabilities"
const MetadataLaunchModeKey = "launch_mode"

var ErrBackendCapacityExhausted = errors.New("backend capacity exhausted")

type AllocationError struct {
	Err               error
	Reason            error
	CapacityExhausted bool
}

func (e *AllocationError) Error() string {
	return e.Err.Error()
}

func (e *AllocationError) Unwrap() error {
	return e.Err
}

func NewAllocationError(err error, reason error, capacityExhausted bool) error {
	return &AllocationError{
		Err:               err,
		Reason:            reason,
		CapacityExhausted: capacityExhausted,
	}
}

// IsCapacityExhausted reports whether err indicates the provider rejected the
// allocation because it has no free runner slots.
func IsCapacityExhausted(err error) bool {
	if err == nil {
		return false
	}
	var allocErr *AllocationError
	if errors.As(err, &allocErr) && allocErr.CapacityExhausted {
		return true
	}
	return errors.Is(err, ErrBackendCapacityExhausted)
}

// CapacityStatus is the broker-side view of provider-reported capacity.
// It mirrors pkg/adapter.CapacityStatus so built-in backends and SDK adapters
// publish the same counters.
type CapacityStatus struct {
	MaxRunners     int
	ActiveRunners  int
	PendingRunners int
	WarmRunners    int
}

// FreeSlots returns non-negative free runner slots from a capacity snapshot.
func FreeSlots(status CapacityStatus) int {
	used := status.ActiveRunners + status.PendingRunners + status.WarmRunners
	free := status.MaxRunners - used
	if free < 0 {
		return 0
	}
	return free
}

// CapacityJSON is the HTTP capacity feed body used by capacity_url endpoints
// and by native Capacity() implementations that speak the same contract.
// Controllers may also expose free_slots; when set without max_runners the
// broker reconstructs a ceiling.
type CapacityJSON struct {
	MaxRunners     int `json:"max_runners"`
	ActiveRunners  int `json:"active_runners"`
	PendingRunners int `json:"pending_runners"`
	WarmRunners    int `json:"warm_runners"`
	FreeSlots      int `json:"free_slots"`
}

// CapacityStatusFromJSON maps a capacity feed payload into CapacityStatus,
// reconstructing MaxRunners from free_slots when needed.
func CapacityStatusFromJSON(payload CapacityJSON) CapacityStatus {
	status := CapacityStatus{
		MaxRunners:     payload.MaxRunners,
		ActiveRunners:  payload.ActiveRunners,
		PendingRunners: payload.PendingRunners,
		WarmRunners:    payload.WarmRunners,
	}
	// Prefer explicit free_slots when controllers publish it without a max.
	if payload.FreeSlots > 0 && status.MaxRunners <= 0 {
		status.MaxRunners = payload.FreeSlots + status.ActiveRunners + status.PendingRunners + status.WarmRunners
	}
	if status.MaxRunners <= 0 && payload.FreeSlots == 0 {
		// free_slots:0 with no max is a valid "full" signal when work is in flight.
		if payload.ActiveRunners > 0 || payload.PendingRunners > 0 || payload.WarmRunners > 0 {
			status.MaxRunners = payload.ActiveRunners + payload.PendingRunners + payload.WarmRunners
		}
	}
	return status
}

// DecodeCapacityJSON decodes a capacity_url response body into CapacityStatus.
func DecodeCapacityJSON(r io.Reader) (CapacityStatus, error) {
	var payload CapacityJSON
	if err := json.NewDecoder(r).Decode(&payload); err != nil && err != io.EOF {
		return CapacityStatus{}, err
	}
	return CapacityStatusFromJSON(payload), nil
}

type ProvisionedRunner struct {
	RunnerLabel string
	Metadata    map[string]string
}

type Backend interface {
	Name() model.BackendName
	Provision(ctx context.Context, request model.AllocationRequest, allocation model.AllocationStatus) (ProvisionedRunner, error)
}

type CleanupBackend interface {
	Cleanup(ctx context.Context, status model.AllocationStatus) error
}

// CapacityBackend is an optional interface backends implement to publish
// provider-reported live capacity for routing decisions.
type CapacityBackend interface {
	Capacity(ctx context.Context) (CapacityStatus, error)
}

type Registry struct {
	backends map[model.BackendName]Backend
}

func NewRegistry(entries ...Backend) *Registry {
	backends := make(map[model.BackendName]Backend, len(entries))
	for _, entry := range entries {
		backends[entry.Name()] = entry
	}
	return &Registry{backends: backends}
}

func (r *Registry) Get(name model.BackendName) (Backend, bool) {
	backend, ok := r.backends[name]
	return backend, ok
}

func DefaultRunnerLabel(name model.BackendName, allocationID string) string {
	sanitized := strings.ReplaceAll(string(name), "-", "")
	return fmt.Sprintf("uecb-%s-%s", sanitized, allocationID)
}

func NormalizeCapabilities(values []string) []string {
	if len(values) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		capability := strings.ToLower(strings.TrimSpace(value))
		if capability == "" {
			continue
		}
		if _, ok := seen[capability]; ok {
			continue
		}
		seen[capability] = struct{}{}
		normalized = append(normalized, capability)
	}

	if len(normalized) == 0 {
		return nil
	}

	sort.Strings(normalized)
	return normalized
}

func CapabilitySet(values []string) map[string]struct{} {
	normalized := NormalizeCapabilities(values)
	if len(normalized) == 0 {
		return nil
	}

	result := make(map[string]struct{}, len(normalized))
	for _, value := range normalized {
		result[value] = struct{}{}
	}
	return result
}

func WithCapabilitiesMetadata(cfg model.BackendConfig, metadata map[string]string) map[string]string {
	capabilities := NormalizeCapabilities(cfg.Capabilities)
	if metadata == nil && len(capabilities) == 0 {
		return nil
	}

	result := make(map[string]string, len(metadata)+1)
	for key, value := range metadata {
		result[key] = value
	}

	if len(capabilities) == 0 {
		delete(result, MetadataCapabilitiesKey)
		return result
	}

	result[MetadataCapabilitiesKey] = strings.Join(capabilities, ",")
	return result
}
