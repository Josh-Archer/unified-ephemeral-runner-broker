package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/api"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	arcbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/arc"
	azurebackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/azurefunctions"
	azurevmbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/azurevm"
	cloudbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/cloudrun"
	codebuildbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/codebuild"
	ec2backend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/ec2"
	gcebackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/gce"
	lambdabackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/lambda"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/runtime"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
)

func main() {
	cfg, err := config.Load(os.Getenv("UECB_CONFIG_PATH"))
	if err != nil {
		log.Fatalf("load config: %v", err)
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
	server := api.NewServer(
		service,
		[]string{"https://token.actions.githubusercontent.com"},
		cfg.Broker.API.OIDCAudience,
		cfg.Broker.AllowUnauthenticated,
	)

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for now := range ticker.C {
			service.SweepExpired(now)
		}
	}()

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			service.ReconcileWarmPools()
		}
	}()

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			service.ReconcileBackendHealth()
		}
	}()

	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for now := range ticker.C {
			service.ReconcileQueue(context.Background(), now)
		}
	}()

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

	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, server.Handler()))
}
