package http

import "testing"

// TestResolveWorkerDialEndpoint_Precedence pins the resolution order
// resolveWorkerDialEndpoint documents, in BOTH directions: with a stored
// URL on the pairing row, and without one (memql#3434).
//
// The regression this guards: resolveWorkerDialEndpoint used to reach the
// pairing row's origin through identity.DeriveGRPCEndpoint, which consults
// MEMQL_DISCOVERY_GRPC_ENDPOINT FIRST. That variable is set in every
// deployed environment (deploy/k8s/base/identity.yaml declares it, the
// local overlay patches it, and the sealed genesis envelope carries one
// too), so tier 2 never actually read the stored URL -- every worker was
// handed the one cluster-wide advertised endpoint no matter which origin
// it paired against.
func TestResolveWorkerDialEndpoint_Precedence(t *testing.T) {
	const discovery = "cockpit.local.znas.io:443"

	t.Run("stored URL wins over the cluster-wide discovery endpoint", func(t *testing.T) {
		t.Setenv("MEMQL_WORKER_DIAL_ENDPOINT", "")
		t.Setenv("MEMQL_DISCOVERY_GRPC_ENDPOINT", discovery)

		got := resolveWorkerDialEndpoint("https://app.local.znas.io")
		if want := "app.local.znas.io:443"; got != want {
			t.Errorf("resolveWorkerDialEndpoint = %q, want %q (the pairing row's own origin, not the advertised %q)", got, want, discovery)
		}
	})

	t.Run("discovery endpoint is the fallback when no stored URL", func(t *testing.T) {
		t.Setenv("MEMQL_WORKER_DIAL_ENDPOINT", "")
		t.Setenv("MEMQL_DISCOVERY_GRPC_ENDPOINT", discovery)

		got := resolveWorkerDialEndpoint("")
		if got != discovery {
			t.Errorf("resolveWorkerDialEndpoint(\"\") = %q, want %q", got, discovery)
		}
	})

	t.Run("worker override outranks both", func(t *testing.T) {
		t.Setenv("MEMQL_WORKER_DIAL_ENDPOINT", "agent.acme.com:443")
		t.Setenv("MEMQL_DISCOVERY_GRPC_ENDPOINT", discovery)

		got := resolveWorkerDialEndpoint("https://app.local.znas.io")
		if want := "agent.acme.com:443"; got != want {
			t.Errorf("resolveWorkerDialEndpoint = %q, want %q", got, want)
		}
	})

	t.Run("stored URL is mapped, never republished as a URL", func(t *testing.T) {
		t.Setenv("MEMQL_WORKER_DIAL_ENDPOINT", "")
		t.Setenv("MEMQL_DISCOVERY_GRPC_ENDPOINT", "")

		// The origin's OWN port is where the SPA serves HTTP, never where a
		// client dials gRPC -- the scheme names the port, the port is dropped.
		got := resolveWorkerDialEndpoint("http://localhost:3000")
		if want := "localhost:50050"; got != want {
			t.Errorf("resolveWorkerDialEndpoint = %q, want %q", got, want)
		}
	})

	t.Run("nothing configured and no stored URL yields nothing", func(t *testing.T) {
		t.Setenv("MEMQL_WORKER_DIAL_ENDPOINT", "")
		t.Setenv("MEMQL_DISCOVERY_GRPC_ENDPOINT", "")

		if got := resolveWorkerDialEndpoint(""); got != "" {
			t.Errorf("resolveWorkerDialEndpoint(\"\") = %q, want \"\"", got)
		}
	})
}
