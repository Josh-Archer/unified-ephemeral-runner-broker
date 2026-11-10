package config

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// poolBackendRef extracts pools[<pool>] and, when present, .backends[<backend>]
// from the validation errors validateAll already produces.
var poolBackendRef = regexp.MustCompile(`pools\[([^\]]+)\](?:\.backends\[([^\]]+)\])?`)

// PoolBackendError is one pool that failed validation. Backend is set when
// the failure is inside a backend; the whole pool is still disabled.
type PoolBackendError struct {
	Pool    model.PoolName
	Backend model.BackendName
	Err     error
}

func (e PoolBackendError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.Backend != "" {
		return fmt.Sprintf("pools[%s].backends[%s] failed validation", e.Pool, e.Backend)
	}
	return fmt.Sprintf("pools[%s] failed validation", e.Pool)
}

func (e PoolBackendError) Unwrap() error { return e.Err }

// PoolBackendErrors is the strict-validation error for one or more pools
// that cannot be served. A single failure keeps the original message so
// existing checks still see the pool and backend names.
type PoolBackendErrors []PoolBackendError

func (e PoolBackendErrors) Error() string {
	switch len(e) {
	case 0:
		return "invalid pool config"
	case 1:
		return e[0].Error()
	default:
		parts := make([]string, len(e))
		for i := range e {
			parts[i] = e[i].Error()
		}
		return strings.Join(parts, "; ")
	}
}

// Validate reports whether cfg can be served as a whole. Process-wide
// failures and any invalid pool are errors. Use Partition or LoadServing
// to keep the valid pools and disable the rest.
func Validate(cfg model.BrokerConfig) error {
	_, disabled, err := Partition(cfg)
	if err != nil {
		return err
	}
	if len(disabled) > 0 {
		return PoolBackendErrors(disabled)
	}
	return nil
}

// Partition separates process-wide config failures from pools that can be
// disabled. On success, the returned config contains only pools that passed
// validation, and disabled lists the pools that were removed.
//
// A non-nil error is fatal: the broker must not start. That includes
// unusable broker-wide settings, missing auth, and the case where every
// pool failed validation. When every pool is invalid there is nothing left
// to serve, so the process exits instead of looking healthy while it drops
// every job. The error names each pool and, when known, its backend.
func Partition(cfg model.BrokerConfig) (model.BrokerConfig, []PoolBackendError, error) {
	originalPools := len(cfg.Pools)
	cfg.Pools = append([]model.PoolConfig(nil), cfg.Pools...)

	maxSteps := originalPools + 1
	for _, class := range cfg.Broker.SizeClasses {
		maxSteps += len(class.Backends) + 1
	}
	if maxSteps < 1 {
		maxSteps = 1
	}

	disabled := make([]PoolBackendError, 0)
	dropped := map[model.PoolName]struct{}{}
	// Backends that disappeared because their pool was disabled. A size class
	// may still name them. A backend that was never configured is a real
	// config error and stays fatal.
	removedBackends := map[string]struct{}{}
	var last error
	for step := 0; step < maxSteps; step++ {
		err := validateAll(cfg)
		if err == nil {
			return cfg, disabled, nil
		}
		last = err
		msg := err.Error()
		if processFatalMessage(msg) {
			return model.BrokerConfig{}, nil, err
		}
		if backend, ok := sizeClassUnknownBackend(msg); ok {
			_, removed := removedBackends[backend]
			if !removed || backendConfigured(cfg, backend) {
				return model.BrokerConfig{}, nil, err
			}
			next, changed := dropSizeClassBackend(cfg, backend)
			if !changed {
				return model.BrokerConfig{}, nil, err
			}
			cfg = next
			continue
		}
		pool, backend, ok := isolatablePool(cfg, err)
		if !ok {
			return model.BrokerConfig{}, nil, err
		}
		if _, seen := dropped[pool]; seen {
			return model.BrokerConfig{}, nil, err
		}
		dropped[pool] = struct{}{}
		for _, existing := range cfg.Pools {
			if existing.Name != pool {
				continue
			}
			for name := range existing.Backends {
				removedBackends[string(name)] = struct{}{}
			}
			break
		}
		disabled = append(disabled, PoolBackendError{Pool: pool, Backend: backend, Err: err})
		cfg.Pools = withoutPool(cfg.Pools, pool)
		if len(cfg.Pools) == 0 && originalPools > 0 {
			return model.BrokerConfig{}, disabled, fmt.Errorf("all pools failed validation: %w", PoolBackendErrors(disabled))
		}
	}
	if last == nil {
		last = fmt.Errorf("config validation did not finish")
	}
	return model.BrokerConfig{}, nil, last
}

// processFatalMessage reports settings that must stop the process even when
// the text also names a pool. Missing GitHub auth is one credential for the
// whole broker, not a property of one pool.
func processFatalMessage(msg string) bool {
	switch {
	case strings.Contains(msg, "github.auth.secretRef"):
		return true
	case strings.Contains(msg, "secret or secretRef"):
		return true
	case strings.Contains(msg, "sessionSecretRef is required"):
		return true
	default:
		return false
	}
}

func isolatablePool(cfg model.BrokerConfig, err error) (model.PoolName, model.BackendName, bool) {
	if err == nil || processFatalMessage(err.Error()) {
		return "", "", false
	}
	pool, backend := parsePoolBackend(err.Error())
	if pool == "" || !hasPool(cfg.Pools, pool) {
		return "", "", false
	}
	return pool, backend, true
}

func parsePoolBackend(msg string) (model.PoolName, model.BackendName) {
	match := poolBackendRef.FindStringSubmatch(msg)
	if match == nil {
		return "", ""
	}
	return model.PoolName(match[1]), model.BackendName(match[2])
}

func backendConfigured(cfg model.BrokerConfig, backend string) bool {
	for _, pool := range cfg.Pools {
		if _, ok := pool.Backends[model.BackendName(backend)]; ok {
			return true
		}
	}
	return false
}

func hasPool(pools []model.PoolConfig, name model.PoolName) bool {
	for _, pool := range pools {
		if pool.Name == name {
			return true
		}
	}
	return false
}

func withoutPool(pools []model.PoolConfig, name model.PoolName) []model.PoolConfig {
	kept := make([]model.PoolConfig, 0, len(pools))
	for _, pool := range pools {
		if pool.Name != name {
			kept = append(kept, pool)
		}
	}
	return kept
}

// sizeClassUnknownBackend matches the error from validateSizeClasses when a
// class lists a backend that no longer exists because its pool was disabled.
func sizeClassUnknownBackend(msg string) (string, bool) {
	if !strings.Contains(msg, "broker.sizeClasses[") || !strings.Contains(msg, "unknown backend ") {
		return "", false
	}
	_, rest, ok := strings.Cut(msg, "unknown backend ")
	if !ok {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	if len(rest) < 2 || rest[0] != '"' {
		return "", false
	}
	rest = rest[1:]
	end := strings.IndexByte(rest, '"')
	if end <= 0 {
		return "", false
	}
	return rest[:end], true
}

// dropSizeClassBackend removes backend from every size class. A class whose
// only backend was removed is deleted so a size-* request fails closed
// instead of matching every remaining backend. The caller's map is not mutated.
func dropSizeClassBackend(cfg model.BrokerConfig, backend string) (model.BrokerConfig, bool) {
	if len(cfg.Broker.SizeClasses) == 0 {
		return cfg, false
	}
	name := model.BackendName(backend)
	next := make(map[string]model.SizeClassConfig, len(cfg.Broker.SizeClasses))
	changed := false
	for className, class := range cfg.Broker.SizeClasses {
		filtered := make([]model.BackendName, 0, len(class.Backends))
		removed := false
		for _, candidate := range class.Backends {
			if candidate == name {
				removed = true
				continue
			}
			filtered = append(filtered, candidate)
		}
		if !removed {
			next[className] = class
			continue
		}
		changed = true
		if len(filtered) == 0 {
			continue
		}
		class.Backends = filtered
		next[className] = class
	}
	if !changed {
		return cfg, false
	}
	cfg.Broker.SizeClasses = next
	return cfg, true
}
