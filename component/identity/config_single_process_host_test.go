package identity

import "testing"

// A hostname never told you how many processes are behind it (memql#3593).
//
// isSingleProcessHost gates the memql#3400 guard, whose whole subject is "can
// there be more than one of me?". `identity.memql.localhost` is the local
// cluster's traefik front door and `make scale N=2` puts two identity pods
// behind it, so any answer that calls it one process re-opens the memql#1515
// outage: divergent JWKS across replicas, ~half of all token verifications
// failing with `unknown kid`.
//
// The exemption is therefore the literal loopback names and nothing else.
func TestIsSingleProcessHostIsLoopbackNamesOnly(t *testing.T) {
	single := []string{
		"localhost", "127.0.0.1", "::1", "0.0.0.0",
		"localhost:8085", "127.0.0.1:8085",
	}
	for _, host := range single {
		if !isSingleProcessHost(host) {
			t.Errorf("isSingleProcessHost(%q) = false, want true", host)
		}
	}

	notSingle := []string{
		"identity.memql.localhost",
		"api.memql.localhost",
		"memql.localhost",
		"identity.local.example.com",
		"identity.example.com",
	}
	for _, host := range notSingle {
		if isSingleProcessHost(host) {
			t.Errorf("isSingleProcessHost(%q) = true, want false -- a front door is not one process", host)
		}
	}
}
