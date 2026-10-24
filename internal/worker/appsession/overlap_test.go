//go:build linux || darwin

package appsession

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
)

func TestOverlappingSessionsKeepIndependentConfigs(t *testing.T) {
	for _, app := range []string{apps.IDCodex, apps.IDClaudeCode} {
		for first := range 2 {
			t.Run(fmt.Sprintf("%s/end-%d-first", app, first), func(t *testing.T) {
				ws, state := t.TempDir(), t.TempDir()
				original := []byte("person's project config")
				if err := os.WriteFile(filepath.Join(ws, ".mcp.json"), original, 0o600); err != nil {
					t.Fatal(err)
				}
				var configs []*mcpConfig
				for n := range 2 {
					m, err := writeMCPConfig(app, ws, testCodexEndpoint, fmt.Sprint("session-bearer-", n), fmt.Sprint("overlap-", n), state)
					if err != nil {
						t.Fatal(err)
					}
					configs = append(configs, m)
					t.Cleanup(m.Remove)
				}
				if configs[0].configPath == configs[1].configPath {
					t.Fatal("overlapping sessions share credential path")
				}
				configs[first].Remove()
				other := 1 - first
				if err := configs[other].Renew("remaining-session-renewed"); err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(configs[other].configPath)
				if err != nil || !strings.Contains(string(body), "remaining-session-renewed") {
					t.Fatalf("remaining config lost: %v", err)
				}
				if _, err := os.Stat(configs[first].configPath); !os.IsNotExist(err) {
					t.Fatal("finished credential survived")
				}
				body, err = os.ReadFile(filepath.Join(ws, ".mcp.json"))
				if err != nil || string(body) != string(original) {
					t.Fatal("person's project config changed")
				}
				configs[other].Remove()
				if found := grepTree(t, ws, "remaining-session-renewed"); len(found) != 0 {
					t.Fatal("credential survived cleanup", found)
				}
			})
		}
	}
}

func TestOverlappingOpenLaunchersKeepTheirOwnExitAndConfig(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "workspace with spaces")
	if err := os.MkdirAll(ws, 0o700); err != nil {
		t.Fatal(err)
	}
	spec, _ := apps.SpecFor(apps.IDClaudeCode)
	spec.Binary = fakeApp(t, "claude", "printf '%s\\n' \"$@\" > \"$MEMQL_TEST_ARGV\"\nexit \"$MEMQL_TEST_EXIT\"\n")
	var scripts, exits, argvFiles, configs []string
	for n := range 2 {
		dir := filepath.Join(ws, sessionScaffoldDir, fmt.Sprint("open-", n))
		m, err := writeMCPConfig(apps.IDClaudeCode, ws, testCodexEndpoint, testBearer, fmt.Sprint("open-", n), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Remove)
		exit, argv := filepath.Join(dir, "exit"), filepath.Join(dir, "argv")
		script, err := writeOpenLauncher(spec, ws, "--review this", []string{fmt.Sprint("MEMQL_TEST_EXIT=", n+7), "MEMQL_TEST_ARGV=" + argv}, m.configPath, exit)
		if err != nil {
			t.Fatal(err)
		}
		scripts, exits, argvFiles, configs = append(scripts, script), append(exits, exit), append(argvFiles, argv), append(configs, m.configPath)
	}
	if scripts[0] == scripts[1] {
		t.Fatal("open launchers share their script")
	}
	for n := range 2 {
		_ = exec.Command("sh", scripts[n]).Run()
		code, ok := readOpenExitFile(exits[n])
		if !ok || code != n+7 {
			t.Fatalf("session %d exit = %d, %v", n, code, ok)
		}
		argv := readArgv(t, argvFiles[n])
		if argvValue(argv, "--mcp-config") != configs[n] || !strings.Contains(strings.Join(argv, " "), "--strict-mcp-config") {
			t.Fatal("interactive door lost its private config", argv)
		}
		if len(argv) != 5 || argv[3] != "--" || argv[4] != "--review this" {
			t.Fatal("dash-leading prompt was not a positional argument", argv)
		}
	}
}

func TestFailedTranscriptRetentionKeepsOnlyItsOwnBytes(t *testing.T) {
	ws := t.TempDir()
	library := newFakeLibrary(t)
	handler := library.server.Config.Handler
	library.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer failed-session" {
			http.Error(w, "failed upload", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	})
	var sessions []*session
	var paths []string
	for n, credential := range []string{"failed-session", testBearer} {
		s := &session{id: fmt.Sprint("retention-", n), workspace: ws, library: NewLibrary(library.server.URL, credential, library.server.Client()), redact: newRedactor(credential)}
		if err := s.openTranscript(ws); err != nil {
			t.Fatal(err)
		}
		if _, err := s.transcript.WriteString(fmt.Sprint("preserved-marker-", n)); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, s.transcript.Name())
		sessions = append(sessions, s)
	}
	if _, err := sessions[0].pushOutputs(context.Background()); err == nil {
		t.Fatal("failed retention was hidden")
	}
	if _, err := sessions[1].pushOutputs(context.Background()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(paths[0])
	if err != nil || string(body) != "preserved-marker-0" {
		t.Fatal("failed session's exact transcript was lost", err)
	}
	if _, err := os.Stat(paths[1]); !os.IsNotExist(err) {
		t.Fatal("successfully retained transcript was not cleaned")
	}
}

func TestOverlappingSessionsRetainTheirOwnTranscript(t *testing.T) {
	for first := range 2 {
		t.Run(fmt.Sprint("end-", first, "-first"), func(t *testing.T) {
			ws := t.TempDir()
			library := newFakeLibrary(t)
			var sessions []*session
			for n := range 2 {
				s := &session{id: fmt.Sprint("overlap-", n), workspace: ws, library: NewLibrary(library.server.URL, testBearer, library.server.Client()), redact: newRedactor(testBearer)}
				if err := s.openTranscript(ws); err != nil {
					t.Fatal(err)
				}
				if _, err := s.transcript.WriteString(fmt.Sprint("private-session-marker-", n)); err != nil {
					t.Fatal(err)
				}
				sessions = append(sessions, s)
			}
			for _, n := range []int{first, 1 - first} {
				if _, err := sessions[n].pushOutputs(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			uploads := library.uploaded()
			for n := range 2 {
				found := false
				for name, body := range uploads {
					if strings.HasSuffix(name, fmt.Sprint("app-session-overlap-", n, "-transcript.log")) {
						found = true
						if string(body) != fmt.Sprint("private-session-marker-", n) {
							t.Fatalf("session %d retained another session's bytes", n)
						}
					}
				}
				if !found {
					t.Fatalf("session %d transcript not retained", n)
				}
			}
		})
	}
}
