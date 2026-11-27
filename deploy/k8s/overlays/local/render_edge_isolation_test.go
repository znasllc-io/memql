package local

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/frontdoor"
	"gopkg.in/yaml.v3"
)

func isolationObjects(t *testing.T, rendered string) []map[string]any {
	t.Helper()
	var objects []map[string]any
	decoder := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var object map[string]any
		err := decoder.Decode(&object)
		if errors.Is(err, io.EOF) {
			return objects
		}
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, object)
	}
}

func isolationMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func TestLocalEdgeHTTPOnlyAcceptsTheMeasuredIngress(t *testing.T) {
	objects := isolationObjects(t, render(t))
	var labels map[string]any
	for _, object := range objects {
		metadata := isolationMap(object["metadata"])
		if object["kind"] == "Deployment" && metadata["name"] == "edge" && metadata["namespace"] == "memql" {
			labels = isolationMap(isolationMap(isolationMap(isolationMap(object["spec"])["template"])["metadata"])["labels"])
		}
	}
	if labels["app.kubernetes.io/name"] != "edge" {
		t.Fatal("edge pod labels missing; isolation would select no real workload")
	}
	// NetworkPolicies combine additively. A second policy selecting edge could
	// reopen HTTP even while the narrow policy still passes an exact comparison.
	matched := 0
	for _, object := range objects {
		metadata := isolationMap(object["metadata"])
		if object["kind"] != "NetworkPolicy" || metadata["namespace"] != "memql" {
			continue
		}
		spec := isolationMap(object["spec"])
		selector := isolationMap(spec["podSelector"])
		if expressions, ok := selector["matchExpressions"]; ok && expressions != nil {
			t.Fatalf("review selector expressions for policy %v before claiming edge isolation", metadata["name"])
		}
		selectsEdge := true
		for key, value := range isolationMap(selector["matchLabels"]) {
			if labels[key] != value {
				selectsEdge = false
			}
		}
		if !selectsEdge {
			continue
		}
		matched++
		if metadata["name"] != "edge-http-from-local-ingress" {
			t.Fatalf("additional policy %v selects edge; audit its additive ingress permissions", metadata["name"])
		}
		var want map[string]any
		if err := yaml.Unmarshal([]byte(`
podSelector:
  matchLabels: {app.kubernetes.io/name: edge}
policyTypes: [Ingress]
ingress:
  - from:
      - namespaceSelector:
          matchLabels: {kubernetes.io/metadata.name: kube-system}
        podSelector:
          matchLabels:
            app.kubernetes.io/name: traefik
            app.kubernetes.io/instance: traefik-kube-system
    ports: [{protocol: TCP, port: 8085}]
  - ports: [{protocol: TCP, port: 50051}, {protocol: TCP, port: 50062}]
`), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(spec, want) {
			t.Fatalf("edge policy must allow only the measured namespace AND pod on HTTP, preserve mesh ports, and leave egress unchanged: %#v", spec)
		}
	}
	if matched != 1 {
		t.Fatalf("rendered %d edge policies, want exactly one enforced ingress boundary", matched)
	}
}

func TestLocalPublicAPICannotRouteAroundCallsEdgeLimits(t *testing.T) {
	apiHost := frontdoor.RoleHost(frontdoor.RoleAPI, "memql.localhost")
	apiRules, catchAll := 0, false
	for _, object := range isolationObjects(t, render(t)) {
		if object["kind"] != "Ingress" {
			continue
		}
		rules, _ := isolationMap(object["spec"])["rules"].([]any)
		for _, rawRule := range rules {
			rule := isolationMap(rawRule)
			if rule["host"] != apiHost {
				continue
			}
			apiRules++
			paths, _ := isolationMap(rule["http"])["paths"].([]any)
			for _, rawPath := range paths {
				path := isolationMap(rawPath)
				prefix, _ := path["path"].(string)
				service := isolationMap(isolationMap(path["backend"])["service"])
				port := isolationMap(service["port"])["number"]
				if prefix == "/" {
					catchAll = true
					if service["name"] != "bff" || port != 50051 {
						t.Fatal("public API catch-all must remain gRPC, never the native HTTP listener")
					}
				}
				if service["name"] != "bff-http" && !(service["name"] == "bff" && port == 8085) {
					continue
				}
				for _, internal := range []string{"/calls", "/memql/calls", "/_memql/calls"} {
					// Refuse both an ancestor prefix and a newly published exact
					// descendant. Either would bypass the edge's visitor budgets.
					trimmed := strings.TrimSuffix(prefix, "/")
					if trimmed == "" || trimmed == internal || strings.HasPrefix(internal, trimmed+"/") || strings.HasPrefix(trimmed, internal+"/") {
						t.Fatalf("public API HTTP path %q exposes native Calls prefix %q", prefix, internal)
					}
				}
			}
		}
	}
	if apiRules == 0 || !catchAll {
		t.Fatal("public API ingress/catch-all absent; bypass check would be vacuous")
	}
}
