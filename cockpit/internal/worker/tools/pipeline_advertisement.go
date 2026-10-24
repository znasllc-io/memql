package tools

import (
	"encoding/json"
	"sort"
)

// RepositoryScopes is the public routing part of this machine's live policy.
// A present empty list explicitly allows all repositories; absence grants none.
// The command handler still rechecks Check immediately before execution.
func (pp PipelinesPolicy) RepositoryScopes() map[string][]string {
	if !pp.Allow {
		return nil
	}
	repositories := make([]string, 0, len(pp.Repos))
	for _, repository := range pp.Repos {
		repositories = append(repositories, normalRepository(repository))
	}
	sort.Strings(repositories)
	return map[string][]string{"workerHost.pipeline_step": repositories}
}

// AdvertisementFingerprint excludes paths and other private machine settings.
func (pp PipelinesPolicy) AdvertisementFingerprint() string {
	if !pp.Allow {
		return ""
	}
	value, _ := json.Marshal(pp.RepositoryScopes())
	return string(value)
}
