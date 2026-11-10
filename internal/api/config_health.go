package api

import "github.com/prometheus/client_golang/prometheus"

// disabledPoolMetricName is the gauge scraped from /metrics when startup
// validation disabled one or more pools.
const disabledPoolMetricName = "uecb_pool_config_disabled"

// DisabledPool is a pool left out of serving because its config failed
// validation. Error is the validation message and names the pool and backend.
type DisabledPool struct {
	Pool    string `json:"pool"`
	Backend string `json:"backend,omitempty"`
	Error   string `json:"error"`
}

// SetDisabledPools records pools that failed config validation. Call it
// before NewServer so /metrics includes them.
func (s *Service) SetDisabledPools(pools []DisabledPool) {
	if s == nil {
		return
	}
	if len(pools) == 0 {
		s.disabledPools = nil
		return
	}
	s.disabledPools = append([]DisabledPool(nil), pools...)
}

// DisabledPools returns a copy of the pools disabled at startup.
func (s *Service) DisabledPools() []DisabledPool {
	if s == nil || len(s.disabledPools) == 0 {
		return nil
	}
	return append([]DisabledPool(nil), s.disabledPools...)
}

func registerDisabledPools(reg prometheus.Registerer, pools []DisabledPool) error {
	if reg == nil || len(pools) == 0 {
		return nil
	}
	gauge := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: disabledPoolMetricName,
		Help: "Pools disabled at startup because their config failed validation. 1 means the pool is not serving. The error label is the validation message and names the pool and backend.",
	}, []string{"pool", "backend", "error"})
	if err := reg.Register(gauge); err != nil {
		return err
	}
	for _, pool := range pools {
		gauge.WithLabelValues(pool.Pool, pool.Backend, pool.Error).Set(1)
	}
	return nil
}
