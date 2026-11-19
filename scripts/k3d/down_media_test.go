// Tests for the calls media server's part in scripts/k3d/down.sh (make down).
//
// `make media-up` starts the media server beside the cluster with restart: unless-stopped
// and its key pairs rendered into ~/.memql/media, and `make down` used to leave both:
// the containers came back with Docker, holding credentials for a cluster that no longer
// existed. down.sh now runs scripts/media/local-down.sh (make media-down) when the tree
// has it, before it deletes the cluster.
//
// Hermetic in the same way as down_purge_test.go: stub k3d, kubectl and docker, and a
// HOME of the test's own.
package k3d

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// renderedMediaFiles are what make media-up leaves in ~/.memql/media.
var renderedMediaFiles = []string{"livekit.yaml", "Caddyfile", "front-door-ca.pem"}

func homeWithRenderedMedia(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	state := filepath.Join(home, ".memql", "media")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range renderedMediaFiles {
		if err := os.WriteFile(filepath.Join(state, name), []byte("rendered\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func TestDownStopsTheLocalMediaServer(t *testing.T) {
	home := homeWithRenderedMedia(t)
	code, env, calls := downRunWithHome(t, home, []string{"DOWN_MEDIA_RUNNING=1"})
	if code != 0 || !env.OK || !env.Result.Deleted {
		t.Fatalf("exit %d ok=%v deleted=%v, want the cluster deleted\ncalls:\n%s", code, env.OK, env.Result.Deleted, calls)
	}
	if env.Result.MediaServer != "stopped" {
		t.Errorf("mediaServer = %q, want stopped", env.Result.MediaServer)
	}
	stop := strings.Index(calls, "docker compose --project-name memql-media down --remove-orphans")
	if stop < 0 {
		t.Fatalf("make down did not take the media server down:\n%s", calls)
	}
	if del := strings.Index(calls, "k3d cluster delete memql"); del >= 0 && del < stop {
		t.Errorf("the cluster was deleted before the media server was stopped:\n%s", calls)
	}
	for _, name := range renderedMediaFiles {
		if _, err := os.Stat(filepath.Join(home, ".memql", "media", name)); err == nil {
			t.Errorf("%s survived make down; it carries the media server's credentials", name)
		}
	}
}

// No Docker daemon means no media server can be running, so there is nothing to stop and
// the cluster teardown goes on.
func TestDownWithDockerStoppedHasNoMediaServerToStop(t *testing.T) {
	code, env, calls := downRun(t, []string{"DOWN_DOCKER_DOWN=1"})
	if code != 0 || !env.OK || !env.Result.Deleted {
		t.Fatalf("exit %d ok=%v deleted=%v, want the cluster deleted\ncalls:\n%s", code, env.OK, env.Result.Deleted, calls)
	}
	if env.Result.MediaServer != "unavailable" || env.Result.MediaServerDetail == "" {
		t.Errorf("mediaServer = %q (%q), want unavailable with the reason", env.Result.MediaServer, env.Result.MediaServerDetail)
	}
}

// A media server that would not stop fails make down, but only after the cluster is gone:
// the teardown the operator asked for happens, and the failure is not swallowed.
func TestDownFailsAfterTheClusterWhenTheMediaServerWillNotStop(t *testing.T) {
	code, env, calls := downRun(t, []string{"DOWN_MEDIA_RUNNING=1", "DOWN_MEDIA_DOWN_FAILS=1"})
	if code != 5 || env.OK || env.Error == nil || env.Error.Code != 5 {
		t.Fatalf("exit %d, envelope %+v; want a code-5 failure\ncalls:\n%s", code, env, calls)
	}
	if !strings.Contains(calls, "k3d cluster delete memql") || !env.Result.Deleted {
		t.Errorf("the cluster was not deleted before the failure was reported:\n%s", calls)
	}
	if env.Result.MediaServer != "failed" || !strings.Contains(env.Error.Message, "make media-down") {
		t.Errorf("mediaServer = %q, message %q; want failed, naming make media-down", env.Result.MediaServer, env.Error.Message)
	}
}

// A fake make media-down that fails with a reason holding escaped quotes and a backslash.
const downFakeMediaDown = `#!/usr/bin/env bash
printf '%s\n' '{"ok":false,"capability":"media.localDown","changed":false,"result":{},"error":{"code":5,"message":"compose said \"no such project\" at C:\\media"}}'
exit 5
`

// The reason in make media-down's envelope reaches make down's answer whole, escaped quotes
// and backslash included, on a machine without jq too (capability.sh's
// cap_envelope_error_message reads it; the pattern it replaced stopped at the first
// escaped quote). down.sh runs from a copy of its tree, beside a fake local-down.sh.
func TestDownReportsTheMediaServersWholeReason(t *testing.T) {
	const reason = `compose said "no such project" at C:\media`
	tree := t.TempDir()
	for rel, body := range map[string]string{
		"scripts/k3d/down.sh":         readRepoFile(t, "scripts/k3d/down.sh"),
		"scripts/lib/capability.sh":   readRepoFile(t, "scripts/lib/capability.sh"),
		"scripts/media/local-down.sh": downFakeMediaDown,
	} {
		path := filepath.Join(tree, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, noJQ := range []bool{false, true} {
		code, env, calls := downRunScript(t, filepath.Join(tree, "scripts", "k3d", "down.sh"), t.TempDir(), noJQ, nil)
		if code != 5 || env.Error == nil {
			t.Fatalf("jq absent=%v: exit %d, envelope %+v; want a code-5 failure\ncalls:\n%s", noJQ, code, env, calls)
		}
		if env.Result.MediaServerDetail != reason || !strings.Contains(env.Error.Message, reason) {
			t.Errorf("jq absent=%v: mediaServerDetail = %q and the failure says %q; want the reason %q whole",
				noJQ, env.Result.MediaServerDetail, env.Error.Message, reason)
		}
	}
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
