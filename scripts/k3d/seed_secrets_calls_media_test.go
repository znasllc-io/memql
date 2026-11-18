package k3d

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// seed_secrets_calls_media_test.go -- the memql-calls-media Secret, the eight
// MEMQL_CALLS_MEDIA_* values the bff reads to reach the local media server
// (integrations/calls/config.go; scripts/media/local-up.sh starts the server).
//
// Four values are the local target's and are written on every run, derived from the
// domain the run resolved; four are the two key pairs, generated once and then kept,
// because the media server and the engine must agree on them and the engine seals them
// into the graph at boot. Same shape as the voice credentials beside them.

var callsMediaLiteral = regexp.MustCompile(`--from-literal=(MEMQL_CALLS_MEDIA_[A-Z_]+)=(\S*)`)

// callsMediaLiterals returns the values the run asked kubectl to write into
// memql-calls-media, and fails when no such write was recorded.
func callsMediaLiterals(t *testing.T, calls []string) map[string]string {
	t.Helper()
	for _, c := range calls {
		if !strings.Contains(c, "create secret generic memql-calls-media") {
			continue
		}
		out := map[string]string{}
		for _, m := range callsMediaLiteral.FindAllStringSubmatch(c, -1) {
			out[m[1]] = m[2]
		}
		return out
	}
	t.Fatalf("no `create secret generic memql-calls-media` was recorded.\ncalls:\n  %s", strings.Join(calls, "\n  "))
	return nil
}

var hexSecret = regexp.MustCompile(`^[0-9a-f]{64}$`)

func requireJQ(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed; seed-secrets.sh reads an existing Secret with it")
	}
}

func TestCallsMediaSecretDescribesTheLocalMediaTarget(t *testing.T) {
	stdout, stderr, calls, code := runSeedSecretsFull(t, scenario{})
	if code != 0 {
		t.Fatalf("seed failed (exit %d): %s", code, stdout)
	}
	got := callsMediaLiterals(t, calls)
	for name, want := range map[string]string{
		"MEMQL_CALLS_MEDIA_PROVIDER":   "docker-local",
		"MEMQL_CALLS_MEDIA_API_URL":    "http://host.k3d.internal:7880",
		"MEMQL_CALLS_MEDIA_PUBLIC_URL": "wss://media.memql.localhost:7443",
		"MEMQL_CALLS_MEDIA_SEATS":      "50",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q", name, got[name], want)
		}
	}
	if len(got) != 8 {
		t.Errorf("memql-calls-media holds %d values, want all eight: the engine refuses boot on a partial set", len(got))
	}
	if got["MEMQL_CALLS_MEDIA_API_KEY"] == "" || got["MEMQL_CALLS_MEDIA_WEBHOOK_KEY"] == "" {
		t.Error("a key pair was seeded with no key")
	}
	if got["MEMQL_CALLS_MEDIA_API_KEY"] == got["MEMQL_CALLS_MEDIA_WEBHOOK_KEY"] {
		t.Error("the room key and the webhook key are the same key, so they cannot be rotated apart")
	}
	for _, name := range []string{"MEMQL_CALLS_MEDIA_API_SECRET", "MEMQL_CALLS_MEDIA_WEBHOOK_SECRET"} {
		if !hexSecret.MatchString(got[name]) {
			t.Errorf("%s is not 256 random bits as hex", name)
		}
	}
	for _, name := range []string{"MEMQL_CALLS_MEDIA_API_SECRET", "MEMQL_CALLS_MEDIA_WEBHOOK_SECRET"} {
		if strings.Contains(stdout+stderr, got[name]) {
			t.Errorf("seed-secrets.sh logged %s", name)
		}
	}
}

// A routine `make secrets` must not rotate the key pairs: the running media server was
// rendered with them (make media-up), and a rotation it did not hear about refuses every
// pass and every webhook until somebody restarts it.
func TestCallsMediaCredentialsSurviveAReseed(t *testing.T) {
	requireJQ(t)
	_, _, calls, code := runSeedSecretsFull(t, scenario{})
	if code != 0 {
		t.Fatal("first seed failed")
	}
	first := callsMediaLiterals(t, calls)
	held := map[string]string{}
	for k, v := range first {
		held[k] = v
	}
	// A stale derived value is re-derived, never kept: the four target values follow
	// the run, the four credentials follow the cluster.
	held["MEMQL_CALLS_MEDIA_PUBLIC_URL"] = "wss://media.stale.invalid:7443"

	stdout, stderr, again, code := runSeedSecretsFull(t, scenario{clusterCallsMedia: held})
	if code != 0 {
		t.Fatalf("reseed failed (exit %d): %s", code, stdout)
	}
	second := callsMediaLiterals(t, again)
	for _, name := range []string{
		"MEMQL_CALLS_MEDIA_API_KEY", "MEMQL_CALLS_MEDIA_API_SECRET",
		"MEMQL_CALLS_MEDIA_WEBHOOK_KEY", "MEMQL_CALLS_MEDIA_WEBHOOK_SECRET",
	} {
		if second[name] != first[name] {
			t.Errorf("a reseed replaced %s", name)
		}
	}
	if second["MEMQL_CALLS_MEDIA_PUBLIC_URL"] != "wss://media.memql.localhost:7443" {
		t.Errorf("the public URL was carried over (%q) rather than derived from the domain", second["MEMQL_CALLS_MEDIA_PUBLIC_URL"])
	}
	if strings.Contains(stdout+stderr, first["MEMQL_CALLS_MEDIA_API_SECRET"]) {
		t.Error("the reseed logged the API secret")
	}
}

// A Secret holding one pair and not the other is refused before anything is written: a
// fresh second pair would be a credential the running media server does not have, and
// guessing which half is right is not this script's call.
func TestAnIncompleteCallsMediaSecretIsRefusedBeforeAnyWrite(t *testing.T) {
	requireJQ(t)
	stdout, _, calls, code := runSeedSecretsFull(t, scenario{clusterCallsMedia: map[string]string{
		"MEMQL_CALLS_MEDIA_API_KEY":    "callsapi-fixture",
		"MEMQL_CALLS_MEDIA_API_SECRET": strings.Repeat("ab", 32),
	}})
	if code != 3 {
		t.Fatalf("an incomplete memql-calls-media exited %d, want 3 (refused): %s", code, stdout)
	}
	if !strings.Contains(stdout, "memql-calls-media") {
		t.Errorf("the refusal does not name the Secret: %s", stdout)
	}
	if muts := mutatedAnything(calls); len(muts) != 0 {
		t.Fatalf("the cluster was written before the refusal: %v", muts)
	}
}

func TestAnUnreadableCallsMediaSecretStopsBeforeAnyWrite(t *testing.T) {
	_, _, calls, code := runSeedSecretsFull(t, scenario{callsMediaReadFails: true})
	if code == 0 {
		t.Fatal("a failed read was treated as a cluster with no media credentials, which would mint new ones")
	}
	if muts := mutatedAnything(calls); len(muts) != 0 {
		t.Fatalf("the cluster was written after a failed read: %v", muts)
	}
}
