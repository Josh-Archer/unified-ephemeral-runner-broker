package model

import (
	"fmt"
	"strings"
)

const (
	GitHubScopeOrganization = "organization"
	GitHubScopeRepository   = "repository"
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
	default:
		return fmt.Errorf("unsupported github.scope.type %q", scopeType)
	}
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
