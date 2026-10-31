package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
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
	kubernetesbackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/kubernetes"
	lambdabackend "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/backend/lambda"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/capacity"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/config"
	githubapi "github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/runtime"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/store"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/telemetry"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/tier"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/webhook"
)

func main() {
	cfg, err := config.Load(os.Getenv("UECB_CONFIG_PATH"))
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	shutdownTracing, err := telemetry.Setup(context.Background())
	if err != nil {
		log.Fatalf("configure tracing: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(ctx); err != nil {
			log.Printf("otel shutdown: %v", err)
		}
	}()

	if err := config.ValidateReplicaSafety(cfg, expectedReplicasFromEnv()); err != nil {
		log.Fatalf("replica safety: %v", err)
	}

	secretReader, err := runtime.NewSecretReaderFromEnv()
	if err != nil {
		log.Fatalf("configure kubernetes secret reader: %v", err)
	}

	kubeAPI, err := runtime.NewKubernetesAPIFromEnv()
	if err != nil {
		log.Fatalf("configure kubernetes api client: %v", err)
	}
	var runnerPods kubernetesbackend.API
	if kubeAPI != nil {
		runnerPods = kubeAPI
	}

	// The broker's own GitHub client: registers kubernetes-backend runners and
	// checks webhook jobs. Its token never leaves the broker.
	githubClient := githubapi.NewClient(githubapi.TokenFromSecret(secretReader, cfg.GitHub.Auth.SecretRef, ""), nil)

	registry, desktopBackend := buildRegistry(cfg, secretReader, runnerPods, githubClient)
	healthChecker, err := runtime.NewSecretRefCheckerFromEnv(cfg)
	if err != nil {
		log.Fatalf("configure runtime dependencies: %v", err)
	}

	service := api.NewService(cfg, registry, healthChecker.Check)
	wireDesktopActiveCounter(desktopBackend, service.Store())
	if cfg.Broker.Webhooks.Enabled {
		service.SetLifecycleNotifier(webhook.New(cfg.Broker.Webhooks, secretReader, nil))
		log.Printf("allocation lifecycle webhooks enabled endpoints=%d", len(cfg.Broker.Webhooks.Endpoints))
	}
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

	var capacityManager *capacity.Manager
	if cfg.Broker.LiveCapacity.Enabled {
		capacityManager = capacity.NewManager()
		service.SetCapacityManager(capacityManager)
		reporter := capacity.RegistryReporter{Registry: registry}
		probeTimeout := cfg.Broker.LiveCapacity.ProbeTimeout
		if probeTimeout <= 0 {
			probeTimeout = 2 * time.Second
		}
		if cfg.Broker.LiveCapacity.RefreshOnStartup {
			ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			capacity.Refresh(ctx, capacityManager, reporter, cfg, time.Now().UTC())
			cancel()
		}
		capacity.StartRefreshLoop(context.Background(), capacityManager, reporter, cfg, cfg.Broker.LiveCapacity.RefreshInterval)
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

	if webhookPoolsConfigured(cfg) {
		server.SetGitHubWebhook(api.GitHubWebhookOptions{
			WorkflowRun:   githubClient.GetWorkflowRun,
			RejectForks:   cfg.GitHub.Scope.IsPerRepository() && !cfg.Broker.GitHubWebhook.AllowForkPullRequests,
			AllowedActors: cfg.Broker.GitHubWebhook.AllowedActors,
			Secret:        api.GitHubWebhookSecretFunc(cfg.Broker.GitHubWebhook, secretReader.ReadSecret),
			Inbox:         store.AsWebhookInbox(service.Store()),
			IsLeader: func(ctx context.Context) (bool, error) {
				if !useLeaderElection {
					return true, nil
				}
				return elector.TryAcquireLeadership(ctx, store.LeaderLeaseName, identity, leaseTTL)
			},
		})
		log.Printf("github workflow_job webhook enabled at /webhooks/github")
	}

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

	if webhookPoolsConfigured(cfg) {
		startBackground("github-webhook-inbox", 5*time.Second, func(time.Time) {
			if _, err := server.DrainGitHubWebhookInbox(context.Background()); err != nil {
				log.Printf("github webhook inbox drain: %v", err)
			}
		})
	}

	if tierManager != nil {
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for now := range ticker.C {
				tierManager.MarkStale(cfg.Broker.TierRouting.StaleAfter, now)
			}
		}()
	}

	if capacityManager != nil {
		go func() {
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for now := range ticker.C {
				capacityManager.MarkStale(cfg.Broker.LiveCapacity.StaleAfter, now)
			}
		}()
	}

	addr := os.Getenv("UECB_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// /webhooks/github can be exposed publicly; bound slow header reads.
	httpServer := &http.Server{Addr: addr, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("listening on %s stateStore=%s ha=%v", addr, strings.TrimSpace(cfg.Broker.StateStore.Type), useLeaderElection)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("http shutdown: %v", err)
	}
}

func webhookPoolsConfigured(cfg model.BrokerConfig) bool {
	for _, pool := range cfg.Pools {
		if len(pool.WebhookLabels) > 0 {
			return true
		}
	}
	return false
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

func buildRegistry(cfg model.BrokerConfig, secretReader runtime.SecretReader, runnerPods kubernetesbackend.API, registrar kubernetesbackend.Registrar) (*backend.Registry, *desktopbackend.Backend) {
	desktopBackend := desktopbackend.New(cfg)
	// Preserve a logical ARC backend name when migrating its physical driver.
	// Existing allocation clients can keep pinning arc without workflow edits.
	var arcTarget backend.Backend = arcbackend.New(cfg, secretReader)
	for _, pool := range cfg.Pools {
		if pool.Backends[model.BackendARC].Driver == "kubernetes" {
			arcTarget = kubernetesbackend.NewNamed(cfg, runnerPods, registrar, model.BackendARC)
			break
		}
	}
	entries := []backend.Backend{
		arcTarget,
		codebuildbackend.New(cfg, secretReader),
		lambdabackend.New(cfg, secretReader),
		cloudbackend.New(cfg, secretReader),
		azurebackend.New(cfg, secretReader),
		azurevmbackend.New(cfg),
		desktopBackend,
		kubernetesbackend.New(cfg, runnerPods, registrar),
		ec2backend.New(cfg, secretReader),
		gcebackend.New(cfg, secretReader),
	}
	seen := map[model.BackendName]bool{model.BackendARC: true}
	for _, pool := range cfg.Pools {
		for name, target := range pool.Backends {
			if target.Driver == "kubernetes" && !seen[name] {
				entries = append(entries, kubernetesbackend.NewNamed(cfg, runnerPods, registrar, name))
				seen[name] = true
			}
		}
	}
	registry := backend.NewRegistry(entries...)
	return registry, desktopBackend
}

func wireDesktopActiveCounter(desktop *desktopbackend.Backend, s store.Store) {
	if desktop != nil && s != nil {
		desktop.WithActiveCounter(s)
	}
}
