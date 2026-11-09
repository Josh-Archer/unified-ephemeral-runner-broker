package scaleset

import (
	"context"
	"errors"
	"fmt"
)

const (
	// ModeObserve records demand and does not place runners.
	ModeObserve = "observe"
	// ModeAllocate places one runner per missing assigned job.
	ModeAllocate = "allocate"
)

// ErrCapacity means the pool cannot place another runner right now.
// Scale still returns nil so the message is acknowledged. The listener keeps
// the last statistics and tries the remainder on the next poll, including an
// empty long poll.
var ErrCapacity = errors.New("scaleset: capacity")

// Decision is one comparison of GitHub's assigned jobs with local runners.
type Decision struct {
	Pool             string
	ScaleSetName     string
	Mode             string
	Desired          int
	Running          int
	Allocating       int
	Delta            int
	Allocated        int
	CapacityLimited  bool
	AcquireRequested int
}

// DemandConfig builds a DemandScaler. Inventory is required. Allocate is
// required in ModeAllocate. Acquire is optional and is ignored in observe
// mode: acquiring a job without a JIT-registered scale-set runner strands it.
type DemandConfig struct {
	Pool         string
	ScaleSetName string
	Mode         string
	Inventory    func(ctx context.Context) (running, allocating int, err error)
	Allocate     func(ctx context.Context) error
	Acquire      func(ctx context.Context, requestIDs []int64) error
	OnDecision   func(Decision)
}

// DemandScaler turns TotalAssignedJobs minus local runners into allocations.
// Scale is not safe for concurrent calls.
type DemandScaler struct {
	pool         string
	scaleSetName string
	mode         string
	inventory    func(ctx context.Context) (running, allocating int, err error)
	allocate     func(ctx context.Context) error
	acquire      func(ctx context.Context, requestIDs []int64) error
	onDecision   func(Decision)
	last         *Statistic
}

// NewDemandScaler validates cfg and returns a scaler.
func NewDemandScaler(cfg DemandConfig) (*DemandScaler, error) {
	if cfg.Pool == "" {
		return nil, errors.New("pool is required")
	}
	mode := cfg.Mode
	if mode == "" {
		mode = ModeObserve
	}
	if mode != ModeObserve && mode != ModeAllocate {
		return nil, fmt.Errorf("unsupported scale set mode %q", mode)
	}
	if cfg.Inventory == nil {
		return nil, errors.New("inventory is required")
	}
	if mode == ModeAllocate && cfg.Allocate == nil {
		return nil, errors.New("allocate is required when mode is allocate")
	}
	return &DemandScaler{
		pool:         cfg.Pool,
		scaleSetName: cfg.ScaleSetName,
		mode:         mode,
		inventory:    cfg.Inventory,
		allocate:     cfg.Allocate,
		acquire:      cfg.Acquire,
		onDecision:   cfg.OnDecision,
	}, nil
}

// Scale applies one message. A nil message reapplies the last statistics so
// an empty long poll can finish a capacity-limited scale-up. The upstream
// listener does not remember statistics; caching them is how this scaler
// converges between job messages.
func (d *DemandScaler) Scale(ctx context.Context, msg *Message) error {
	if msg == nil {
		if d.last == nil {
			return nil
		}
		return d.apply(ctx, *d.last, 0)
	}
	acquired := 0
	if d.mode == ModeAllocate && d.acquire != nil && len(msg.JobAvailableMessages) > 0 {
		ids := make([]int64, 0, len(msg.JobAvailableMessages))
		for _, job := range msg.JobAvailableMessages {
			if job == nil || job.RunnerRequestID == 0 {
				continue
			}
			ids = append(ids, job.RunnerRequestID)
		}
		if len(ids) > 0 {
			if err := d.acquire(ctx, ids); err != nil {
				return err
			}
			acquired = len(ids)
		}
	}
	if msg.Statistics == nil {
		return nil
	}
	stats := *msg.Statistics
	d.last = &stats
	return d.apply(ctx, stats, acquired)
}

func (d *DemandScaler) apply(ctx context.Context, stats Statistic, acquired int) error {
	running, allocating, err := d.inventory(ctx)
	if err != nil {
		return err
	}
	delta := stats.TotalAssignedJobs - running - allocating
	deficit := delta
	if deficit < 0 {
		deficit = 0
	}
	decision := Decision{
		Pool:             d.pool,
		ScaleSetName:     d.scaleSetName,
		Mode:             d.mode,
		Desired:          stats.TotalAssignedJobs,
		Running:          running,
		Allocating:       allocating,
		Delta:            delta,
		AcquireRequested: acquired,
	}
	if d.mode != ModeAllocate || deficit == 0 || d.allocate == nil {
		d.emit(decision)
		return nil
	}
	for i := 0; i < deficit; i++ {
		if err := ctx.Err(); err != nil {
			d.emit(decision)
			return err
		}
		if err := d.allocate(ctx); err != nil {
			if errors.Is(err, ErrCapacity) {
				decision.CapacityLimited = true
				d.emit(decision)
				return nil
			}
			d.emit(decision)
			return err
		}
		decision.Allocated++
	}
	d.emit(decision)
	return nil
}

func (d *DemandScaler) emit(decision Decision) {
	if d.onDecision != nil {
		d.onDecision(decision)
	}
}
