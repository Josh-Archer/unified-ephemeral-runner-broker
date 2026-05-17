package tier

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier/promclient"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier/provider"
)

type SecretReader interface {
	ReadSecret(context.Context, string) (map[string]string, error)
}

type ConfigRefresher struct {
	cfg          model.BrokerConfig
	secretReader SecretReader
	providers    provider.Client
	now          func() time.Time
}

func NewConfigRefresher(cfg model.BrokerConfig, secretReader SecretReader) *ConfigRefresher {
	return &ConfigRefresher{
		cfg:          cfg,
		secretReader: secretReader,
		providers:    provider.NewHTTPClient(resolvePrometheusTimeout(cfg.Broker.TierRouting.Prometheus.Timeout)),
		now:          time.Now,
	}
}

func (r *ConfigRefresher) Refresh(ctx context.Context) ([]Decision, error) {
	if r == nil || !r.cfg.Broker.TierRouting.Enabled {
		return nil, nil
	}
	now := r.now()
	decisions := map[decisionKey]Decision{}
	var firstErr error
	for _, ruleCfg := range r.cfg.Broker.TierRouting.ProviderRules {
		decision, err := r.refreshProviderRule(ctx, ruleCfg, now)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		r.addProviderRuleDecisions(decisions, decision, ruleCfg)
	}
	for _, pool := range r.cfg.Pools {
		for backendName, backendCfg := range pool.Backends {
			for _, ruleCfg := range backendCfg.TierRules {
				decision, err := r.refreshRule(ctx, pool.Name, backendName, ruleCfg, now)
				if err != nil && firstErr == nil {
					firstErr = err
				}
				mergeDecision(decisions, decision)
			}
		}
	}
	return sortedDecisions(decisions), firstErr
}

func (r *ConfigRefresher) refreshRule(ctx context.Context, pool model.PoolName, backendName model.BackendName, ruleCfg model.TierRuleConfig, now time.Time) (Decision, error) {
	rule := Rule{
		Name:               ruleCfg.Name,
		Action:             normalizeAction(ruleCfg.Action),
		SoftLimitRatio:     ruleCfg.SoftLimitRatio,
		HardLimitRatio:     ruleCfg.HardLimitRatio,
		MinRemainingCredit: ruleCfg.MinRemainingCredit,
		ProjectionWindow:   ruleCfg.ProjectionWindow,
		Combine:            CombineMostRestrictive,
	}
	snapshots := make([]SourceSnapshot, 0, 2)
	var providerSnapshot SourceSnapshot
	var firstErr error
	if strings.TrimSpace(ruleCfg.ProviderRef) != "" {
		var err error
		providerSnapshot, err = r.providerSnapshot(ctx, ruleCfg.ProviderRef, now)
		if err != nil {
			firstErr = err
			snapshots = append(snapshots, SourceSnapshot{
				Source:    ruleCfg.ProviderRef,
				UpdatedAt: now,
				Err:       err.Error(),
			})
		} else {
			snapshots = append(snapshots, providerSnapshot)
		}
	}

	if strings.TrimSpace(ruleCfg.UsageQuery) != "" {
		value, queryErr := r.prometheusValue(ctx, ruleCfg.UsageQuery)
		if queryErr != nil {
			if firstErr == nil {
				firstErr = queryErr
			}
			snapshots = append(snapshots, SourceSnapshot{
				Source:    "prometheus",
				UpdatedAt: now,
				Err:       queryErr.Error(),
			})
		} else if providerSnapshot.Limit > 0 || providerSnapshot.RemainingCredit > 0 {
			providerSnapshot.Used = value
			providerSnapshot.UpdatedAt = now
			snapshots = replaceProviderSnapshot(snapshots, providerSnapshot)
		} else {
			snapshots = append(snapshots, SourceSnapshot{
				Source:    "prometheus",
				Limit:     1,
				Used:      value,
				UpdatedAt: now,
			})
		}
	}

	if strings.TrimSpace(ruleCfg.BurnRateQuery) != "" {
		value, queryErr := r.prometheusValue(ctx, ruleCfg.BurnRateQuery)
		if queryErr != nil {
			if firstErr == nil {
				firstErr = queryErr
			}
			snapshots = append(snapshots, SourceSnapshot{
				Source:    "prometheus-burn-rate",
				UpdatedAt: now,
				Err:       queryErr.Error(),
			})
		} else if providerSnapshot.Limit > 0 || providerSnapshot.RemainingCredit > 0 {
			providerSnapshot.BurnRate = value
			providerSnapshot.UpdatedAt = now
			snapshots = replaceProviderSnapshot(snapshots, providerSnapshot)
		} else {
			snapshots = append(snapshots, SourceSnapshot{
				Source:    "prometheus-burn-rate",
				Limit:     1,
				BurnRate:  value,
				UpdatedAt: now,
			})
		}
	}

	decision := Evaluate(EvaluationInput{
		Pool:      pool,
		Backend:   backendName,
		Rule:      rule,
		Snapshots: snapshots,
		Now:       now,
	})
	return decision, firstErr
}

func (r *ConfigRefresher) refreshProviderRule(ctx context.Context, ruleCfg model.ProviderTierRuleConfig, now time.Time) (Decision, error) {
	return r.refreshRule(ctx, "", "", model.TierRuleConfig{
		Name:               ruleCfg.Name,
		ProviderRef:        ruleCfg.ProviderRef,
		UsageQuery:         ruleCfg.UsageQuery,
		BurnRateQuery:      ruleCfg.BurnRateQuery,
		SoftLimitRatio:     ruleCfg.SoftLimitRatio,
		HardLimitRatio:     ruleCfg.HardLimitRatio,
		MinRemainingCredit: ruleCfg.MinRemainingCredit,
		ProjectionWindow:   ruleCfg.ProjectionWindow,
		Action:             ruleCfg.Action,
	}, now)
}

func (r *ConfigRefresher) addProviderRuleDecisions(decisions map[decisionKey]Decision, decision Decision, ruleCfg model.ProviderTierRuleConfig) {
	providerCfg, ok := r.cfg.Broker.TierRouting.Providers[ruleCfg.ProviderRef]
	if !ok {
		return
	}
	for _, pool := range r.cfg.Pools {
		for backendName, backendCfg := range pool.Backends {
			if !providerRuleMatchesBackend(ruleCfg, providerCfg, backendName, backendCfg) {
				continue
			}
			next := decision
			next.Pool = pool.Name
			next.Backend = backendName
			mergeDecision(decisions, next)
		}
	}
}

func (r *ConfigRefresher) providerSnapshot(ctx context.Context, ref string, now time.Time) (SourceSnapshot, error) {
	providerCfg, ok := r.cfg.Broker.TierRouting.Providers[ref]
	if !ok {
		return SourceSnapshot{}, fmt.Errorf("provider ref %q is not configured", ref)
	}
	secret, err := r.readSecret(ctx, providerCfg.SecretRef)
	if err != nil {
		return SourceSnapshot{}, err
	}
	snapshotURL := strings.TrimSpace(secret["snapshot_url"])
	if snapshotURL == "" {
		snapshotURL = strings.TrimSpace(secret["url"])
	}
	if snapshotURL == "" {
		return SourceSnapshot{}, fmt.Errorf("provider %q secret is missing snapshot_url", ref)
	}
	token := strings.TrimSpace(secret["token"])
	if token == "" {
		token = strings.TrimSpace(secret["bearer_token"])
	}
	snapshot, err := r.providers.Snapshot(ctx, provider.Request{
		Provider: providerCfg.Provider,
		Mode:     providerCfg.Mode,
		URL:      snapshotURL,
		Token:    token,
		Source:   ref,
	})
	if err != nil {
		return SourceSnapshot{}, err
	}
	if snapshot.UpdatedAt.IsZero() {
		snapshot.UpdatedAt = now
	}
	return SourceSnapshot{
		Source:          snapshot.Source,
		Limit:           snapshot.Limit,
		Used:            snapshot.Used,
		RemainingCredit: snapshot.RemainingCredit,
		WindowEnd:       snapshot.WindowEnd,
		UpdatedAt:       snapshot.UpdatedAt,
		Err:             snapshot.Err,
	}, nil
}

func (r *ConfigRefresher) prometheusValue(ctx context.Context, query string) (float64, error) {
	promCfg := r.cfg.Broker.TierRouting.Prometheus
	token := ""
	if strings.TrimSpace(promCfg.SecretRef) != "" {
		secret, err := r.readSecret(ctx, promCfg.SecretRef)
		if err != nil {
			return 0, err
		}
		token = strings.TrimSpace(secret["bearer_token"])
		if token == "" {
			token = strings.TrimSpace(secret["token"])
		}
	}
	client := promclient.Client{
		BaseURL:     promCfg.URL,
		BearerToken: token,
		Timeout:     resolvePrometheusTimeout(promCfg.Timeout),
	}
	return client.QueryInstant(ctx, query)
}

func (r *ConfigRefresher) readSecret(ctx context.Context, ref string) (map[string]string, error) {
	if strings.TrimSpace(ref) == "" {
		return map[string]string{}, nil
	}
	if r.secretReader == nil {
		return nil, fmt.Errorf("secret reader is not configured")
	}
	return r.secretReader.ReadSecret(ctx, ref)
}

func replaceProviderSnapshot(snapshots []SourceSnapshot, providerSnapshot SourceSnapshot) []SourceSnapshot {
	for index, snapshot := range snapshots {
		if snapshot.Source == providerSnapshot.Source {
			snapshots[index] = providerSnapshot
			return snapshots
		}
	}
	return append(snapshots, providerSnapshot)
}

func normalizeAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case ActionObserveOnly:
		return ActionObserveOnly
	case ActionDeprioritize:
		return ActionDeprioritize
	default:
		return ActionDisable
	}
}

func resolvePrometheusTimeout(timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	return 2 * time.Second
}

type decisionKey struct {
	pool    model.PoolName
	backend model.BackendName
}

func mergeDecision(decisions map[decisionKey]Decision, next Decision) {
	key := decisionKey{pool: next.Pool, backend: next.Backend}
	current, ok := decisions[key]
	if !ok || decisionMoreRestrictive(next, current) {
		decisions[key] = next
	}
}

func sortedDecisions(decisions map[decisionKey]Decision) []Decision {
	result := make([]Decision, 0, len(decisions))
	for _, decision := range decisions {
		result = append(result, decision)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Pool != result[j].Pool {
			return result[i].Pool < result[j].Pool
		}
		return result[i].Backend < result[j].Backend
	})
	return result
}

func decisionMoreRestrictive(candidate, current Decision) bool {
	if decisionStateRank(candidate) != decisionStateRank(current) {
		return decisionStateRank(candidate) > decisionStateRank(current)
	}
	return decisionActionRank(candidate.Action) > decisionActionRank(current.Action)
}

func decisionStateRank(decision Decision) int {
	if decision.Stale {
		return 2
	}
	switch decision.State {
	case StateExceeded:
		return 4
	case StateApproaching:
		return 3
	case StateUnknown:
		return 2
	default:
		return 1
	}
}

func decisionActionRank(action string) int {
	switch normalizeAction(action) {
	case ActionDisable:
		return 3
	case ActionDeprioritize:
		return 2
	default:
		return 1
	}
}

func providerRuleMatchesBackend(ruleCfg model.ProviderTierRuleConfig, providerCfg model.TierProviderConfig, backendName model.BackendName, backendCfg model.BackendConfig) bool {
	if len(ruleCfg.Backends) > 0 {
		for _, candidate := range ruleCfg.Backends {
			if candidate == backendName {
				return true
			}
		}
		return false
	}
	return backendProvider(backendName, backendCfg) == strings.ToLower(strings.TrimSpace(providerCfg.Provider))
}

func backendProvider(backendName model.BackendName, backendCfg model.BackendConfig) string {
	for _, capability := range backendCfg.Capabilities {
		switch strings.ToLower(strings.TrimSpace(capability)) {
		case "cloud:aws":
			return "aws"
		case "cloud:azure":
			return "azure"
		case "cloud:gcp":
			return "gcp"
		}
	}
	switch backendName {
	case model.BackendCodeBuild, model.BackendLambda, model.BackendEC2:
		return "aws"
	case model.BackendCloudRun, model.BackendGCE:
		return "gcp"
	case model.BackendAzureFunctions, model.BackendAzureVM:
		return "azure"
	default:
		return ""
	}
}
