package model

import (
	"fmt"
	"strings"
)

const (
	GitHubScopeOrganization = "organization"
	GitHubScopeRepository   = "repository"
	// GitHubScopeOwner registers each runner on the repository that requested
	// it, for any repository owned by github.scope.owner. It lets one pool serve
	// every repository of a personal account, which has no organization-level
	// runners. The repository comes from the workflow_job webhook or the
	// caller's OIDC token; see ForRepository.
	GitHubScopeOwner = "owner"
)

func (s GitHubScope) Validate() error {
	scopeType := strings.TrimSpace(s.Type)
	switch scopeType {
	case GitHubScopeOrganization:
		if strings.TrimSpace(s.Organization) == "" {
			return fmt.Errorf("github.scope.organization is required when github.scope.type=%q", GitHubScopeOrganization)
		}
		return nil
	case GitHubScopeRepository:
		if strings.TrimSpace(s.Owner) == "" {
			return fmt.Errorf("github.scope.owner is required when github.scope.type=%q", GitHubScopeRepository)
		}
		if strings.TrimSpace(s.Repository) == "" {
			return fmt.Errorf("github.scope.repository is required when github.scope.type=%q", GitHubScopeRepository)
		}
		return nil
	case GitHubScopeOwner:
		if strings.TrimSpace(s.Owner) == "" {
			return fmt.Errorf("github.scope.owner is required when github.scope.type=%q", GitHubScopeOwner)
		}
		if strings.TrimSpace(s.Repository) != "" {
			return fmt.Errorf("github.scope.repository must be empty when github.scope.type=%q; the repository comes from each allocation", GitHubScopeOwner)
		}
		return nil
	default:
		return fmt.Errorf("unsupported github.scope.type %q", scopeType)
	}
}

// IsPerRepository reports whether runners register on the requesting
// repository rather than one configured target.
func (s GitHubScope) IsPerRepository() bool {
	return strings.TrimSpace(s.Type) == GitHubScopeOwner
}

// ForRepository returns the concrete scope a runner registers under for an
// allocation requested by fullName ("owner/repo"). Organization and repository
// scopes are returned unchanged. Owner scope becomes a repository scope for
// fullName, which must belong to the configured owner.
func (s GitHubScope) ForRepository(fullName string) (GitHubScope, error) {
	if !s.IsPerRepository() {
		return s, nil
	}
	if err := s.Validate(); err != nil {
		return GitHubScope{}, err
	}
	owner, repo, ok := strings.Cut(strings.TrimSpace(fullName), "/")
	owner, repo = strings.TrimSpace(owner), strings.TrimSpace(repo)
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return GitHubScope{}, fmt.Errorf("github.scope.type=%q needs the requesting repository as owner/repo, got %q", GitHubScopeOwner, fullName)
	}
	if !strings.EqualFold(owner, strings.TrimSpace(s.Owner)) {
		return GitHubScope{}, fmt.Errorf("repository %q is not owned by github.scope.owner %q", fullName, s.Owner)
	}
	return GitHubScope{
		Type:       GitHubScopeRepository,
		Owner:      strings.TrimSpace(s.Owner),
		Repository: repo,
	}, nil
}

// TargetRepository returns the repository ("owner/repo") an allocation's
// runner should register on: the workflow_job webhook's repository, else the
// allocating OIDC principal's repository.
func (s AllocationStatus) TargetRepository() string {
	if s.Request != nil {
		if repo := strings.TrimSpace(s.Request.TargetRepository); repo != "" {
			return repo
		}
	}
	return strings.TrimSpace(s.Repository)
}

func (s GitHubScope) TargetURL() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}

	switch strings.TrimSpace(s.Type) {
	case GitHubScopeOrganization:
		return fmt.Sprintf("https://github.com/%s", strings.TrimSpace(s.Organization)), nil
	case GitHubScopeRepository:
		return fmt.Sprintf("https://github.com/%s/%s", strings.TrimSpace(s.Owner), strings.TrimSpace(s.Repository)), nil
	case GitHubScopeOwner:
		return "", fmt.Errorf("github.scope.type=%q has no single target URL; resolve it with ForRepository first", GitHubScopeOwner)
	default:
		return "", fmt.Errorf("unsupported github.scope.type %q", strings.TrimSpace(s.Type))
	}
}

func (s GitHubScope) RunnerGroup(pool PoolName) string {
	prefix := strings.TrimSpace(s.RunnerGroupPrefix)
	if prefix == "" || strings.TrimSpace(s.Type) != GitHubScopeOrganization {
		return ""
	}
	return fmt.Sprintf("%s-%s", prefix, pool)
}
