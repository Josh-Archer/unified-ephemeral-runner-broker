package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scaleset"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scheduler"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	scaleSetDemandSource = "scaleset"
	scaleSetMetadataKey  = "scale_set"
)

var (
	scaleSetDecisions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "uecb_scaleset_decisions_total",
		Help: "Runner scale set demand decisions by pool, mode, and outcome (match, disagree, allocated, capacity).",
	}, []string{"pool", "mode", "outcome"})
	scaleSetRunners = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "uecb_scaleset_runners_allocated_total",
		Help: "Runners placed from Runner Scale Set statistics, by pool. This path does not list queued workflow runs.",
	}, []string{"pool"})
)

func init() {
	// The default registry is what /metrics serves. Tests install a fresh
	// registry before NewServer, so NewServer registers the same collectors
	// there too. Inc still updates this process-wide vector.
	registerScaleSetMetrics(prometheus.DefaultRegisterer)
}

func registerScaleSetMetrics(reg prometheus.Registerer) {
	if reg == nil {
		reg = prometheus.DefaultRegisterer
	}
	for _, collector := range []prometheus.Collector{scaleSetDecisions, scaleSetRunners} {
		if err := reg.Register(collector); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				panic(err)
			}
		}
	}
}

// ScaleSetOccupancy counts runners already covering a pool.
// running is ready or warm. allocating is reserved or pending.
func (s *Service) ScaleSetOccupancy(poolName model.PoolName) (running, allocating int) {
	if s == nil || s.store == nil {
		return 0, 0
	}
	for _, status := range s.store.List() {
		if status.Pool != poolName {
			continue
		}
		switch status.State {
		case model.StateReady, model.StateWarm:
			running++
		case model.StateReserved, model.StatePending:
			allocating++
		}
	}
	return running, allocating
}

// RunScaleSetListener opens message sessions and listens until ctx is cancelled
// or a listener fails. open must not include secret values in its error.
func (s *Service) RunScaleSetListener(ctx context.Context, open func(context.Context) (map[string]scaleset.Client, error), isLeader func(context.Context) (bool, error)) error {
	if s == nil || !s.cfg.Broker.ScaleSetListener.Enabled {
		return nil
	}
	if open == nil {
		return errors.New("scale set listener has no session source")
	}
	clients, err := open(ctx)
	if err != nil {
		return err
	}
	return s.RunScaleSetListenerWithClients(ctx, clients, isLeader)
}

// RunScaleSetListenerWithClients listens on the given sessions, keyed by scale
// set name. It does not list queued workflow runs.
func (s *Service) RunScaleSetListenerWithClients(ctx context.Context, clients map[string]scaleset.Client, isLeader func(context.Context) (bool, error)) error {
	if s == nil || !s.cfg.Broker.ScaleSetListener.Enabled {
		return nil
	}
	targets := make([]scaleset.Target, 0, len(clients))
	for _, pool := range s.cfg.Pools {
		name := strings.TrimSpace(pool.ScaleSet.Name)
		if name == "" || pool.ScaleSet.ID <= 0 {
			continue
		}
		client, ok := clients[name]
		if !ok || client == nil {
			return fmt.Errorf("scale set %q has no message session", name)
		}
		scaler, err := s.newScaleSetScaler(pool)
		if err != nil {
			return err
		}
		targets = append(targets, scaleset.Target{
			ScaleSetID: pool.ScaleSet.ID,
			MaxRunners: scaleSetMaxRunners(pool),
			Client:     scaleset.WithLeader(client, isLeader),
			Scaler:     scaler,
		})
	}
	if len(targets) == 0 {
		return errors.New("scale set listener enabled but no pool has scaleSet.name and id")
	}
	return scaleset.Run(ctx, targets)
}

func (s *Service) newScaleSetScaler(pool model.PoolConfig) (*scaleset.DemandScaler, error) {
	var last string
	return scaleset.NewDemandScaler(scaleset.DemandConfig{
		Pool:         string(pool.Name),
		ScaleSetName: strings.TrimSpace(pool.ScaleSet.Name),
		Mode:         s.cfg.Broker.ScaleSetListener.EffectiveMode(),
		Inventory: func(context.Context) (int, int, error) {
			running, allocating := s.ScaleSetOccupancy(pool.Name)
			return running, allocating, nil
		},
		Allocate: func(ctx context.Context) error {
			return s.AllocateForScaleSet(ctx, pool)
		},
		OnDecision: func(d scaleset.Decision) {
			key := fmt.Sprintf("%d:%d:%t", d.Delta, d.Allocated, d.CapacityLimited)
			if key == last {
				return
			}
			last = key
			recordScaleSetDecision(d)
			log.Printf("scale set decision pool=%s scale_set=%s mode=%s desired=%d running=%d allocating=%d delta=%d allocated=%d capacity_limited=%t",
				d.Pool, d.ScaleSetName, d.Mode, d.Desired, d.Running, d.Allocating, d.Delta, d.Allocated, d.CapacityLimited)
		},
	})
}

// AllocateForScaleSet places one runner through the normal tier, quota, and
// fallback path. The runner labels are the scale set name. JIT registration
// is not done here.
func (s *Service) AllocateForScaleSet(ctx context.Context, pool model.PoolConfig) error {
	request := model.AllocationRequest{
		Pool:       pool.Name,
		JobTimeout: pool.WebhookJobTimeout,
		Labels:     scaleSetRunnerLabels(pool),
	}
	status, err := s.Allocate(ctx, request)
	if err != nil {
		if scaleSetCapacity(err) {
			return fmt.Errorf("%w: %s", scaleset.ErrCapacity, err.Error())
		}
		return err
	}
	s.stampScaleSet(status, strings.TrimSpace(pool.ScaleSet.Name))
	return nil
}

func scaleSetCapacity(err error) bool {
	return errors.Is(err, scheduler.ErrNoCapacity) ||
		errors.Is(err, scheduler.ErrQuotaExceeded) ||
		errors.Is(err, ErrBackendLiveCapacity) ||
		backend.IsCapacityExhausted(err)
}

func scaleSetRunnerLabels(pool model.PoolConfig) []string {
	name := strings.TrimSpace(pool.ScaleSet.Name)
	if strings.EqualFold(name, "self-hosted") {
		return []string{"self-hosted"}
	}
	return []string{"self-hosted", name}
}

func (s *Service) stampScaleSet(status model.AllocationStatus, name string) {
	if status.ID == "" {
		return
	}
	metadata := make(map[string]string, len(status.Metadata)+2)
	for key, value := range status.Metadata {
		metadata[key] = value
	}
	metadata["demand_source"] = scaleSetDemandSource
	metadata[scaleSetMetadataKey] = name
	status.Metadata = metadata
	_ = s.store.Save(status)
}

func scaleSetMaxRunners(pool model.PoolConfig) int {
	total := 0
	for _, cfg := range pool.Backends {
		if !cfg.Enabled || cfg.MaxRunners <= 0 {
			continue
		}
		total += cfg.MaxRunners
	}
	return total
}

func recordScaleSetDecision(d scaleset.Decision) {
	outcome := "match"
	switch {
	case d.CapacityLimited:
		outcome = "capacity"
	case d.Allocated > 0:
		outcome = "allocated"
	case d.Delta != 0:
		outcome = "disagree"
	}
	scaleSetDecisions.WithLabelValues(d.Pool, d.Mode, outcome).Inc()
	if d.Allocated > 0 {
		scaleSetRunners.WithLabelValues(d.Pool).Add(float64(d.Allocated))
	}
}

// scaleSetOwnsDemand reports whether allocate mode replaces webhook and
// queued-run demand for this pool.
func (s *Service) scaleSetOwnsDemand(pool model.PoolConfig) bool {
	if s == nil || !s.cfg.Broker.ScaleSetListener.Allocates() {
		return false
	}
	return strings.TrimSpace(pool.ScaleSet.Name) != "" && pool.ScaleSet.ID > 0
}

// queuedReconcileNeeded is false only when every webhook pool is owned by the
// scale-set listener. A broker with no webhook pools still scans if reconcile
// is invoked, matching the previous direct-call behavior.
func (s *Service) queuedReconcileNeeded() bool {
	sawWebhook := false
	for _, pool := range s.cfg.Pools {
		if len(pool.WebhookLabels) == 0 {
			continue
		}
		sawWebhook = true
		if !s.scaleSetOwnsDemand(pool) {
			return true
		}
	}
	return !sawWebhook
}

// scaleSetCovers reports whether a scale-set-owned pool would have taken this job.
func (s *Service) scaleSetCovers(jobLabels []string, repository string) bool {
	if !webhookJobLabelsSpecific(jobLabels) {
		return false
	}
	for _, pool := range s.cfg.Pools {
		if !s.scaleSetOwnsDemand(pool) || !jobLabelsMatchPool(pool, jobLabels) {
			continue
		}
		if repository != "" && !pool.ServesWebhookRepository(repository) {
			continue
		}
		return true
	}
	return false
}
