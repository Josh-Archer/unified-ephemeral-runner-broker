package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/api"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend"
	arcbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/arc"
	azurebackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/azurefunctions"
	cloudbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/cloudrun"
	lambdabackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/lambda"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/runtime"
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
		arcbackend.New(),
		lambdabackend.New(cfg, secretReader),
		cloudbackend.New(cfg, secretReader),
		azurebackend.New(),
	)
	healthChecker, err := runtime.NewSecretRefCheckerFromEnv(cfg)
	if err != nil {
		log.Fatalf("configure runtime dependencies: %v", err)
	}

	service := api.NewService(cfg, registry, healthChecker.Check)
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

	addr := os.Getenv("UECB_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, server.Handler()))
}
