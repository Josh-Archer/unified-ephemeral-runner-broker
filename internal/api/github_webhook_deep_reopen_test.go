package api

import (
	"testing"
	"time"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

func deepListings(l countingLister) (lists, allAges int) {
	for _, c := range l.runLists {
		if c.window == reconcileRunWindow {
			lists++
			if c.allAges {
				allAges++
			}
		}
	}
	return lists, allAges
}

// home#4206: after a state reset the deep scan must reopen without waiting
// for its interval and list every queued run regardless of age.
func TestReconcileDeepScanReopensAllAgesAfterStateReset(t *testing.T) {
	service := newServiceWithConfig(nil)
	now := time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.repos = []string{"my-org/home"}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})

	if err := service.store.Save(model.AllocationStatus{ID: "live", State: model.StateReady, Pool: model.PoolFull}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	reconcileCycle(t, server, l)
	if lists, all := deepListings(l); lists != 1 || all != 0 {
		t.Fatalf("non-empty state: deep lists=%d allAges=%d, want 1 windowed pass", lists, all)
	}

	// State reset one minute later, well inside reconcileDeepInterval.
	if err := service.store.Delete("live"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	now = now.Add(time.Minute)
	l.runLists = nil
	reconcileCycle(t, server, l)
	if lists, all := deepListings(l); lists != 1 || all != 1 {
		t.Fatalf("after reset: deep lists=%d allAges=%d, want an immediate all-ages pass", lists, all)
	}
}

// An empty store (fresh start) scans every queued run regardless of age.
func TestReconcileDeepScanEmptyStateListsAllAges(t *testing.T) {
	service := newServiceWithConfig(nil)
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.repos = []string{"my-org/home"}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})
	reconcileCycle(t, server, l)
	if _, all := deepListings(l); all != 1 {
		t.Fatalf("empty state deep pass allAges listings=%d, want 1: %+v", all, l.runLists)
	}
}

// A deep pass that stays open past reconcileDeepMaxOpen is restarted from a
// fresh repository count instead of staying parked forever.
func TestReconcileDeepScanRestartsStalledPass(t *testing.T) {
	service := newServiceWithConfig(nil)
	now := time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	server := newTestServer(t, service)
	l := newCountingLister(service.now)
	l.repos = []string{"my-org/a", "my-org/b"}
	server.SetGitHubWebhook(GitHubWebhookOptions{WorkflowJobs: l})
	st := &server.githubWebhook.reconcile
	st.deepOpen = true
	st.deepOpenedAt = now.Add(-2 * reconcileDeepMaxOpen)
	st.lastDeep = now
	st.deep = repoScanner{cursor: 44, remaining: 0} // parked: nothing left to list
	reconcileCycle(t, server, l)
	if lists, _ := deepListings(l); lists != 2 {
		t.Fatalf("stalled deep pass listed %d repos, want a restarted pass over 2", lists)
	}
}
