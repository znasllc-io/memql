package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
	sdkworker "github.com/znasllc-io/memql/sdk/go/worker"
)

// The two halves of the worker-pairing dial contract, joined (memql#3437).
//
// The identity service resolves the address a paired worker should dial and
// puts it in the redeem reply's `clusterUrl`
// (component/identity/http/pair.go). The worker turns that string into a
// connection with sdk/go/worker.ParseClusterURL, which is where the transport
// decision is actually MADE -- and it reads a bare `host:port` as
// useTLS=false. Nothing in the build links the emitter to that parser, and
// neither half was wrong on its own: the server produced a legitimate dial
// address, and the parser applied its documented default. Together they
// dialled cockpit.local.znas.io:443 in plaintext, putting an `mql_wkr_`
// bearer token on the wire before the handshake failed.
//
// These tests are the join. They live in the root package because it is the
// only module that requires both component/identity and sdk/go -- the same
// reason discovery_endpoint_deploy_test.go lives here.
//
// The cockpit is NOT patched for this: cmd/memql-cockpit/internal/worker/
// connect.go already feeds the reply straight to ParseClusterURL and uses the
// returned flag, so a scheme-carrying value flows through unchanged.

// pairingDialURL mirrors the redeem handler's tiering below the operator
// override, which is what the shared #3399 fixture describes.
func pairingDialURL(configured, identityURL string) string {
	if v := identity.DialURLFromEndpoint(configured); v != "" {
		return v
	}
	return identity.DialURLFromOrigin(identityURL)
}

// TestPairingReplyTellsTheWorkerToUseTLS is acceptance criterion 2 stated
// end to end: a value the SERVER produced for a TLS endpoint must not come
// out of ParseClusterURL as plaintext.
func TestPairingReplyTellsTheWorkerToUseTLS(t *testing.T) {
	cases := []struct {
		name         string
		storedOrigin string
		wantEndpoint string
		wantTLS      bool
	}{
		{
			name:         "the local front door -- the exact value that dialled :443 in the clear",
			storedOrigin: "https://cockpit.local.znas.io",
			wantEndpoint: "cockpit.local.znas.io:443",
			wantTLS:      true,
		},
		{
			name:         "the SPA origin a pairing code is actually minted from",
			storedOrigin: "https://app.local.znas.io",
			wantEndpoint: "app.local.znas.io:443",
			wantTLS:      true,
		},
		{
			name:         "a cloud origin",
			storedOrigin: "https://app.acme.com",
			wantEndpoint: "app.acme.com:443",
			wantTLS:      true,
		},
		{
			name:         "the SPA's own HTTPS port is not the gRPC port, and does not change the answer",
			storedOrigin: "https://app.acme.com:8443",
			wantEndpoint: "app.acme.com:443",
			wantTLS:      true,
		},
		{
			name:         "a plaintext dev origin is still plaintext -- the fix states the transport, it does not force TLS",
			storedOrigin: "http://localhost:3000",
			wantEndpoint: "localhost:50050",
			wantTLS:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply := identity.DialURLFromOrigin(tc.storedOrigin)
			endpoint, useTLS, err := sdkworker.ParseClusterURL(reply)
			if err != nil {
				t.Fatalf("ParseClusterURL(%q): %v", reply, err)
			}
			if endpoint != tc.wantEndpoint {
				t.Errorf("origin %q -> reply %q -> endpoint %q, want %q",
					tc.storedOrigin, reply, endpoint, tc.wantEndpoint)
			}
			if useTLS != tc.wantTLS {
				t.Errorf("origin %q -> reply %q -> useTLS=%v, want %v -- "+
					"the worker would dial %q with the wrong transport",
					tc.storedOrigin, reply, useTLS, tc.wantTLS, endpoint)
			}
		})
	}
}

// TestPairingReplyNeverMovesTheDialTarget is the safety half. Changing the
// reply's FORM on a credential-carrying hop is only defensible if it changes
// nothing about WHERE the worker connects, so this replays every case in the
// cross-language discovery fixture and asserts the endpoint ParseClusterURL
// extracts is byte-identical to the address the discovery document advertises
// for the same configuration.
func TestPairingReplyNeverMovesTheDialTarget(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("test", "fixtures", "discovery-endpoint-contract.json"))
	if err != nil {
		t.Fatalf("read shared contract fixture: %v", err)
	}
	var contract struct {
		Cases []struct {
			Name        string `json:"name"`
			Configured  string `json:"configured"`
			IdentityURL string `json:"identityUrl"`
			Advertised  string `json:"advertised"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("parse shared contract fixture: %v", err)
	}
	if len(contract.Cases) == 0 {
		t.Fatal("shared contract fixture carries no cases")
	}

	for _, tc := range contract.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			reply := pairingDialURL(tc.Configured, tc.IdentityURL)
			endpoint, useTLS, err := sdkworker.ParseClusterURL(reply)
			if err != nil {
				t.Fatalf("ParseClusterURL(%q): %v", reply, err)
			}
			if endpoint != tc.Advertised {
				t.Errorf("configured=%q identityUrl=%q: reply %q dials %q, but the cluster advertises %q",
					tc.Configured, tc.IdentityURL, reply, endpoint, tc.Advertised)
			}
			// Wherever the reply states a transport, it must be the one the
			// scheme names -- no silent inversion in the parser.
			if wantTLS := strings.HasPrefix(reply, "https://"); strings.Contains(reply, "://") && useTLS != wantTLS {
				t.Errorf("reply %q parsed as useTLS=%v, want %v", reply, useTLS, wantTLS)
			}
		})
	}
}
