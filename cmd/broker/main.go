package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/api"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	arcbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/arc"
	azurebackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/azurefunctions"
	azurevmbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/azurevm"
	cloudbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/cloudrun"
	codebuildbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/codebuild"
	desktopbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/desktop"
	ec2backend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/ec2"
	gcebackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/gce"
	lambdabackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/lambda"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/runtime"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
)

func main() {
	cfg, err := config.Load(os.Getenv("UECB_CONFIG_PATH"))
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	if err := config.ValidateReplicaSafety(cfg, expectedReplicasFromEnv()); err != nil {
		log.Fatalf("replica safety: %v", err)
	}

	secretReader, err := runtime.NewSecretReaderFromEnv()
	if err != nil {
		log.Fatalf("configure kubernetes secret reader: %v", err)
	}

	registry := backend.NewRegistry(
		arcbackend.New(cfg),
		codebuildbackend.New(cfg, secretReader),
		lambdabackend.New(cfg, secretReader),
		cloudbackend.New(cfg, secretReader),
		azurebackend.New(cfg, secretReader),
		azurevmbackend.New(cfg),
		desktopbackend.New(cfg),
		ec2backend.New(cfg, secretReader),
		gcebackend.New(cfg, secretReader),
	)
	healthChecker, err := runtime.NewSecretRefCheckerFromEnv(cfg)
	if err != nil {
		log.Fatalf("configure runtime dependencies: %v", err)
	}

	service := api.NewService(cfg, registry, healthChecker.Check)
	var tierManager *tier.Manager
	if cfg.Broker.TierRouting.Enabled {
		tierManager = tier.NewManager()
		service.SetTierManager(tierManager)
		refresher := tier.NewConfigRefresher(cfg, secretReader)
		if cfg.Broker.TierRouting.RefreshOnStartup {
			decisions, err := refresher.Refresh(context.Background())
			if err != nil {
				log.Printf("initial tier refresh failed: %v", err)
			}
			for _, decision := range decisions {
				tierManager.SetDecision(decision)
			}
		}
		tier.StartRefreshLoop(context.Background(), tierManager, refresher, cfg.Broker.TierRouting.RefreshInterval)
	}
	server := api.NewServerWithPolicy(
		service,
		[]string{"https://token.actions.githubusercontent.com"},
		cfg.Broker.API.OIDCAudience,
		cfg.Broker.AllowUnauthenticated,
		cfg.Broker.API.OIDCPolicy,
	)

	ctx := context.Background()
	identity := leaderIdentity(cfg.Broker.HA.Identity)
	leaseTTL := cfg.Broker.HA.LeaseTTL
	if leaseTTL <= 0 {
		leaseTTL = 15 * time.Second
	}
	elector := store.AsLeaderElector(service.Store())
	useLeaderElection := config.HAEnabled(cfg.Broker)

	startBackground := func(name string, interval time.Duration, fn func(time.Time)) {
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for now := range ticker.C {
				if useLeaderElection {
					ok, err := elector.TryAcquireLeadership(ctx, store.LeaderLeaseName, identity, leaseTTL)
					if err != nil {
						log.Printf("leader election failed: %v", err)
						continue
					}
					if !ok {
						continue
					}
				}
				fn(now)
			}
		}()
		log.Printf("background worker %s interval=%s leaderElection=%v identity=%s", name, interval, useLeaderElection, identity)
	}

	startBackground("sweep-expired", 30*time.Second, func(now time.Time) {
		service.SweepExpired(now)
	})
	startBackground("reconcile-warm", 30*time.Second, func(time.Time) {
		service.ReconcileWarmPools()
	})
	startBackground("reconcile-backend-health", 30*time.Second, func(time.Time) {
		service.ReconcileBackendHealth()
	})
	startBackground("reconcile-queue", 10*time.Second, func(now time.Time) {
		service.ReconcileQueue(context.Background(), now)
	})

	if tierManager != nil {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for now := range ticker.C {
				tierManager.MarkStale(cfg.Broker.TierRouting.StaleAfter, now)
			}
		}()
	}

	addr := os.Getenv("UECB_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	log.Printf("listening on %s stateStore=%s ha=%v", addr, strings.TrimSpace(cfg.Broker.StateStore.Type), useLeaderElection)
	log.Fatal(http.ListenAndServe(addr, server.Handler()))
}

func expectedReplicasFromEnv() int {
	raw := strings.TrimSpace(os.Getenv("UECB_REPLICAS"))
	if raw == "" {
		return 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func leaderIdentity(configured string) string {
	if id := strings.TrimSpace(configured); id != "" {
		return id
	}
	if id := strings.TrimSpace(os.Getenv("UECB_POD_NAME")); id != "" {
		return id
	}
	if id := strings.TrimSpace(os.Getenv("HOSTNAME")); id != "" {
		return id
	}
	return "broker"
}
