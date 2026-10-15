package tools_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/tools"
)

// apps_consent_policy_test.go pins app consent PER CLUSTER
// (apps.homes.<home>.allow). A machine paired with two clusters used to
// carry one machine-wide apps.allow, so allowing Claude Code to test it
// against a local cluster offered it to production as well. Tool consent
// windows were already per home (memql-cockpit#433: each cluster gets this
// machine on the terms its owner set for THAT cluster); app sessions, which
// do exactly what workerHost.exec does, now follow the same rule.

const twoHomes = `apps:
  homes:
    api.memql.localhost:
      allow:
        - claude-code
    api.memql.example.com:
      allow: []
`

// A cluster the file names gets exactly its own list; a cluster the file
// does not name gets NOTHING. Default-deny per home, never an inheritance
// from a machine-wide list.
func TestAppsAllowFor_IsPerCluster(t *testing.T) {
	p := policyFrom(t, twoHomes)
	if got := p.AppsAllowFor("api.memql.localhost"); len(got) != 1 || got[0] != "claude-code" {
		t.Errorf("local = %v, want [claude-code]", got)
	}
	if got := p.AppsAllowFor("api.memql.example.com"); len(got) != 0 {
		t.Errorf("a cluster whose list is empty = %v, want nothing", got)
	}
	if got := p.AppsAllowFor("a-cluster-added-later"); len(got) != 0 {
		t.Errorf("a cluster the file does not name = %v, want nothing (default-deny)", got)
	}
	if problems := p.AppConsentProblems(); len(problems) != 0 {
		t.Errorf("a well-formed block has no problems, got %v", problems)
	}
}

func TestAppsAllowFor_DefaultsToDeny(t *testing.T) {
	if got := tools.DefaultPolicy().AppsAllowFor("api.memql.localhost"); len(got) != 0 {
		t.Errorf("default = %v, want nothing", got)
	}
	var nilPolicy *tools.Policy
	if got := nilPolicy.AppsAllowFor("api.memql.localhost"); got != nil {
		t.Errorf("nil policy = %v, want nil", got)
	}
}

// Home ids are hostnames and registry names; a capital letter the owner
// typed is not a different cluster.
func TestAppsAllowFor_ReadsTheHomeWithoutRegardToCase(t *testing.T) {
	p := policyFrom(t, "apps:\n  homes:\n    API.memql.localhost:\n      allow: [Claude-Code]\n")
	if got := p.AppsAllowFor("api.memql.localhost"); len(got) != 1 || got[0] != "claude-code" {
		t.Errorf("got %v, want [claude-code]", got)
	}
}

// The copy matters: the worker asks on every beat and every session start,
// and a shared slice would race a SIGHUP reload.
func TestAppsAllowFor_ReturnsACopy(t *testing.T) {
	p := policyFrom(t, twoHomes)
	got := p.AppsAllowFor("api.memql.localhost")
	got[0] = "mutated"
	if again := p.AppsAllowFor("api.memql.localhost"); again[0] != "claude-code" {
		t.Errorf("AppsAllowFor handed out its backing array: %v", again)
	}
}

// A SIGHUP REVOKES. The block replaces on reload:
// merging a consent would make withdrawing it take a restart, which is the
// wrong direction for the setting that lets a cluster run an app here.
func TestAppsHomes_AReloadGrantsAndRevokes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("apps:\n  homes:\n    local:\n      allow: [claude-code]\n")
	p, err := tools.LoadPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	write("apps:\n  homes:\n    local:\n      allow: [claude-code, codex]\n")
	if err := p.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := p.AppsAllowFor("local"); len(got) != 2 {
		t.Fatalf("after a grant = %v, want two apps", got)
	}
	write("apps:\n  homes:\n    local:\n      allow: [codex]\n")
	if err := p.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := p.AppsAllowFor("local"); len(got) != 1 || got[0] != "codex" {
		t.Errorf("after a withdrawal = %v, want [codex] alone", got)
	}
}

// THE RETIRED MACHINE-WIDE LIST ALLOWS NOTHING, AND SAYS SO. It is not
// honoured -- honouring it is the very crossing this change closes -- and
// it is not silently dropped either: the owner is told, with the command
// that says which cluster they meant.
func TestAppsAllow_TheRetiredMachineWideListAllowsNothing(t *testing.T) {
	p := policyFrom(t, "shell:\n  allow: [terraform]\napps:\n  allow:\n    - claude-code\n")
	if got := p.AppsAllowFor("api.memql.localhost"); len(got) != 0 {
		t.Errorf("the machine-wide list still allowed %v", got)
	}
	problems := strings.Join(p.AppConsentProblems(), "\n")
	for _, want := range []string{
		"apps.allow is no longer read",
		"memql worker apps --allow claude-code --home",
	} {
		if !strings.Contains(problems, want) {
			t.Errorf("problems = %q, want them to say %q", problems, want)
		}
	}
	if err := p.CheckShell("terraform plan"); err != nil {
		t.Errorf("the rest of the file must still load: %v", err)
	}
}

// ONE WRONG ENTRY COSTS THAT ENTRY. The block is walked as a node, for the
// reason apps.levels is: a typed decode fails the WHOLE file on a
// shorthand, and a worker that cannot parse policy.yaml runs on defaults
// that lose every other line the owner wrote.
func TestAppsHomes_AMisshapenEntryIsAProblemNotAFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want string
	}{
		"a list for a cluster": {
			body: "apps:\n  homes:\n    local: [claude-code]\n",
			want: "apps.homes.local: a mapping with allow: belongs here",
		},
		"a word for the list": {
			body: "apps:\n  homes:\n    local:\n      allow: claude-code\n",
			want: "apps.homes.local.allow: a list of apps belongs here",
		},
		"a misspelt key": {
			body: "apps:\n  homes:\n    local:\n      alow: [claude-code]\n",
			want: `apps.homes.local: takes allow, and "alow" is not it`,
		},
		"an app this cockpit does not drive": {
			body: "apps:\n  homes:\n    local:\n      allow: [gemini-cli]\n",
			want: `apps.homes.local.allow: this cockpit drives no app called "gemini-cli"`,
		},
		"a list of clusters": {
			body: "apps:\n  homes:\n    - local\n",
			want: "apps.homes: a mapping of clusters belongs here",
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := policyFrom(t, "shell:\n  allow: [terraform]\n"+tc.body)
			if got := p.AppsAllowFor("local"); len(got) != 0 {
				t.Errorf("a misshapen entry allowed %v", got)
			}
			if problems := strings.Join(p.AppConsentProblems(), "\n"); !strings.Contains(problems, tc.want) {
				t.Errorf("problems = %q, want %q", problems, tc.want)
			}
			if err := p.CheckShell("terraform plan"); err != nil {
				t.Errorf("the rest of the file was lost to the apps block: %v", err)
			}
		})
	}
}
