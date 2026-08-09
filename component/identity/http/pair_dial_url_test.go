package http

import (
	"strings"
	"testing"
)

// TestResolveWorkerDialEndpoint_StatesTransportSecurity pins the FORM of the
// value the pairing reply carries (memql#3437), where the sibling
// TestResolveWorkerDialEndpoint_Precedence pins which tier produces it.
//
// The defect: the resolver returned a bare `host:port`, and the consumer --
// sdk/go/worker.ParseClusterURL, which the cockpit calls with exactly this
// string (cmd/memql-cockpit/internal/worker/connect.go) -- documents a bare
// value as useTLS=false. So a reply of "cockpit.local.znas.io:443" told the
// worker to dial a TLS port IN PLAINTEXT, putting its `mql_wkr_` bearer token
// on the wire in the clear before the handshake failed.
//
// The server was never guessing here: every input carries the answer. The
// pairing row's clusterUrl is a full origin, and an operator-configured
// endpoint carries whatever scheme the operator wrote. The reply now STATES
// the transport instead of discarding it.
//
// The dial target itself does not move -- strip the scheme from any value
// below and the pre-fix bare form comes back byte for byte. Only the silence
// about TLS is gone.
func TestResolveWorkerDialEndpoint_StatesTransportSecurity(t *testing.T) {
	t.Run("a TLS pairing origin is reported as TLS", func(t *testing.T) {
		t.Setenv("MEMQL_WORKER_DIAL_ENDPOINT", "")
		t.Setenv("MEMQL_DISCOVERY_GRPC_ENDPOINT", "")

		got := resolveWorkerDialEndpoint("https://app.local.znas.io")
		if want := "https://app.local.znas.io:443"; got != want {
			t.Errorf("resolveWorkerDialEndpoint = %q, want %q", got, want)
		}
		if !strings.HasPrefix(got, "https://") {
			t.Errorf("resolveWorkerDialEndpoint = %q -- a bare :443 address is read as PLAINTEXT by "+
				"sdk/go/worker.ParseClusterURL, so the reply has to say https", got)
		}
	})

	t.Run("a plaintext pairing origin is reported as plaintext", func(t *testing.T) {
		t.Setenv("MEMQL_WORKER_DIAL_ENDPOINT", "")
		t.Setenv("MEMQL_DISCOVERY_GRPC_ENDPOINT", "")

		got := resolveWorkerDialEndpoint("http://localhost:3000")
		if want := "http://localhost:50050"; got != want {
			t.Errorf("resolveWorkerDialEndpoint = %q, want %q", got, want)
		}
	})

	t.Run("an operator override's own scheme is honoured", func(t *testing.T) {
		t.Setenv("MEMQL_DISCOVERY_GRPC_ENDPOINT", "")

		for _, tc := range []struct{ configured, want string }{
			{"https://agent.acme.com", "https://agent.acme.com:443"},
			{"https://agent.acme.com:8443", "https://agent.acme.com:8443"},
			{"http://agent.internal:50050", "http://agent.internal:50050"},
			// No scheme and no port: the resolver already infers the PORT
			// here (front door vs dev loopback), so stating the transport
			// that inference implies adds no new guess.
			{"agent.acme.com", "https://agent.acme.com:443"},
			{"localhost", "http://localhost:50050"},
		} {
			t.Setenv("MEMQL_WORKER_DIAL_ENDPOINT", tc.configured)
			if got := resolveWorkerDialEndpoint("https://app.local.znas.io"); got != tc.want {
				t.Errorf("MEMQL_WORKER_DIAL_ENDPOINT=%q: resolveWorkerDialEndpoint = %q, want %q",
					tc.configured, got, tc.want)
			}
		}
	})

	t.Run("a scheme-less override WITH a port stays bare rather than inventing one", func(t *testing.T) {
		// Nothing in `host:443` says whether that listener speaks TLS, and the
		// port is not evidence: a TLS listener on 8443 is ordinary, so reading
		// 443 as https would be exactly as much of a guess as reading 8443 as
		// http. The server refuses to invent the fact it was not told; the
		// handler logs a WARN so the silence is audible, and the documented
		// remedy is to write the scheme.
		t.Setenv("MEMQL_DISCOVERY_GRPC_ENDPOINT", "")
		t.Setenv("MEMQL_WORKER_DIAL_ENDPOINT", "agent.acme.com:443")

		got := resolveWorkerDialEndpoint("https://app.local.znas.io")
		if want := "agent.acme.com:443"; got != want {
			t.Errorf("resolveWorkerDialEndpoint = %q, want %q (verbatim -- the operator said a port, not a transport)", got, want)
		}
	})

	t.Run("the advertised discovery endpoint keeps its own spelling", func(t *testing.T) {
		t.Setenv("MEMQL_WORKER_DIAL_ENDPOINT", "")

		// Bare: the deployed value (deploy/k8s/base/identity.yaml), which
		// memql#3399's deploy gate REQUIRES to be a bare dial address. Same
		// ambiguity, same refusal to invent.
		t.Setenv("MEMQL_DISCOVERY_GRPC_ENDPOINT", "cockpit.local.znas.io:443")
		if got, want := resolveWorkerDialEndpoint(""), "cockpit.local.znas.io:443"; got != want {
			t.Errorf("resolveWorkerDialEndpoint(\"\") = %q, want %q", got, want)
		}

		// Scheme-ful: an operator who states the transport is believed.
		t.Setenv("MEMQL_DISCOVERY_GRPC_ENDPOINT", "https://cockpit.local.znas.io")
		if got, want := resolveWorkerDialEndpoint(""), "https://cockpit.local.znas.io:443"; got != want {
			t.Errorf("resolveWorkerDialEndpoint(\"\") = %q, want %q", got, want)
		}
	})

	t.Run("the dial target is unchanged -- only the silence about TLS is gone", func(t *testing.T) {
		t.Setenv("MEMQL_WORKER_DIAL_ENDPOINT", "")
		t.Setenv("MEMQL_DISCOVERY_GRPC_ENDPOINT", "cockpit.local.znas.io:443")

		for _, tc := range []struct{ stored, wantBare string }{
			{"https://app.local.znas.io", "app.local.znas.io:443"},
			{"https://app.local.znas.io:8443/x", "app.local.znas.io:443"},
			{"http://localhost:3000", "localhost:50050"},
			{"", "cockpit.local.znas.io:443"},
		} {
			got := resolveWorkerDialEndpoint(tc.stored)
			if bare := stripDialScheme(got); bare != tc.wantBare {
				t.Errorf("resolveWorkerDialEndpoint(%q) = %q; stripped to %q, want the pre-fix %q -- "+
					"the fix may state the transport, it may not move the endpoint",
					tc.stored, got, bare, tc.wantBare)
			}
		}
	})
}

// stripDialScheme removes a leading `<scheme>://` if present.
func stripDialScheme(v string) string {
	if i := strings.Index(v, "://"); i >= 0 {
		return v[i+len("://"):]
	}
	return v
}
