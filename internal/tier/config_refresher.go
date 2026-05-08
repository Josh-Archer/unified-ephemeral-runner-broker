package tier

import (
	"context"
	"fmt"
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
	decisions := make([]Decision, 0)
	var firstErr error
	for _, pool := range r.cfg.Pools {
		for backendName, backendCfg := range pool.Backends {
			for _, ruleCfg := range backendCfg.TierRules {
				decision, err := r.refreshRule(ctx, pool.Name, backendName, ruleCfg, now)
				if err != nil && firstErr == nil {
					firstErr = err
				}
				decisions = append(decisions, decision)
			}
		}
	}
	return decisions, firstErr
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
