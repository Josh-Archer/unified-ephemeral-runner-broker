package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// GitHub Actions variable names: letters, digits and underscores, not
// starting with a digit or GITHUB_.
var actionsVariableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var ownerRepo = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

func validateHostedOverflow(cfg model.BrokerConfig) error {
	for _, pool := range cfg.Pools {
		overflow := pool.HostedOverflow
		if overflow == nil || !overflow.Enabled {
			continue
		}
		prefix := fmt.Sprintf("pools[%s].hostedOverflow", pool.Name)
		if len(pool.WebhookLabels) == 0 {
			return fmt.Errorf("%s needs webhookLabels: only webhook-queued jobs can be steered by a repository variable", prefix)
		}
		name := strings.TrimSpace(overflow.Variable)
		if !actionsVariableName.MatchString(name) || strings.HasPrefix(strings.ToUpper(name), "GITHUB_") {
			return fmt.Errorf("%s.variable %q is not a valid GitHub Actions variable name", prefix, overflow.Variable)
		}
		self := strings.TrimSpace(overflow.SelfHostedValue)
		hosted := strings.TrimSpace(overflow.HostedValue)
		if self == "" || hosted == "" {
			return fmt.Errorf("%s.selfHostedValue and hostedValue are required", prefix)
		}
		if self == hosted {
			return fmt.Errorf("%s.selfHostedValue and hostedValue must differ", prefix)
		}
		if len(overflow.Repositories) == 0 {
			return fmt.Errorf("%s.repositories must list at least one owner/repo", prefix)
		}
		seen := map[string]bool{}
		for _, repo := range overflow.Repositories {
			repo = strings.TrimSpace(repo)
			if !ownerRepo.MatchString(repo) {
				return fmt.Errorf("%s.repositories entry %q must be owner/repo", prefix, repo)
			}
			if seen[strings.ToLower(repo)] {
				return fmt.Errorf("%s.repositories lists %q twice", prefix, repo)
			}
			seen[strings.ToLower(repo)] = true
		}
		if overflow.SaturateAfter < 0 || (overflow.SaturateAfter > 0 && overflow.SaturateAfter < 30*time.Second) {
			return fmt.Errorf("%s.saturateAfter must be at least 30s", prefix)
		}
		if overflow.RecoverAfter < 0 {
			return fmt.Errorf("%s.recoverAfter must not be negative", prefix)
		}
		if len(overflow.BudgetRules) > 0 && !cfg.Broker.TierRouting.Enabled {
			return fmt.Errorf("%s.budgetRules need broker.tierRouting.enabled", prefix)
		}
	}
	return nil
}
