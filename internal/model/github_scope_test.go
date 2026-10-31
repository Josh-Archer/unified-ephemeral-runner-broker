package model

import "testing"

func TestGitHubScopeForRepository(t *testing.T) {
	owner := GitHubScope{Type: GitHubScopeOwner, Owner: "Example-Owner"}

	scope, err := owner.ForRepository("example-owner/questkit")
	if err != nil {
		t.Fatalf("ForRepository: %v", err)
	}
	if scope.Type != GitHubScopeRepository || scope.Owner != "Example-Owner" || scope.Repository != "questkit" {
		t.Fatalf("unexpected resolved scope: %+v", scope)
	}
	url, err := scope.TargetURL()
	if err != nil || url != "https://github.com/Example-Owner/questkit" {
		t.Fatalf("TargetURL = %q, %v", url, err)
	}

	for _, bad := range []string{"", "questkit", "other-owner/questkit", "example-owner/", "example-owner/a/b"} {
		if _, err := owner.ForRepository(bad); err == nil {
			t.Errorf("ForRepository(%q) should fail", bad)
		}
	}
	if _, err := owner.TargetURL(); err == nil {
		t.Error("owner scope has no single target URL")
	}

	fixed := GitHubScope{Type: GitHubScopeRepository, Owner: "example-owner", Repository: "home"}
	if scope, err := fixed.ForRepository("example-owner/questkit"); err != nil || scope != fixed {
		t.Fatalf("repository scope should pass through unchanged, got %+v, %v", scope, err)
	}
}

func TestAllocationStatusTargetRepository(t *testing.T) {
	status := AllocationStatus{Repository: "owner/from-oidc"}
	if got := status.TargetRepository(); got != "owner/from-oidc" {
		t.Fatalf("OIDC repository fallback = %q", got)
	}
	status.Request = &AllocationRequest{TargetRepository: "owner/from-webhook"}
	if got := status.TargetRepository(); got != "owner/from-webhook" {
		t.Fatalf("webhook repository = %q", got)
	}
}
