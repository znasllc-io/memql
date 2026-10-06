package worker

import (
	"fmt"
	"strings"
)

// RepositoryScopes advertises exact repositories accepted by a named action.
// A present, empty list explicitly accepts all repositories. An absent action
// accepts none. It is the worker's report, never an operator-label override;
// execution must still enforce the worker's current policy.
type RepositoryScopes map[string][]string

// NormalizeRepository follows the repository identity used by worker policy.
func NormalizeRepository(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".git")
}

func (s RepositoryScopes) Accepts(action, repository string) bool {
	repositories, present := s[action]
	if !present || repositories == nil || NormalizeRepository(repository) == "" {
		return false
	}
	if len(repositories) == 0 {
		return true
	}
	for _, allowed := range repositories {
		if NormalizeRepository(allowed) == NormalizeRepository(repository) {
			return true
		}
	}
	return false
}

func (s RepositoryScopes) validate() error {
	if len(s) > maxCapabilityDescriptorActions {
		return fmt.Errorf("too many repository scopes")
	}
	for action, repositories := range s {
		if repositories == nil {
			return fmt.Errorf("null repository scope for %s", action)
		}
		if !capabilityNamePattern.MatchString(action) {
			return fmt.Errorf("invalid repository-scoped action %q", action)
		}
		if len(repositories) > 64 {
			return fmt.Errorf("too many repositories for %s", action)
		}
		for _, name := range repositories {
			name = NormalizeRepository(name)
			if name == "" || len(name) > 512 || strings.ContainsAny(name, "\r\n\t\x00") {
				return fmt.Errorf("invalid repository in scope for %s", action)
			}
		}
	}
	return nil
}

// RepositoryScopesFromMap validates the persisted descriptor before using its
// scope. Invalid or absent metadata never widens the set of accepted work.
func RepositoryScopesFromMap(value any) RepositoryScopes {
	m, _ := value.(map[string]any)
	d := capabilityDescriptorFromMap(m)
	if d == nil {
		return nil
	}
	return d.RepositoryScopes
}
