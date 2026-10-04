package k3d

import (
	"regexp"
	"strings"
	"testing"
)

// Exercise the real installer so generating a key without delivering it cannot
// pass. A reseed must preserve previously issued unsubscribe links.
func TestCampaignKeyBootstrap(t *testing.T) {
	for _, state := range []string{"absent", "present"} {
		t.Run(state, func(t *testing.T) {
			stdout, stderr, calls, code := runSeedSecretsFull(t, scenario{secretState: state})
			if code != 0 {
				t.Fatalf("seed failed: %s %s", stdout, stderr)
			}
			key := seededLiteral(t, calls, "MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET")
			if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(key) {
				t.Fatal("installer did not deliver a 256-bit key")
			}
			if strings.Contains(stdout+stderr, key) {
				t.Fatal("installer logged its key")
			}
			again, logs, next, exit := runSeedSecretsFull(t, scenario{secretState: "present", clusterCampaignKey: key})
			if exit != 0 || seededLiteral(t, next, "MEMQL_CAMPAIGNS_UNSUBSCRIBE_SECRET") != key {
				t.Fatal("reseed replaced the signing key")
			}
			if strings.Contains(again+logs, key) {
				t.Fatal("reseed logged its key")
			}
		})
	}
}

func TestCampaignKeyReadFailureStopsBeforeMutation(t *testing.T) {
	_, _, calls, code := runSeedSecretsFull(t, scenario{secretState: "present", campaignReadFails: true})
	if code == 0 {
		t.Fatal("failed read was treated as a missing key")
	}
	if mutations := mutatedAnything(calls); len(mutations) != 0 {
		t.Fatalf("mutated cluster after failed key read: %v", mutations)
	}
}
