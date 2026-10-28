package deploycontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type packageRollTransport func(*http.Request) (*http.Response, error)

func (f packageRollTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func packageDeploymentFixture() map[string]any {
	return map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": map[string]any{"name": "bff", "namespace": "memql", "uid": "original-uid", "resourceVersion": "10", "generation": 1}, "spec": map[string]any{"replicas": 2, "template": map[string]any{"metadata": map[string]any{"annotations": map[string]any{"existing": "preserved"}}, "spec": map[string]any{"containers": []any{map[string]any{"name": "bff", "image": "image@sha256:fixed"}}}}}, "status": map[string]any{"observedGeneration": 1, "replicas": 2, "updatedReplicas": 2, "readyReplicas": 2, "availableReplicas": 2}}
}
func packageRollAPI(t *testing.T, row map[string]any, patch func([]map[string]any) error) *ClusterAPI {
	t.Helper()
	return NewClusterAPIWith("https://kube.invalid", "test-only", &http.Client{Transport: packageRollTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/apis/apps/v1/namespaces/memql/deployments/bff" {
			t.Fatal("request escaped exact named target")
		}
		switch req.Method {
		case "GET":
			raw, _ := json.Marshal(row)
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
		case "PATCH":
			if req.Header.Get("Content-Type") != "application/json-patch+json" {
				t.Fatal("not a conditional JSON patch")
			}
			var ops []map[string]any
			if json.NewDecoder(req.Body).Decode(&ops) != nil {
				t.Fatal("invalid patch")
			}
			if err := patch(ops); err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		default:
			t.Fatal("unexpected effect verb")
			return nil, fmt.Errorf("refused")
		}
	})})
}
func TestPackageRolloutUsesStableConditionalPatchAndExactReadback(t *testing.T) {
	row := packageDeploymentFixture()
	patches := 0
	api := packageRollAPI(t, row, func(ops []map[string]any) error {
		patches++
		if len(ops) != 4 || ops[0]["path"] != "/metadata/uid" || ops[0]["value"] != "original-uid" || ops[1]["value"] != "10" || ops[2]["path"] != "/spec/template" || ops[3]["path"] != "/spec/template" {
			t.Fatalf("unbounded patch: %+v", ops)
		}
		spec := row["spec"].(map[string]any)
		before, _ := json.Marshal(spec["template"])
		test, _ := json.Marshal(ops[2]["value"])
		if string(before) != string(test) {
			t.Fatal("patch did not test complete old template")
		}
		spec["template"] = ops[3]["value"]
		row["metadata"].(map[string]any)["generation"] = 2
		return nil
	})
	intent, err := api.PreparePackageRollout(context.Background(), "memql", "bff", "deployment-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.ApplyPackageRollout(context.Background(), intent); err != nil {
		t.Fatal(err)
	}
	observed, err := api.ObservePackageRollout(context.Background(), intent)
	if err != nil || !observed.Applied || observed.Ready {
		t.Fatalf("old observedGeneration looked ready: %+v %v", observed, err)
	}
	row["status"].(map[string]any)["observedGeneration"] = 2
	observed, err = api.ObservePackageRollout(context.Background(), intent)
	if err != nil || !observed.Ready {
		t.Fatalf("exact ready target refused: %+v %v", observed, err)
	}
	annotations := row["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"].(map[string]any)
	if annotations["existing"] != "preserved" || annotations[packageRolloutAnnotation] != "deployment-1" || patches != 1 {
		t.Fatal("marker or unrelated annotation changed")
	}
	if err := api.ApplyPackageRollout(context.Background(), intent); err == nil || patches != 1 {
		t.Fatal("same intent patched twice")
	}
}
func TestPackageRolloutRefusesIdentityTemplateAndCapacityDrift(t *testing.T) {
	for _, kind := range []string{"uid", "replicas", "image", "annotation"} {
		t.Run(kind, func(t *testing.T) {
			row := packageDeploymentFixture()
			calls := 0
			api := packageRollAPI(t, row, func([]map[string]any) error { calls++; return nil })
			i, err := api.PreparePackageRollout(context.Background(), "memql", "bff", "deployment-2")
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "uid":
				row["metadata"].(map[string]any)["uid"] = "replacement"
			case "replicas":
				row["spec"].(map[string]any)["replicas"] = 1
			case "image":
				row["spec"].(map[string]any)["template"].(map[string]any)["spec"] = map[string]any{"containers": []any{map[string]any{"image": "different"}}}
			case "annotation":
				row["spec"].(map[string]any)["template"].(map[string]any)["metadata"].(map[string]any)["annotations"] = map[string]any{"foreign": "change"}
			}
			if _, err := api.ObservePackageRollout(context.Background(), i); err == nil {
				t.Fatal("drift was accepted")
			}
			if err := api.ApplyPackageRollout(context.Background(), i); err == nil || calls != 0 {
				t.Fatal("drift was patched")
			}
		})
	}
}
func TestPackageRolloutLostReplyKeepsSameMarkerForObservation(t *testing.T) {
	row := packageDeploymentFixture()
	calls := 0
	api := packageRollAPI(t, row, func(ops []map[string]any) error {
		calls++
		row["spec"].(map[string]any)["template"] = ops[3]["value"]
		row["metadata"].(map[string]any)["generation"] = 2
		row["status"].(map[string]any)["observedGeneration"] = 2
		return fmt.Errorf("lost reply")
	})
	i, err := api.PreparePackageRollout(context.Background(), "memql", "bff", "deployment-3")
	if err != nil {
		t.Fatal(err)
	}
	copy := i
	if err := api.ApplyPackageRollout(context.Background(), i); err == nil {
		t.Fatal("fault was not observed")
	}
	observed, err := api.ObservePackageRollout(context.Background(), i)
	if err != nil || !observed.Ready || !reflect.DeepEqual(copy, i) || calls != 1 {
		t.Fatal("lost reply was not reconciled from the fixed intent")
	}
}
func TestPackageRolloutRejectsPathsBeforeRequest(t *testing.T) {
	calls := 0
	api := NewClusterAPIWith("https://kube.invalid", "", &http.Client{Transport: packageRollTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, fmt.Errorf("unexpected") })})
	for _, name := range []string{"../secrets", "bff?watch=true", "bff/scale", ""} {
		if _, err := api.PreparePackageRollout(context.Background(), "memql", name, "selected"); err == nil {
			t.Fatal("path accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid path reached provider")
	}
}
