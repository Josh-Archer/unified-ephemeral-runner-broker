package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func webhookFullPool(pool *model.PoolConfig) {
	if pool.Name == model.PoolFull {
		pool.WebhookLabels = []string{"self-hosted", "uecb-full"}
	}
}

func queuedJob(runID, jobID int64) []github.WorkflowJob {
	return []github.WorkflowJob{{
		ID:     jobID,
		RunID:  runID,
		Status: "queued",
		Labels: []string{"self-hosted", "uecb-full"},
	}}
}

// Home (the GitHub scope repository) must be scanned first every cycle so a
// lost webhook is recovered on the next tick (~1m, well under 2m) even when
// the per-cycle budget cannot cover every other repository.
func TestReconcileQueuedGitHubJobsPrioritizesHomeUnderBudget(t *testing.T) {
	service := newServiceWithConfig(webhookFullPool)
	service.cfg.GitHub.Scope = model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "Josh-Archer", Repository: "home"}
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.runsRecent = map[string][]int64{}
	l.jobs = map[string][]github.WorkflowJob{}
	// Enough other repositories, each with several recent runs, that a
	// round-robin starting at the front of the GitHub listing would spend
	// the whole budget before reaching home at the end.
	for i := 0; i < 40; i++ {
		repo := fmt.Sprintf("other/r%02d", i)
		l.repos = append(l.repos, repo)
		l.runsRecent[repo] = manyRuns(10000+i*10, 5)
	}
	const homeRepo = "Josh-Archer/home"
	l.repos = append(l.repos, homeRepo)
	l.runsRecent[homeRepo] = []int64{77}
	l.jobs["77"] = queuedJob(77, 501)
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})

	got, err := server.ReconcileQueuedGitHubJobs(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got != 1 {
		t.Fatalf("allocated=%d, want home's queued job served in this cycle", got)
	}
	if _, ok := service.findByWorkflowJobID(501); !ok {
		t.Fatal("home job 501 was not allocated")
	}
	if len(l.runLists) == 0 || !strings.EqualFold(l.runLists[0].repo, homeRepo) {
		t.Fatalf("first run listing = %+v, want %s first", l.runLists, homeRepo)
	}
}

func TestReconcileQueuedGitHubJobsAllowlistSkipsRepositoryListing(t *testing.T) {
	service := newServiceWithConfig(webhookFullPool)
	service.cfg.GitHub.Scope = model.GitHubScope{Type: model.GitHubScopeOwner, Owner: "Josh-Archer", Repository: "home"}
	service.cfg.Broker.GitHubWebhook.ReconcileRepositories = []string{"Josh-Archer/tools", "Josh-Archer/docs"}
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.repos = []string{"other/one", "other/two", "Josh-Archer/tools", "Josh-Archer/docs", "Josh-Archer/home"}
	l.runsRecent = map[string][]int64{
		"Josh-Archer/tools":       {11},
		"Josh-Archer/docs":        {12},
		"Josh-Archer/home": {13},
		"other/one":        {14},
	}
	l.jobs = map[string][]github.WorkflowJob{
		"11": queuedJob(11, 611),
		"12": queuedJob(12, 612),
		"13": queuedJob(13, 613),
		"14": queuedJob(14, 614),
	}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})

	got, err := server.ReconcileQueuedGitHubJobs(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got != 3 {
		t.Fatalf("allocated=%d, want the two allowlisted repos plus home", got)
	}
	if l.repoLists != 0 {
		t.Fatalf("ListRepositories called %d times; an allowlist must not list the owner scope", l.repoLists)
	}
	if _, ok := service.findByWorkflowJobID(614); ok {
		t.Fatal("a repository outside the allowlist must not be scanned")
	}
	seen := map[string]int{}
	for _, c := range l.runLists {
		if c.window == reconcileRecentWindow {
			seen[strings.ToLower(c.repo)]++
		}
	}
	for _, repo := range []string{"josh-archer/home", "josh-archer/tools", "josh-archer/docs"} {
		if seen[repo] == 0 {
			t.Fatalf("recent scan missed %s: %+v", repo, l.runLists)
		}
	}
	if seen["other/one"] != 0 {
		t.Fatalf("recent scan visited a repo outside the allowlist: %+v", l.runLists)
	}
	if len(l.runLists) == 0 || !strings.EqualFold(l.runLists[0].repo, "Josh-Archer/home") {
		t.Fatalf("home must still be scanned first under an allowlist, got %+v", l.runLists)
	}
}

func TestReconcileQueuedGitHubJobsPrefersConfiguredRepositories(t *testing.T) {
	service := newServiceWithConfig(func(pool *model.PoolConfig) {
		if pool.Name == model.PoolFull {
			pool.WebhookLabels = []string{"self-hosted", "uecb-full"}
			pool.WebhookRepositories = []string{"my-org/configured"}
		}
	})
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.repos = []string{"my-org/other", "my-org/configured"}
	l.runsRecent = map[string][]int64{
		"my-org/other":      {21},
		"my-org/configured": {22},
	}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})

	if _, err := server.ReconcileQueuedGitHubJobs(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(l.runLists) == 0 || l.runLists[0].repo != "my-org/configured" {
		t.Fatalf("first listing = %+v, want configured repo first", l.runLists)
	}
}
