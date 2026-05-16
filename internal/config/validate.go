package config

import (
	"fmt"
	"strings"

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
	if err := validateTierRouting(cfg); err != nil {
		return err
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
