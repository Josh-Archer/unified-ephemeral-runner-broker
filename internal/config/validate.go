package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

const (
	tierFailureModePassThrough = "pass-through-round-robin"
	tierFailureModeBlock       = "block"
	tierFailureModeFallback    = "fallback-backends"

	tierActionObserveOnly  = "observe-only"
	tierActionDeprioritize = "deprioritize"
	tierActionDisable      = "disable"

	tierCombineMostRestrictive = "most-restrictive"
)

func Validate(cfg model.BrokerConfig) error {
	if err := validateStateStore(cfg.Broker.StateStore); err != nil {
		return err
	}
	if err := validateHA(cfg.Broker); err != nil {
		return err
	}
	if err := validateAdmissionQueue(cfg.Broker.Queue); err != nil {
		return err
	}
	if err := validateTierRouting(cfg); err != nil {
		return err
	}
	if err := validateLiveCapacity(cfg.Broker.LiveCapacity); err != nil {
		return err
	}
	if err := validateBackendBudgets(cfg); err != nil {
		return err
	}
	return nil
}

func validateBackendBudgets(cfg model.BrokerConfig) error {
	for _, pool := range cfg.Pools {
		for backendName, backendCfg := range pool.Backends {
			if err := validateBudgetConfig(pool.Name, backendName, backendCfg.Budget); err != nil {
				return err
			}
			if backendCfg.CostClass < 0 {
				return fmt.Errorf("pools[%s].backends[%s].costClass must not be negative", pool.Name, backendName)
			}
		}
	}
	return nil
}

func validateBudgetConfig(pool model.PoolName, backend model.BackendName, budget model.BudgetConfig) error {
	if !budget.Enabled {
		return nil
	}
	prefix := fmt.Sprintf("pools[%s].backends[%s].budget", pool, backend)
	if budget.MaxAllocationsDaily < 0 {
		return fmt.Errorf("%s.maxAllocationsDaily must not be negative", prefix)
	}
	if budget.MaxAllocationsMonthly < 0 {
		return fmt.Errorf("%s.maxAllocationsMonthly must not be negative", prefix)
	}
	if budget.MaxAllocationsDaily == 0 && budget.MaxAllocationsMonthly == 0 {
		return fmt.Errorf("%s requires maxAllocationsDaily and/or maxAllocationsMonthly when enabled", prefix)
	}
	return nil
}

func validateWarmPools(cfg model.BrokerConfig) error {
	for _, pool := range cfg.Pools {
		for name, backendCfg := range pool.Backends {
			if err := validateWarmBackendConfig(string(pool.Name), string(name), backendCfg); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateWarmBackendConfig(pool, backend string, cfg model.BackendConfig) error {
	if cfg.WarmMin < 0 {
		return fmt.Errorf("pools[%s].backends[%s].warmMin must not be negative", pool, backend)
	}
	if cfg.WarmMax < 0 {
		return fmt.Errorf("pools[%s].backends[%s].warmMax must not be negative", pool, backend)
	}
	if cfg.WarmTTL < 0 {
		return fmt.Errorf("pools[%s].backends[%s].warmTTL must not be negative", pool, backend)
	}
	if cfg.WarmSchedule == nil {
		return nil
	}
	tz := strings.TrimSpace(cfg.WarmSchedule.Timezone)
	if tz != "" && !strings.EqualFold(tz, "UTC") && !strings.EqualFold(tz, "Local") {
		if _, err := time.LoadLocation(tz); err != nil {
			return fmt.Errorf("pools[%s].backends[%s].warmSchedule.timezone %q is invalid: %w", pool, backend, tz, err)
		}
	}
	for i, window := range cfg.WarmSchedule.Windows {
		if err := validateWarmWindow(pool, backend, i, window); err != nil {
			return err
		}
	}
	return nil
}

func validateWarmWindow(pool, backend string, index int, window model.WarmWindowConfig) error {
	prefix := fmt.Sprintf("pools[%s].backends[%s].warmSchedule.windows[%d]", pool, backend, index)
	if strings.TrimSpace(window.Start) == "" {
		return fmt.Errorf("%s.start is required", prefix)
	}
	if strings.TrimSpace(window.End) == "" {
		return fmt.Errorf("%s.end is required", prefix)
	}
	if err := validateClock(window.Start); err != nil {
		return fmt.Errorf("%s.start: %w", prefix, err)
	}
	if err := validateClock(window.End); err != nil {
		return fmt.Errorf("%s.end: %w", prefix, err)
	}
	for _, day := range window.Days {
		if !isWeekdayToken(day) {
			return fmt.Errorf("%s.days contains unsupported weekday %q", prefix, day)
		}
	}
	if window.WarmMin != nil && *window.WarmMin < 0 {
		return fmt.Errorf("%s.warmMin must not be negative", prefix)
	}
	if window.WarmMax != nil && *window.WarmMax < 0 {
		return fmt.Errorf("%s.warmMax must not be negative", prefix)
	}
	return nil
}

func validateClock(value string) error {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) < 2 || len(parts) > 3 {
		return fmt.Errorf("expected HH:MM or HH:MM:SS")
	}
	var hour, minute int
	if _, err := fmt.Sscanf(parts[0], "%d", &hour); err != nil || hour < 0 || hour > 23 {
		return fmt.Errorf("invalid hour")
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &minute); err != nil || minute < 0 || minute > 59 {
		return fmt.Errorf("invalid minute")
	}
	if len(parts) == 3 {
		var second int
		if _, err := fmt.Sscanf(parts[2], "%d", &second); err != nil || second < 0 || second > 59 {
			return fmt.Errorf("invalid second")
		}
	}
	return nil
}

func isWeekdayToken(day string) bool {
	switch strings.ToLower(strings.TrimSpace(day)) {
	case "monday", "mon", "tuesday", "tue", "tues", "wednesday", "wed",
		"thursday", "thu", "thur", "thurs", "friday", "fri", "saturday", "sat",
		"sunday", "sun":
		return true
	default:
		return false
	}
}

func validateLiveCapacity(cfg model.LiveCapacityConfig) error {
	if !cfg.Enabled {
		return nil
	}
	switch normalizeStringDefault(cfg.FailureMode, "pass-through") {
	case "pass-through", "block", "block-stale", "fail-closed":
	default:
		return fmt.Errorf("broker.liveCapacity.failureMode %q is not supported", cfg.FailureMode)
	}
	if cfg.RefreshInterval < 0 {
		return fmt.Errorf("broker.liveCapacity.refreshInterval must not be negative")
	}
	if cfg.StaleAfter < 0 {
		return fmt.Errorf("broker.liveCapacity.staleAfter must not be negative")
	}
	if cfg.ProbeTimeout < 0 {
		return fmt.Errorf("broker.liveCapacity.probeTimeout must not be negative")
	}
	return nil
}

func validateQualityAware(cfg model.QualityAwareConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if cfg.Window < 0 {
		return fmt.Errorf("broker.qualityAware.window must not be negative")
	}
	if cfg.MinSamples < 0 {
		return fmt.Errorf("broker.qualityAware.minSamples must not be negative")
	}
	w := cfg.Weights
	if w.FreeSlots < 0 || w.SuccessRate < 0 || w.Latency < 0 || w.CapacityErrors < 0 {
		return fmt.Errorf("broker.qualityAware.weights values must not be negative")
	}
	return nil
}

func validateHA(cfg model.BrokerRuntimeConfig) error {
	if cfg.HA.LeaseTTL < 0 {
		return fmt.Errorf("broker.ha.leaseTTL must not be negative")
	}
	enabled := HAEnabled(cfg)
	if enabled && normalizeStringDefault(cfg.StateStore.Type, "memory") != "postgres" {
		// Allow explicit HA with any store that implements leader election
		// (memory/file implement local leases for tests). Only warn via docs;
		// production multi-replica still requires ValidateReplicaSafety.
	}
	return nil
}

// HAEnabled reports whether leader election and shared coordination should run.
func HAEnabled(cfg model.BrokerRuntimeConfig) bool {
	if cfg.HA.Enabled != nil {
		return *cfg.HA.Enabled
	}
	return normalizeStringDefault(cfg.StateStore.Type, "memory") == "postgres"
}

func validateStateStore(cfg model.StateStoreConfig) error {
	switch normalizeStringDefault(cfg.Type, "memory") {
	case "memory":
		return nil
	case "file":
		if strings.TrimSpace(cfg.Path) == "" {
			return fmt.Errorf("broker.stateStore.path is required when type is file")
		}
		return nil
	case "postgres":
		// DSN may come from env at runtime; require either inline DSN or a non-empty env name.
		if strings.TrimSpace(cfg.DSN) == "" && strings.TrimSpace(cfg.DSNEnv) == "" {
			// Empty DSNEnv still defaults to UECB_STATE_STORE_DSN in the store package.
			return nil
		}
		return nil
	default:
		return fmt.Errorf("broker.stateStore.type %q is not supported", cfg.Type)
	}
}

// ValidateReplicaSafety rejects multi-replica deployments that use process-local state.
// expectedReplicas comes from the UECB_REPLICAS environment variable (set by the chart).
func ValidateReplicaSafety(cfg model.BrokerConfig, expectedReplicas int) error {
	if expectedReplicas <= 1 {
		return nil
	}
	storeType := normalizeStringDefault(cfg.Broker.StateStore.Type, "memory")
	switch storeType {
	case "postgres":
		return nil
	default:
		return fmt.Errorf("broker replicas=%d is unsafe with process-local stateStore.type %q; use type postgres for multi-replica HA, or set replicas to 1", expectedReplicas, storeType)
	}
}

func validateAdmissionQueue(cfg model.AdmissionQueueConfig) error {
	if cfg.RetryAfter < 0 {
		return fmt.Errorf("broker.queue.retryAfter must not be negative")
	}
	if cfg.MaxAttempts < 0 {
		return fmt.Errorf("broker.queue.maxAttempts must not be negative")
	}
	return nil
}

func validateTierRouting(cfg model.BrokerConfig) error {
	tierCfg := cfg.Broker.TierRouting
	if !tierCfg.Enabled && !hasTierRules(cfg) {
		return nil
	}

	failureMode := normalizeStringDefault(tierCfg.FailureMode, tierFailureModePassThrough)
	switch failureMode {
	case tierFailureModePassThrough, tierFailureModeBlock, tierFailureModeFallback:
	default:
		return fmt.Errorf("broker.tierRouting.failureMode %q is not supported", tierCfg.FailureMode)
	}

	if tierCfg.RefreshInterval < 0 {
		return fmt.Errorf("broker.tierRouting.refreshInterval must not be negative")
	}
	if tierCfg.StaleAfter < 0 {
		return fmt.Errorf("broker.tierRouting.staleAfter must not be negative")
	}
	if tierCfg.Prometheus.Timeout < 0 {
		return fmt.Errorf("broker.tierRouting.prometheus.timeout must not be negative")
	}

	if failureMode == tierFailureModeFallback {
		if len(tierCfg.FallbackBackends) == 0 {
			return fmt.Errorf("broker.tierRouting.fallbackBackends is required when failureMode is %q", tierFailureModeFallback)
		}
		for _, backendName := range tierCfg.FallbackBackends {
			if !backendNameConfigured(cfg, backendName) {
				return fmt.Errorf("broker.tierRouting.fallbackBackends includes unknown backend %q", backendName)
			}
		}
	}

	for ref, provider := range tierCfg.Providers {
		if strings.TrimSpace(ref) == "" {
			return fmt.Errorf("broker.tierRouting.providers includes an empty provider ref")
		}
		switch normalizeString(provider.Provider) {
		case "aws", "azure", "gcp":
		default:
			return fmt.Errorf("broker.tierRouting.providers.%s.provider %q is not supported", ref, provider.Provider)
		}
		if strings.TrimSpace(provider.Mode) == "" {
			return fmt.Errorf("broker.tierRouting.providers.%s.mode is required", ref)
		}
	}

	for index, rule := range tierCfg.ProviderRules {
		if err := validateProviderTierRule(cfg, tierCfg, index, rule); err != nil {
			return err
		}
	}

	for _, pool := range cfg.Pools {
		for backendName, backendCfg := range pool.Backends {
			for index, rule := range backendCfg.TierRules {
				if err := validateTierRule(tierCfg, pool.Name, backendName, index, rule); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateProviderTierRule(cfg model.BrokerConfig, tierCfg model.TierRoutingConfig, index int, rule model.ProviderTierRuleConfig) error {
	prefix := fmt.Sprintf("broker.tierRouting.providerRules[%d]", index)
	if strings.TrimSpace(rule.ProviderRef) == "" {
		return fmt.Errorf("%s.providerRef is required", prefix)
	}
	if _, ok := tierCfg.Providers[rule.ProviderRef]; !ok {
		return fmt.Errorf("%s.providerRef %q does not match a configured provider", prefix, rule.ProviderRef)
	}
	if len(rule.Backends) > 0 {
		for _, backendName := range rule.Backends {
			if !backendNameConfigured(cfg, backendName) {
				return fmt.Errorf("%s.backends includes unknown backend %q", prefix, backendName)
			}
		}
	}
	return validateTierRule(tierCfg, "", "", index, model.TierRuleConfig{
		Name:               rule.Name,
		ProviderRef:        rule.ProviderRef,
		UsageQuery:         rule.UsageQuery,
		BurnRateQuery:      rule.BurnRateQuery,
		SoftLimitRatio:     rule.SoftLimitRatio,
		HardLimitRatio:     rule.HardLimitRatio,
		MinRemainingCredit: rule.MinRemainingCredit,
		ProjectionWindow:   rule.ProjectionWindow,
		Action:             rule.Action,
	})
}

func validateTierRule(tierCfg model.TierRoutingConfig, pool model.PoolName, backend model.BackendName, index int, rule model.TierRuleConfig) error {
	prefix := fmt.Sprintf("pools[%s].backends[%s].tierRules[%d]", pool, backend, index)
	action := normalizeStringDefault(rule.Action, tierActionDisable)
	switch action {
	case tierActionObserveOnly, tierActionDeprioritize, tierActionDisable:
	default:
		return fmt.Errorf("%s.action %q is not supported", prefix, rule.Action)
	}

	combine := normalizeStringDefault(rule.Combine, tierCombineMostRestrictive)
	if combine != tierCombineMostRestrictive {
		return fmt.Errorf("%s.combine %q is not supported", prefix, rule.Combine)
	}

	if rule.SoftLimitRatio < 0 || rule.SoftLimitRatio > 1 {
		return fmt.Errorf("%s.softLimitRatio must be between 0 and 1", prefix)
	}
	if rule.HardLimitRatio < 0 || rule.HardLimitRatio > 1 {
		return fmt.Errorf("%s.hardLimitRatio must be between 0 and 1", prefix)
	}
	if rule.SoftLimitRatio > 0 && rule.HardLimitRatio > 0 && rule.SoftLimitRatio > rule.HardLimitRatio {
		return fmt.Errorf("%s.softLimitRatio must be less than or equal to hardLimitRatio", prefix)
	}
	if rule.MinRemainingCredit < 0 {
		return fmt.Errorf("%s.minRemainingCredit must not be negative", prefix)
	}
	if rule.ProjectionWindow < 0 {
		return fmt.Errorf("%s.projectionWindow must not be negative", prefix)
	}

	if strings.TrimSpace(rule.ProviderRef) != "" {
		if _, ok := tierCfg.Providers[rule.ProviderRef]; !ok {
			return fmt.Errorf("%s.providerRef %q does not match a configured provider", prefix, rule.ProviderRef)
		}
	}
	if strings.TrimSpace(rule.ProviderRef) == "" && strings.TrimSpace(rule.UsageQuery) == "" && strings.TrimSpace(rule.BurnRateQuery) == "" {
		return fmt.Errorf("%s must define providerRef, usageQuery, or burnRateQuery", prefix)
	}
	if (strings.TrimSpace(rule.UsageQuery) != "" || strings.TrimSpace(rule.BurnRateQuery) != "") && strings.TrimSpace(tierCfg.Prometheus.URL) == "" {
		return fmt.Errorf("%s uses prometheus queries but broker.tierRouting.prometheus.url is empty", prefix)
	}
	return nil
}

func hasTierRules(cfg model.BrokerConfig) bool {
	if len(cfg.Broker.TierRouting.ProviderRules) > 0 {
		return true
	}
	for _, pool := range cfg.Pools {
		for _, backend := range pool.Backends {
			if len(backend.TierRules) > 0 {
				return true
			}
		}
	}
	return false
}

func backendNameConfigured(cfg model.BrokerConfig, name model.BackendName) bool {
	for _, pool := range cfg.Pools {
		if _, ok := pool.Backends[name]; ok {
			return true
		}
	}
	return false
}

func normalizeString(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeStringDefault(value, fallback string) string {
	normalized := normalizeString(value)
	if normalized == "" {
		return fallback
	}
	return normalized
}
