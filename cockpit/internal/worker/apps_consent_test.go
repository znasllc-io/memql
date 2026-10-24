package worker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/worker/inference"
	"github.com/znasllc-io/memql/cockpit/internal/worker/tools"
)

// apps_consent_test.go pins `memql worker apps --allow/--deny [--home]`:
// app consent written per cluster into policy.yaml, as a textual edit that
// leaves every other byte of the owner's file alone, then the running
// worker signalled. Before it, the only way to allow an app was to know
// the file, the key and the signal -- which is what the owner of the test
// Mac had to do by hand.

// The owner's policy.yaml as it stood when app consent went per cluster:
// the machine-wide list, which no longer allows anything. Allowing the app
// for the cluster they meant MIGRATES the file: the per-cluster entry is
// written, and the retired list goes in the same edit.
func TestEditAppConsent_MigratesTheRetiredMachineWideList(t *testing.T) {
	for name, before := range map[string]string{
		"block list": "apps:\n  allow:\n    - claude-code\n",
		"flow list":  "apps:\n  allow: [claude-code]\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, changed, change, err := editAppConsent(before, "api.memql.localhost", []string{"claude-code"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := "apps:\n  homes:\n    api.memql.localhost:\n      allow:\n        - claude-code\n"
			if got != want || !changed {
				t.Errorf("got changed=%v\n%s\nwant\n%s", changed, got, want)
			}
			if strings.Join(change.retired, ",") != "claude-code" || change.retiredErr != nil {
				t.Errorf("change = %+v, want the retired list's claude-code reported", change)
			}
			assertAllows(t, got, "api.memql.localhost", "claude-code")
			assertAllows(t, got, "api.memql.example.com")
		})
	}
}

// operatorsPolicy has an owner's fingerprints on it -- comments, a blank
// line, a four-space list elsewhere, levels under apps -- and every one of
// them must come back as it was.
const operatorsPolicy = `# ~/.memql/policy.yaml -- edited by hand.

shell:
    allow:
        - rustc   # firmware builds

apps:
  homes:
    api.memql.localhost:
      allow:
        - claude-code   # local testing only
  levels:
    claude-code:
      reasoning: {model: opus}
`

func TestEditAppConsent_LeavesEveryOtherByteAlone(t *testing.T) {
	cases := []struct {
		name        string
		home        string
		allow, deny []string
		want        string
	}{{
		name:  "another app for a cluster already listed",
		home:  "api.memql.localhost",
		allow: []string{"codex"},
		want: strings.Replace(operatorsPolicy, "        - claude-code   # local testing only\n",
			"        - claude-code   # local testing only\n        - codex\n", 1),
	}, {
		name:  "a cluster the file does not name yet",
		home:  "api.memql.example.com",
		allow: []string{"codex"},
		want: strings.Replace(operatorsPolicy, "  homes:\n",
			"  homes:\n    api.memql.example.com:\n      allow:\n        - codex\n", 1),
	}, {
		name:  "a cluster named with other capitals is the same cluster",
		home:  "API.memql.LOCALHOST",
		allow: []string{"codex"},
		want: strings.Replace(operatorsPolicy, "        - claude-code   # local testing only\n",
			"        - claude-code   # local testing only\n        - codex\n", 1),
	}, {
		name: "withdrawing the last app leaves the key with nothing under it",
		home: "api.memql.localhost",
		deny: []string{"Claude-Code"},
		want: strings.Replace(operatorsPolicy, "        - claude-code   # local testing only\n", "", 1),
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed, _, err := editAppConsent(operatorsPolicy, tc.home, tc.allow, tc.deny)
			if err != nil {
				t.Fatal(err)
			}
			if !changed || got != tc.want {
				t.Errorf("changed=%v\n--- got ---\n%s\n--- want ---\n%s", changed, got, tc.want)
			}
		})
	}
}

func TestEditAppConsent_WithdrawsOneOfSeveral(t *testing.T) {
	for name, tc := range map[string]struct{ before, want string }{
		"block list": {
			"apps:\n  homes:\n    local:\n      allow:\n        - claude-code\n        - codex\n",
			"apps:\n  homes:\n    local:\n      allow:\n        - codex\n",
		},
		"flow list": {
			"apps:\n  homes:\n    local:\n      allow: [claude-code, codex]  # both\n",
			"apps:\n  homes:\n    local:\n      allow: [codex]  # both\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, _, _, err := editAppConsent(tc.before, "local", nil, []string{"claude-code"})
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got\n%s\nwant\n%s", got, tc.want)
			}
			assertAllows(t, got, "local", "codex")
		})
	}
}

// Nothing to do writes nothing: allowing what is allowed, withdrawing what
// was never there.
func TestEditAppConsent_NothingToDoChangesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		home        string
		allow, deny []string
	}{
		"already allowed":        {"api.memql.localhost", []string{"claude-code"}, nil},
		"never allowed":          {"api.memql.localhost", nil, []string{"codex"}},
		"a cluster not in there": {"api.memql.example.com", nil, []string{"claude-code"}},
	} {
		t.Run(name, func(t *testing.T) {
			got, changed, _, err := editAppConsent(operatorsPolicy, tc.home, tc.allow, tc.deny)
			if err != nil || changed || got != operatorsPolicy {
				t.Errorf("changed=%v err=%v; the file must come back untouched", changed, err)
			}
		})
	}
}

// A fresh machine has no policy.yaml at all.
func TestEditAppConsent_OnAnEmptyFile(t *testing.T) {
	got, changed, _, err := editAppConsent("", "local", []string{"claude-code"}, nil)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !strings.HasPrefix(got, "# MemQL Cockpit worker policy.") {
		t.Errorf("a file this command creates says what it is:\n%s", got)
	}
	assertAllows(t, got, "local", "claude-code")
}

// A shape the editor will not touch is refused, naming what to do by hand.
func TestEditAppConsent_RefusesAShapeItCannotEdit(t *testing.T) {
	_, _, _, err := editAppConsent("apps: {homes: {local: {allow: [codex]}}}\n", "local", []string{"claude-code"}, nil)
	if !errors.Is(err, inference.ErrPolicyNotEditable) || !strings.Contains(err.Error(), "claude-code to apps.homes.local.allow") {
		t.Errorf("err = %v, want a refusal naming the change", err)
	}
}

func assertAllows(t *testing.T, body, home string, want ...string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := tools.LoadPolicy(path)
	if err != nil {
		t.Fatalf("the worker cannot load what was written: %v\n%s", err, body)
	}
	if got := p.AppsAllowFor(home); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s allows %v, want %v\n%s", home, got, want, body)
	}
}

func homesNamed(ids ...string) []Home {
	var out []Home
	for _, id := range ids {
		out = append(out, Home{ID: id, ClusterURL: "https://" + id})
	}
	return out
}

func TestResolveConsentHome(t *testing.T) {
	two := homesNamed("api.memql.localhost", "api.memql.example.com")
	cases := []struct {
		name  string
		homes []Home
		flag  string
		want  string
		err   []string
	}{
		{name: "the only cluster, unnamed", homes: homesNamed("api.memql.localhost"), want: "api.memql.localhost"},
		{name: "by id", homes: two, flag: "api.memql.example.com", want: "api.memql.example.com"},
		{name: "by id, other capitals", homes: two, flag: "API.memql.Example.com", want: "api.memql.example.com"},
		{name: "by cluster URL", homes: two, flag: "https://api.memql.localhost/", want: "api.memql.localhost"},
		{name: "several, unnamed", homes: two, err: []string{
			"enrolled with 2 clusters",
			"memql worker apps --allow claude-code --home api.memql.localhost",
			"memql worker apps --allow claude-code --home api.memql.example.com",
		}},
		{name: "a cluster this machine is not enrolled with", homes: two, flag: "api.other.example", err: []string{
			`not enrolled with "api.other.example"`, "api.memql.localhost", "api.memql.example.com",
		}},
		{name: "nothing enrolled", err: []string{"not enrolled with any cluster", "memql worker pair"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveConsentHome(tc.homes, tc.flag, []string{"claude-code"}, nil)
			if len(tc.err) == 0 {
				if err != nil || got != tc.want {
					t.Errorf("got %q, %v; want %q", got, err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("got %q, want a refusal", got)
			}
			for _, want := range tc.err {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to say %q", err, want)
				}
			}
		})
	}
}

// The whole command: the edit, what it says, and the signal -- sent only
// when something changed.
func TestRunAppConsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte("apps:\n  allow: [claude-code]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	signals := 0
	hup := func(context.Context) error { signals++; return nil }
	var out bytes.Buffer
	req := appConsentRequest{
		policyPath: path, homes: homesNamed("api.memql.localhost", "api.memql.example.com"),
		home: "api.memql.localhost", allow: []string{"claude-code"},
	}
	if err := runAppConsent(context.Background(), &out, req, hup); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"api.memql.localhost now allows claude-code",
		"Removed the machine-wide apps.allow (claude-code)",
		"The running worker was signalled",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output is missing %q:\n%s", want, out.String())
		}
	}
	if signals != 1 {
		t.Errorf("signalled %d times, want once", signals)
	}
	assertAllows(t, readFile(t, path), "api.memql.localhost", "claude-code")
	assertAllows(t, readFile(t, path), "api.memql.example.com")

	out.Reset()
	if err := runAppConsent(context.Background(), &out, req, hup); err != nil {
		t.Fatal(err)
	}
	if signals != 1 || !strings.Contains(out.String(), "already") {
		t.Errorf("a second identical run signalled (%d) or said %q; nothing changed", signals, out.String())
	}

	// A worker that is not running is a sentence, not a failure: the file
	// is what lasts, and the next worker reads it.
	out.Reset()
	req.allow, req.deny = nil, []string{"claude-code"}
	if err := runAppConsent(context.Background(), &out, req, func(context.Context) error { return inference.ErrNoWorker }); err != nil {
		t.Fatalf("a stopped worker failed the command: %v", err)
	}
	if !strings.Contains(out.String(), "api.memql.localhost now allows no app") || !strings.Contains(out.String(), "No running worker") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestRunAppConsent_RefusesWhatItCannotDo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	homes := homesNamed("local")
	for name, tc := range map[string]struct {
		req  appConsentRequest
		want string
	}{
		"an app this cockpit does not drive": {
			appConsentRequest{policyPath: path, homes: homes, allow: []string{"gemini-cli"}},
			`drives no app called "gemini-cli"`,
		},
		"allow and deny at once": {
			appConsentRequest{policyPath: path, homes: homes, allow: []string{"codex"}, deny: []string{"Codex"}},
			"both --allow and --deny",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := runAppConsent(context.Background(), &bytes.Buffer{}, tc.req, func(context.Context) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) || SetupExitCode(err) != SetupExitUsage {
				t.Errorf("err = %v (exit %d), want a usage refusal saying %q", err, SetupExitCode(err), tc.want)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Error("a refused command wrote policy.yaml")
			}
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
