package installation

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// These are the two resource orders observed from the installed Argo CRDs.
// Discovery order varies across reads without changing any resource contract.
func discoveryFixture() *receiverFixture {
	return &receiverFixture{reads: map[string]int{}, objects: map[string]map[string]any{
		"apis/argoproj.io/v1alpha1": {
			"apiVersion": "v1", "kind": "APIResourceList", "groupVersion": "argoproj.io/v1alpha1",
			"resources": []any{
				map[string]any{"name": "applications", "kind": "Application", "namespaced": true, "verbs": []any{"get", "list", "patch"}},
				map[string]any{"name": "applicationsets", "kind": "ApplicationSet", "namespaced": true, "verbs": []any{"get", "list"}},
				map[string]any{"name": "applicationsets/status", "kind": "ApplicationSet", "namespaced": true, "verbs": []any{"get", "patch"}},
				map[string]any{"name": "appprojects", "kind": "AppProject", "namespaced": true, "verbs": []any{"get", "list"}},
			},
		},
	}}
}

func reorderDiscovery(body map[string]any) {
	items := body["resources"].([]any)
	body["resources"] = []any{items[1], items[2], items[0], items[3]}
}

func TestReceiverDiscoveryResourceOrderIsStableAcrossReadsAndRecovery(t *testing.T) {
	const path = "apis/argoproj.io/v1alpha1"
	var digest, invariant string
	for _, phase := range []string{"unchanged", "during observation", "on recovery"} {
		t.Run(phase, func(t *testing.T) {
			f := discoveryFixture()
			if phase == "on recovery" {
				reorderDiscovery(f.objects[path])
			}
			if phase == "during observation" {
				f.before = func(p string, count int) {
					if p == path && count == 2 {
						reorderDiscovery(f.objects[p])
					}
				}
			}
			r := newReceiverReads(f)
			body, err := r.get(context.Background(), path)
			require.NoError(t, err)
			before, err := json.Marshal(body)
			require.NoError(t, err)
			got, err := r.finish(context.Background())
			require.NoError(t, err)
			stable, err := r.configurationInvariant()
			require.NoError(t, err)
			after, err := json.Marshal(body)
			require.NoError(t, err)
			require.Equal(t, string(before), string(after), "normalization must not mutate authenticated reads")
			if phase == "unchanged" {
				digest, invariant = got, stable
			} else {
				require.Equal(t, digest, got)
				require.Equal(t, invariant, stable)
			}
		})
	}
}

func TestReceiverDiscoveryStillRefusesResourceContractChanges(t *testing.T) {
	const path = "apis/argoproj.io/v1alpha1"
	for _, change := range []string{"removed", "added", "duplicate", "kind", "scope", "verbs", "group version", "extra field"} {
		t.Run(change, func(t *testing.T) {
			f := discoveryFixture()
			f.before = func(p string, count int) {
				if p != path || count != 2 {
					return
				}
				body := f.objects[p]
				items := body["resources"].([]any)
				first := items[0].(map[string]any)
				switch change {
				case "removed":
					body["resources"] = items[1:]
				case "added":
					body["resources"] = append(items, map[string]any{"name": "new", "kind": "New"})
				case "duplicate":
					body["resources"] = append(items, items[0])
				case "kind":
					first["kind"] = "Other"
				case "scope":
					first["namespaced"] = false
				case "verbs":
					first["verbs"] = []any{"get"}
				case "group version":
					body["groupVersion"] = "argoproj.io/v2"
				case "extra field":
					body["newConfiguration"] = true
				}
			}
			r := newReceiverReads(f)
			_, err := r.get(context.Background(), path)
			require.NoError(t, err)
			_, err = r.finish(context.Background())
			require.ErrorContains(t, err, "discovery changed")
		})
	}
}
