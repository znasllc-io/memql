package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/cockpit/internal/worker/apps"
)

// fleet_apps_test.go pins app consent PER CLUSTER through the running
// fleet. It used to be one machine-wide list read by one closure that
// ignored the home and reported through one inventory shared by every
// home -- so allowing Claude Code for a local test cluster advertised it,
// allowed and signed in, to production as well, and production could have
// opened a session on it.

// claudeOnPath is a detector that finds a claude without any real one on
// this machine: a fixed lookup, a fixed version, and a signed-in account
// record in its own home.
func claudeOnPath(t *testing.T) *apps.Detector {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".claude.json"),
		[]byte(`{"oauthAccount":{"accountUuid":"u-1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return &apps.Detector{
		Home: home,
		LookPath: func(bin string) (string, error) {
			if bin == "claude" {
				return "/fake/bin/claude", nil
			}
			return "", errors.New("not found")
		},
		RunVersion: func(context.Context, string, []string) (string, error) { return "2.1.283 (Claude Code)", nil },
	}
}

func reportedApp(t *testing.T, r *memqlv1.Register, id string) *memqlv1.AppInfo {
	t.Helper()
	for _, a := range r.GetApps() {
		if a.GetId() == id {
			return a
		}
	}
	t.Fatalf("Register carries no %s: %v", id, r.GetApps())
	return nil
}

func TestFleetAppConsentIsPerCluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// No real claude may be reachable from here: the session this test
	// starts on the allowed cluster must stop at the PATH, not run one.
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	policy, _ := appsCmdPolicy(t, "apps:\n  homes:\n    local:\n      allow: [claude-code]\n")
	clusters := map[string]*fakeCluster{
		"local":      newFakeCluster(func(int) answer { return accept() }),
		"production": newFakeCluster(func(int) answer { return accept() }),
	}
	f, err := NewFleet(FleetOptions{
		Logger:  quietLogger(),
		Workers: fleetOf(root, "local", "production"),
		Policy:  policy,
		AppsFor: NewAppInventories(policy, claudeOnPath(t)).For,
	})
	if err != nil {
		t.Fatal(err)
	}
	pointAt(f, clusters)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	local := clusters["local"].next(t)
	production := clusters["production"].next(t)
	local.await(t, "Register", func(m *memqlv1.WorkerClientMessage) bool { return m.GetRegister() != nil })
	production.await(t, "Register", func(m *memqlv1.WorkerClientMessage) bool { return m.GetRegister() != nil })

	if a := reportedApp(t, local.register(), apps.IDClaudeCode); !a.GetAllowed() || !a.GetSignedIn() {
		t.Errorf("local was told %+v; its own entry allows claude-code", a)
	}
	if a := reportedApp(t, production.register(), apps.IDClaudeCode); a.GetAllowed() {
		t.Errorf("production was told claude-code is allowed; policy.yaml names only local")
	}

	start := func(s *scriptStream, id string) string {
		t.Helper()
		s.say(&memqlv1.WorkerServerMessage{Payload: &memqlv1.WorkerServerMessage_AppSessionStart{
			AppSessionStart: &memqlv1.AppSessionStart{SessionId: id, App: apps.IDClaudeCode, Kind: "run", Prompt: "hello"},
		}})
		end := s.await(t, "the End of "+id, func(m *memqlv1.WorkerClientMessage) bool {
			return m.GetAppSessionEnd().GetSessionId() == id
		})
		return end.GetAppSessionEnd().GetError()
	}
	if got := start(production, "on-production"); !strings.Contains(got, "apps.homes") {
		t.Errorf("production's session ended with %q, want the per-cluster consent refusal", got)
	}
	if got := start(local, "on-local"); strings.Contains(got, "apps.homes") {
		t.Errorf("local's session was refused by consent (%q); its own entry allows claude-code", got)
	}
}
