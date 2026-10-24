//go:build linux || darwin

package appsession

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// The fixture binds a conversation to files in CODEX_HOME, as Codex does.
// A fake that accepts every thread/resume ID would miss losing that home.
func TestCodexHomeFixtureProcess(t *testing.T) {
	if os.Getenv("MEMQL_TEST_CODEX_HOME_FIXTURE") != "1" {
		return
	}
	state := filepath.Join(os.Getenv("CODEX_HOME"), "fixture-conversation")
	const ref = "provider-conversation-1"
	send := func(v any) { _ = json.NewEncoder(os.Stdout).Encode(v) }
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ThreadID string `json:"threadId"`
			} `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			os.Exit(3)
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"userAgent": "stateful-fixture"}
		case "thread/start":
			if err := os.WriteFile(state, []byte(ref), 0o600); err != nil {
				os.Exit(4)
			}
			result = map[string]any{"thread": map[string]any{"id": ref}}
		case "thread/resume":
			body, err := os.ReadFile(state)
			if err != nil || string(body) != req.Params.ThreadID {
				send(map[string]any{"id": req.ID, "error": map[string]any{"code": -32000, "message": "conversation not in this CODEX_HOME"}})
				continue
			}
			result = map[string]any{"thread": map[string]any{"id": ref}}
		case "turn/start":
			turn := map[string]any{"id": "turn-fixture", "items": []any{}, "itemsView": "full", "status": "inProgress"}
			send(map[string]any{"id": req.ID, "result": map[string]any{"turn": turn}})
			turn["status"] = "completed"
			send(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": ref, "turn": turn}})
			continue
		default:
			continue
		}
		send(map[string]any{"id": req.ID, "result": result})
	}
	os.Exit(0)
}

func TestSession_CodexAttachReusesProviderStateWithFreshCredential(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fakeApp(t, "codex", "case \"$*\" in *--help*) exit 0;; esac\nexport MEMQL_TEST_CODEX_HOME_FIXTURE=1\nexec "+shellQuote(binary)+" -test.run=^TestCodexHomeFixtureProcess$\n")
	h := newRig(t, apps.IDCodex)
	first := h.start(t, func(s *memqlv1.AppSessionStart) { s.App = apps.IDCodex; s.SessionId = "provider-first" })
	if first.GetError() != "" || first.GetAppSessionRef() == "" {
		t.Fatalf("first: %v", first)
	}
	if found := grepTree(t, h.workspace, testBearer); len(found) != 0 {
		t.Fatal("first bearer survived", found)
	}
	// Use a new manager too: provider state must survive a worker restart.
	next := newRig(t, apps.IDCodex)
	next.workspace = h.workspace
	second := next.start(t, func(s *memqlv1.AppSessionStart) {
		s.App, s.SessionId, s.Kind, s.AppSessionRef = apps.IDCodex, "provider-next", KindAttach, first.GetAppSessionRef()
		s.Credential = "second-credential"
	})
	if second.GetError() != "" || second.GetAppSessionRef() != first.GetAppSessionRef() {
		t.Fatalf("attach: %v", second)
	}
	if found := grepTree(t, h.workspace, "second-credential"); len(found) != 0 {
		t.Fatal("second bearer survived", found)
	}
}

func TestCodexHomeAttachExcludesActiveWriterAndRecoversAfterSweep(t *testing.T) {
	ws, state := t.TempDir(), t.TempDir()
	first, err := writeMCPConfig(apps.IDCodex, ws, testCodexEndpoint, testBearer, "first", state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.Remove)
	if err := first.rememberProviderRef("saved-ref"); err != nil {
		t.Fatal(err)
	}
	settings := sessionMCPSettings{ResumeRef: "saved-ref"}
	if other, err := writeMCPConfig(apps.IDCodex, ws, testCodexEndpoint, "second", "second", state, settings); err == nil {
		other.Remove()
		t.Fatal("two sessions acquired one conversation home")
	}
	body, err := os.ReadFile(first.configPath)
	if err != nil || !strings.Contains(string(body), testBearer) {
		t.Fatal("failed attach changed active bearer", err)
	}
	// Model process death: the kernel releases the lock, but cleanup never ran.
	first.releaseHome()
	first.releaseHome = nil
	if removed := Sweep(state); removed != 1 {
		t.Fatalf("swept %d configs", removed)
	}
	first.removed = true
	next, err := writeMCPConfig(apps.IDCodex, ws, testCodexEndpoint, "second", "second", state, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Remove()
	if next.configPath != first.configPath {
		t.Fatal("attach lost retained home")
	}
}

func TestCodexHomeLegacyAttachAndScope(t *testing.T) {
	ws := t.TempDir()
	legacy := filepath.Join(ws, sessionScaffoldDir, "codex")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := writeMCPConfig(apps.IDCodex, ws, testCodexEndpoint, testBearer, "legacy", t.TempDir(), sessionMCPSettings{ResumeRef: "legacy-ref"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Remove()
	if filepath.Dir(m.configPath) != legacy {
		t.Fatal("legacy conversation home lost")
	}
	if err := m.rememberProviderRef("legacy-ref"); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"legacy-ref", "../../outside"} {
		if other, err := writeMCPConfig(apps.IDCodex, t.TempDir(), testCodexEndpoint, testBearer, fmt.Sprint("foreign-", ref), t.TempDir(), sessionMCPSettings{ResumeRef: ref}); err == nil {
			other.Remove()
			t.Fatal("attach searched outside its workspace")
		}
	}
}
