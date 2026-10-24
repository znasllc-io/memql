package config

import "testing"

// memql#4133: the engine overlay's gRPC front door is api.memql.localhost.
// The legacy Cockpit front door was retired; make run --cluster local must
// land on the same install `make up` produces.
func TestDefaultLocalEndpointMatchesEngineOverlay(t *testing.T) {
	const want = "https://api.memql.localhost"
	if DefaultLocalEndpoint != want {
		t.Fatalf("DefaultLocalEndpoint = %q, want %q", DefaultLocalEndpoint, want)
	}
}

func TestWithLocalDefaultFillsBlankLocal(t *testing.T) {
	got := WithLocalDefault(ClusterConfig{Name: "local"})
	if got.Endpoint != DefaultLocalEndpoint {
		t.Fatalf("blank local endpoint = %q, want %q", got.Endpoint, DefaultLocalEndpoint)
	}
}

func TestWithLocalDefaultKeepsExplicitEndpoint(t *testing.T) {
	got := WithLocalDefault(ClusterConfig{
		Name:     "local",
		Endpoint: "https://api.example.test",
	})
	if got.Endpoint != "https://api.example.test" {
		t.Fatalf("explicit endpoint overwritten: %q", got.Endpoint)
	}
}

func TestWithLocalDefaultIgnoresOtherClusters(t *testing.T) {
	got := WithLocalDefault(ClusterConfig{Name: "staging"})
	if got.Endpoint != "" {
		t.Fatalf("non-local cluster got a default: %q", got.Endpoint)
	}
}
