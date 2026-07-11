package backend

import (
	"context"
	"errors"
	"fmt"
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
