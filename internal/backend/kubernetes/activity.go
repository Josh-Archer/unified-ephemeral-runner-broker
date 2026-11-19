package kubernetes

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/github"
	"github.com/Josh-Archer/unified-ephemeral-runner-broker/internal/model"
)

// runnerLister is the optional GitHub directory used to tell an idle runner
// (busy=false) from one executing a job. *github.Client implements it.
type runnerLister interface {
	ListRunners(ctx context.Context, scope model.GitHubScope) ([]github.Runner, error)
}

// RunnerActivity reports GitHub's busy flag for allocations whose runner id
// is stored at provision time. Missing ids are omitted (unknown). A listing
// error fails the whole call so the broker does not reap on a partial view.
func (b *Backend) RunnerActivity(ctx context.Context, allocations []model.AllocationStatus) (map[string]bool, error) {
	lister, ok := b.registrar.(runnerLister)
	if b.registrar == nil || !ok {
		return nil, fmt.Errorf("backend %s has no GitHub runner listing", b.name)
	}

	type group struct {
		scope  model.GitHubScope
		allocs []model.AllocationStatus
	}
	byScope := map[string]*group{}
	var order []string
	for _, status := range allocations {
		if _, err := strconv.ParseInt(strings.TrimSpace(status.Metadata[metadataRunnerID]), 10, 64); err != nil {
			continue
		}
		scope, err := b.cfg.GitHub.Scope.ForRepository(status.TargetRepository())
		if err != nil {
			continue
		}
		key := scope.Type + "/" + scope.Organization + "/" + scope.Owner + "/" + scope.Repository
		item := byScope[key]
		if item == nil {
			item = &group{scope: scope}
			byScope[key] = item
			order = append(order, key)
		}
		item.allocs = append(item.allocs, status)
	}
	if len(order) == 0 {
		return map[string]bool{}, nil
	}

	out := make(map[string]bool, len(allocations))
	for _, key := range order {
		item := byScope[key]
		runners, err := lister.ListRunners(ctx, item.scope)
		if err != nil {
			return nil, fmt.Errorf("list runners for %s: %w", key, err)
		}
		byID := make(map[int64]github.Runner, len(runners))
		for _, runner := range runners {
			byID[runner.ID] = runner
		}
		for _, status := range item.allocs {
			id, _ := strconv.ParseInt(strings.TrimSpace(status.Metadata[metadataRunnerID]), 10, 64)
			runner, found := byID[id]
			if !found {
				continue
			}
			out[status.ID] = runner.Busy
		}
	}
	return out, nil
}
