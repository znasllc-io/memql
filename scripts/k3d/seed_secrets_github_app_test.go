package k3d

import (
	"strings"
	"testing"
)

// seed_secrets_github_app_test.go -- epic memql#4912's local-delivery half.
//
// THE ALL-OR-NONE RULE HAS TWO ENDS AND THEY DISAGREE ABOUT WHAT IS KIND. The
// identity node refuses to boot on a HALF-configured GitHub App -- one to five
// of the six values -- which is right in the cloud, where the operator who set
// four of six is the person who needs to hear about it. Locally the same
// refusal is a crash-loop that reads as "the cluster is broken", so this script
// blanks a partial set and says so. The tests below pin both directions,
// because the pleasant failure and the loud one are each wrong in the other
// place.
//
// They drive the REAL script against the fake kubectl and assert on the
// recorded `kubectl create secret` argv, matching the master-key and
// signing-key suites: what matters is not the arithmetic of which values were
// found, it is what the script ultimately WRITES to the cluster.

// githubAppValues is a complete, obviously-synthetic set. Built from parts
// rather than written as high-entropy literals, for the reason seedB64 is: a
// constant that looks like a key is what a secret scanner hunts for, and
// history is append-only.
var githubAppValues = map[string]string{
	"MEMQL_GITHUB_APP_ID":              "424242",
	"MEMQL_GITHUB_APP_SLUG":            "memql-" + "example",
	"MEMQL_GITHUB_APP_CLIENT_ID":       "Iv1." + strings.Repeat("c", 16),
	"MEMQL_GITHUB_APP_CLIENT_SECRET":   "not-a-real-" + "client-secret-" + strings.Repeat("s", 8),
	"MEMQL_GITHUB_APP_PRIVATE_KEY_B64": "bm90LWEtcmVhbC1w" + "cml2YXRlLWtleQ==",
	"MEMQL_GITHUB_APP_WEBHOOK_SECRET":  "not-a-real-" + "webhook-secret-" + strings.Repeat("w", 8),
}

// githubAppKeys is the order the six are asserted in, so a failure names them
// consistently.
var githubAppKeys = []string{
	"MEMQL_GITHUB_APP_ID",
	"MEMQL_GITHUB_APP_SLUG",
	"MEMQL_GITHUB_APP_CLIENT_ID",
	"MEMQL_GITHUB_APP_CLIENT_SECRET",
	"MEMQL_GITHUB_APP_PRIVATE_KEY_B64",
	"MEMQL_GITHUB_APP_WEBHOOK_SECRET",
}

// TestSeedSecretsSeedsEverySlotWithNoGitHubApp is the ordinary local cluster.
//
// The KEYS are present and EMPTY. That is deliberate and it is what makes the
// Secret's shape independent of configuration: a node envFroms this Secret, so
// a key that appears and disappears is an environment that changes shape
// between bring-ups, and "the variable is unset" and "the variable is empty"
// are the same thing to the config loader either way.
func TestSeedSecretsSeedsEverySlotWithNoGitHubApp(t *testing.T) {
	_, calls, code := runSeedSecrets(t, scenario{})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, key := range githubAppKeys {
		if got := seededLiteral(t, calls, key); got != "" {
			t.Errorf("%s = %q with nothing in the environment, want empty", key, got)
		}
	}
}

// TestSeedSecretsSeedsAllSixWhenAllSixAreExported.
func TestSeedSecretsSeedsAllSixWhenAllSixAreExported(t *testing.T) {
	_, calls, code := runSeedSecrets(t, scenario{githubApp: githubAppValues})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, key := range githubAppKeys {
		if got := seededLiteral(t, calls, key); got != githubAppValues[key] {
			t.Errorf("%s = %q, want %q", key, got, githubAppValues[key])
		}
	}
}

// TestSeedSecretsBlanksAPartialGitHubApp is the point of the whole exercise.
//
// Five of six exported means the operator is mid-setup. Seeding those five
// would make the identity node refuse to boot -- correctly, by its own rule --
// and a local cluster would come up with identity in CrashLoopBackOff and
// nothing saying why. So the script blanks all six and warns, naming the
// exports that would turn it on.
func TestSeedSecretsBlanksAPartialGitHubApp(t *testing.T) {
	partial := map[string]string{}
	for k, v := range githubAppValues {
		if k == "MEMQL_GITHUB_APP_PRIVATE_KEY_B64" {
			continue
		}
		partial[k] = v
	}

	_, calls, code := runSeedSecrets(t, scenario{githubApp: partial})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 -- a partial set disables the feature, it does not fail the run", code)
	}
	for _, key := range githubAppKeys {
		if got := seededLiteral(t, calls, key); got != "" {
			t.Errorf("%s = %q with only five of six exported.\n"+
				"A half-configured app REFUSES BOOT on the identity node by design, so seeding the "+
				"five that are present would leave a local cluster with identity crash-looping and "+
				"nothing saying why.", key, got)
		}
	}
}

// TestSeedSecretsWarnsAboutAPartialGitHubApp. Blanking silently would leave an
// operator who exported five values with a cluster that simply has no Connect
// button and no explanation.
func TestSeedSecretsWarnsAboutAPartialGitHubApp(t *testing.T) {
	partial := map[string]string{
		"MEMQL_GITHUB_APP_ID":   githubAppValues["MEMQL_GITHUB_APP_ID"],
		"MEMQL_GITHUB_APP_SLUG": githubAppValues["MEMQL_GITHUB_APP_SLUG"],
	}
	stderr := runSeedSecretsStderr(t, scenario{githubApp: partial})

	if !strings.Contains(stderr, "only partly configured") {
		t.Errorf("the run does not say the app is partly configured.\nstderr:\n%s", stderr)
	}
	for _, key := range githubAppKeys {
		if !strings.Contains(stderr, key) {
			t.Errorf("the warning does not name %s, so an operator cannot tell which export they "+
				"are missing.\nstderr:\n%s", key, stderr)
		}
	}
}

// TestSeedSecretsNeverLogsAGitHubAppSecret is the seed_secrets_signing_key
// pattern applied to the three values here that are secrets.
//
// seed-secrets.sh runs in operator terminals and its stderr lands in CI logs; a
// client secret, a webhook secret or an App private key that leaks there is one
// that must be rotated -- and rotating an App private key invalidates every
// installation token the cluster can mint.
//
// IT CARRIES ITS OWN POSITIVE CONTROL. A grep that finds nothing proves nothing
// until the instrument could have moved, so the test first requires that the
// run produced log output naming the GitHub App at all.
func TestSeedSecretsNeverLogsAGitHubAppSecret(t *testing.T) {
	stderr := runSeedSecretsStderr(t, scenario{githubApp: githubAppValues})

	if !strings.Contains(stderr, "GitHub Connect enabled") {
		t.Fatalf("the run logged nothing about the GitHub App, so scanning its output for a leaked "+
			"value proves nothing.\nstderr:\n%s", stderr)
	}
	for _, key := range []string{
		"MEMQL_GITHUB_APP_CLIENT_SECRET",
		"MEMQL_GITHUB_APP_PRIVATE_KEY_B64",
		"MEMQL_GITHUB_APP_WEBHOOK_SECRET",
	} {
		if strings.Contains(stderr, githubAppValues[key]) {
			t.Errorf("the value of %s appears in the script's human log output.\nstderr:\n%s", key, stderr)
		}
	}
}
