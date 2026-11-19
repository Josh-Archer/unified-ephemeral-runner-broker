package kubernetes

import (
	"context"
	"errors"
	"testing"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

type listingRegistrar struct {
	fakeRegistrar
	byRepo map[string][]github.Runner
	err    error
	calls  []model.GitHubScope
}

func (l *listingRegistrar) ListRunners(_ context.Context, scope model.GitHubScope) ([]github.Runner, error) {
	l.calls = append(l.calls, scope)
	if l.err != nil {
		return nil, l.err
	}
	if l.byRepo == nil {
		return nil, nil
	}
	return l.byRepo[scope.Repository], nil
}

func activityStatus(id, repo, runnerID string) model.AllocationStatus {
	status := model.AllocationStatus{ID: id, Metadata: map[string]string{}}
	if runnerID != "" {
		status.Metadata[metadataRunnerID] = runnerID
	}
	if repo != "" {
		status.Request = &model.AllocationRequest{TargetRepository: repo}
	}
	return status
}

func TestRunnerActivityMatchesBusyByRunnerID(t *testing.T) {
	cfg := newConfig(t)
	lister := &listingRegistrar{byRepo: map[string][]github.Runner{
		"home": {
			{ID: 42, Busy: false},
			{ID: 7, Busy: true},
		},
		"other": {
			{ID: 5, Busy: true},
		},
	}}
	backend := New(cfg, nil, lister)
	out, err := backend.RunnerActivity(context.Background(), []model.AllocationStatus{
		activityStatus("idle", "example-owner/home", "42"),
		activityStatus("busy", "example-owner/home", "7"),
		activityStatus("missing", "example-owner/home", "99"),
		activityStatus("noid", "example-owner/home", ""),
		activityStatus("other", "example-owner/other", "5"),
		activityStatus("foreign", "someone-else/repo", "1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("activity=%v, want idle/busy/other only", out)
	}
	if out["idle"] || !out["busy"] || !out["other"] {
		t.Fatalf("activity=%v", out)
	}
	if _, ok := out["missing"]; ok {
		t.Fatal("runner missing from GitHub must stay unknown")
	}
	if len(lister.calls) != 2 {
		t.Fatalf("list calls=%d, want one per repository", len(lister.calls))
	}
}

func TestRunnerActivityRequiresLister(t *testing.T) {
	cfg := newConfig(t)
	backend := New(cfg, nil, &fakeRegistrar{})
	_, err := backend.RunnerActivity(context.Background(), []model.AllocationStatus{
		activityStatus("a", "example-owner/home", "1"),
	})
	if err == nil {
		t.Fatal("expected error when the registrar cannot list runners")
	}
}

func TestRunnerActivityListErrorFailsClosed(t *testing.T) {
	cfg := newConfig(t)
	lister := &listingRegistrar{err: errors.New("github down")}
	backend := New(cfg, nil, lister)
	_, err := backend.RunnerActivity(context.Background(), []model.AllocationStatus{
		activityStatus("a", "example-owner/home", "1"),
	})
	if err == nil {
		t.Fatal("expected list error")
	}
}

func TestRunnerActivitySkipsEmptyIDs(t *testing.T) {
	cfg := newConfig(t)
	lister := &listingRegistrar{}
	backend := New(cfg, nil, lister)
	out, err := backend.RunnerActivity(context.Background(), []model.AllocationStatus{
		activityStatus("a", "example-owner/home", ""),
		{ID: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 || len(lister.calls) != 0 {
		t.Fatalf("out=%v calls=%d", out, len(lister.calls))
	}
}
