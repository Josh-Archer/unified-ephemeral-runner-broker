package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/api"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/runtime"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/scaleset"
)

// startScaleSetListener runs the Runner Scale Set listener beside webhook
// delivery and queued-run reconcile. A session error is retried; it does not
// stop the broker. Secret values are never logged.
func startScaleSetListener(ctx context.Context, cfg model.BrokerConfig, service *api.Service, secrets runtime.SecretReader, isLeader func(context.Context) (bool, error)) {
	go func() {
		backoff := time.Second
		for {
			if ctx.Err() != nil {
				return
			}
			if isLeader != nil {
				ok, err := isLeader(ctx)
				if err != nil {
					log.Printf("scale set listener leader election: %v", err)
					if !sleepCtx(ctx, backoff) {
						return
					}
					continue
				}
				if !ok {
					if !sleepCtx(ctx, 5*time.Second) {
						return
					}
					continue
				}
			}
			err := service.RunScaleSetListener(ctx, func(ctx context.Context) (map[string]scaleset.Client, error) {
				return scaleSetClients(ctx, cfg, secrets)
			}, isLeader)
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, scaleset.ErrNotLeader) {
				continue
			}
			log.Printf("scale set listener stopped: %v", err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	}()
	log.Printf("scale set listener enabled mode=%s", cfg.Broker.ScaleSetListener.EffectiveMode())
}

func scaleSetClients(ctx context.Context, cfg model.BrokerConfig, secrets runtime.SecretReader) (map[string]scaleset.Client, error) {
	ref := cfg.Broker.ScaleSetListener.SessionSecretRef
	data, err := secrets.ReadSecret(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("read scale set session secret %q: %w", ref, err)
	}
	return scaleset.HTTPClientsFromSecret(cfg.Pools, data, nil, func(ctx context.Context) (map[string]string, error) {
		return secrets.ReadSecret(ctx, ref)
	})
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
