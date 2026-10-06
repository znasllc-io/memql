package worker

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRepositoryScopesFailClosedAndRoundTrip(t *testing.T) {
	const action = "workerHost.pipeline_step"
	for _, tc := range []struct {
		name   string
		scopes RepositoryScopes
		repo   string
		want   bool
	}{
		{"unknown", nil, "o/r", false},
		{"explicit all", RepositoryScopes{action: {}}, "o/r", true},
		{"no repository", RepositoryScopes{action: {}}, "", false},
		{"canonical exact match", RepositoryScopes{action: {" O/R.GIT "}}, "o/r", true},
		{"other repository", RepositoryScopes{action: {"o/a"}}, "o/b", false},
		{"no prefix grant", RepositoryScopes{action: {"o/r"}}, "o/r-extra", false},
		{"different action", RepositoryScopes{"workerHost.exec": {}}, "o/r", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := CapabilityDescriptor{Platform: "linux", DisplayServer: "none", SchemaVersion: 1, RepositoryScopes: tc.scopes}
			raw, _ := json.Marshal(d.AsMap())
			var persisted map[string]any
			if err := json.Unmarshal(raw, &persisted); err != nil {
				t.Fatal(err)
			}
			if got := RepositoryScopesFromMap(persisted).Accepts(action, tc.repo); got != tc.want {
				t.Fatalf("persisted scope accepts %q = %v, want %v", tc.repo, got, tc.want)
			}
		})
	}
	for _, raw := range []string{
		`{"platform":"linux","displayServer":"none","schemaVersion":1,"repositoryScopes":{"workerHost.pipeline_step":null}}`,
		`{"platform":"linux","displayServer":"none","schemaVersion":1,"repositoryScopes":{"workerHost.pipeline_step":[""]}}`,
		`{"platform":"linux","displayServer":"none","schemaVersion":1,"repositoryScopes":{"workerHost.pipeline_step":"*"}}`,
		`{"platform":"linux","displayServer":"none","schemaVersion":1,"repositoryScopes":{"bad action":[]}}`,
	} {
		if _, err := ParseCapabilityDescriptor(raw); err == nil {
			t.Fatalf("accepted malformed scope: %s", raw)
		}
	}
	oversized := RepositoryScopes{action: {strings.Repeat("x", 513)}}
	if oversized.validate() == nil {
		t.Fatal("unbounded repository")
	}
}
