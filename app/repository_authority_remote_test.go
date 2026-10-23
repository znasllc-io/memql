package app

import (
	"encoding/json"
	"testing"
)

func TestRepositoryAuthorityPeersRequireOperatorBoundTLSAndRequestCredential(t *testing.T) {
	valid := repositoryAuthorityPeer{Endpoint: "https://api.example.test", CredentialSecret: "upstream-request-token"}
	encode := func(p repositoryAuthorityPeer) string {
		b, _ := json.Marshal(map[string]any{"formatVersion": 1, "peers": map[string]any{"maintainer": p}})
		return string(b)
	}
	if got, err := repositoryAuthorityPeerConfig(encode(valid), "maintainer"); err != nil || got != valid {
		t.Fatalf("valid peer: %+v %v", got, err)
	}
	for _, endpoint := range []string{"http://api.example.test", "api.example.test:443", "https://user:password@api.example.test", "https://api.example.test/path", "https://api.example.test?token=x", "https://api.example.test#fragment", "https://"} {
		p := valid
		p.Endpoint = endpoint
		if _, err := repositoryAuthorityPeerConfig(encode(p), "maintainer"); err == nil {
			t.Fatalf("accepted endpoint %q", endpoint)
		}
	}
	if _, err := repositoryAuthorityPeerConfig(encode(valid), "unregistered"); err == nil {
		t.Fatal("accepted caller-selected peer")
	}
	p := valid
	p.CredentialSecret = ""
	if _, err := repositoryAuthorityPeerConfig(encode(p), "maintainer"); err == nil {
		t.Fatal("accepted anonymous peer")
	}
	for _, raw := range []string{encode(valid) + "{}", `{"formatVersion":1,"owner":"forged","peers":{}}`, `{"formatVersion":2,"peers":{}}`} {
		if _, err := repositoryAuthorityPeerConfig(raw, "maintainer"); err == nil {
			t.Fatal("accepted malformed peer configuration")
		}
	}
}

func TestRepositoryAuthorityPeerDispatchPinsDestinationAndAuthenticatedRequester(t *testing.T) {
	original := repositoryAuthorityPeer{Endpoint: "https://api.example.test", CredentialSecret: "request-token", RootsSecret: "request-roots"}
	binding, err := repositoryAuthorityPeerDispatch(original, "v1:identity:user:person", "", nil)
	if err != nil || len(binding) != 64 {
		t.Fatalf("initial authenticated binding: %s %v", binding, err)
	}
	calls := 0
	call := func() error { calls++; return nil }
	// The secret contents do not enter the fingerprint. A fresh token for the
	// same peer-authenticated person keeps the same stable request identity.
	if got, err := repositoryAuthorityPeerDispatch(original, "person", binding, call); err != nil || got != binding || calls != 1 {
		t.Fatalf("same-principal credential rotation: %s %v calls=%d", got, err, calls)
	}
	for _, scenario := range []string{"endpoint", "credential reference", "trust reference", "requester", "missing identity", "missing binding"} {
		t.Run(scenario, func(t *testing.T) {
			peer, requester, expected := original, "person", binding
			switch scenario {
			case "endpoint":
				peer.Endpoint = "https://replacement.example.test"
			case "credential reference":
				peer.CredentialSecret = "different-token"
			case "trust reference":
				peer.RootsSecret = "different-roots"
			case "requester":
				requester = "another-person"
			case "missing identity":
				requester = ""
			case "missing binding":
				expected = ""
			}
			before := calls
			if _, err := repositoryAuthorityPeerDispatch(peer, requester, expected, call); err == nil || calls != before {
				t.Fatalf("request dispatched through changed binding: %v calls=%d", err, calls)
			}
		})
	}
}
