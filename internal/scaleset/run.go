package scaleset

import (
	"context"
	"errors"
	"fmt"
)

// Target is one scale set the broker listens to.
type Target struct {
	ScaleSetID int
	MaxRunners int
	Client     Client
	Scaler     Scaler
}

// Run starts one listener per target and returns the first error.
// The other listeners are cancelled. A cancelled parent context returns
// ctx.Err().
func Run(ctx context.Context, targets []Target) error {
	if len(targets) == 0 {
		return errors.New("scaleset: no listeners")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, len(targets))
	for _, target := range targets {
		target := target
		go func() {
			errCh <- runTarget(ctx, target)
		}()
	}
	var first error
	for i := 0; i < len(targets); i++ {
		err := <-errCh
		if err == nil || errors.Is(err, context.Canceled) {
			continue
		}
		if first == nil {
			first = err
			cancel()
		}
	}
	if first != nil {
		return first
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func runTarget(ctx context.Context, target Target) error {
	listener, err := New(target.Client, Config{ScaleSetID: target.ScaleSetID, MaxRunners: target.MaxRunners})
	if err != nil {
		return err
	}
	if err := listener.Run(ctx, target.Scaler); err != nil {
		return fmt.Errorf("scale set %d: %w", target.ScaleSetID, err)
	}
	return nil
}
