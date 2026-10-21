package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// service.env carries the account's half of the bundled worker service, and
// only the names the installer and the owner may set.
func TestServiceEnvSetsOnlyItsOwnNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), serviceEnvFile)
	body := "# written by activate-app.sh\nOLLAMA_HOST=127.0.0.1:11500\n\nOLLAMA_MODELS=/Volumes/Models\nPATH=/opt/tools/bin:/usr/bin\nMEMQL_WORKER_TOKEN=mql_wkr_never\nDYLD_INSERT_LIBRARIES=/tmp/x.dylib\nnot a pair\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := readServiceEnv(path)
	want := map[string]string{"OLLAMA_HOST": "127.0.0.1:11500", "OLLAMA_MODELS": "/Volumes/Models", "PATH": "/opt/tools/bin:/usr/bin"}
	if len(got) != len(want) {
		t.Fatalf("readServiceEnv = %v, want %v", got, want)
	}
	for name, value := range want {
		if got[name] != value {
			t.Errorf("%s = %q, want %q", name, got[name], value)
		}
	}
	if readServiceEnv(filepath.Join(t.TempDir(), "missing")) != nil {
		t.Error("a missing service.env must set nothing")
	}
	link := filepath.Join(t.TempDir(), "link.env")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if readServiceEnv(link) != nil {
		t.Error("a link in place of service.env must set nothing")
	}
}

// The plists MemQL.app registers and the names `worker service` asks
// SMAppService for are one contract: a renamed file is a service macOS
// cannot find, reported only on the machine that installs it.
func TestBundledAgentPlistsMatchTheServiceNames(t *testing.T) {
	agents := filepath.Join("..", "..", "native", "macos", "agents")
	for plist, wants := range map[string][]string{
		workerAgentPlist: {
			"<string>com.znasllc.memql-worker</string>",
			"<key>BundleProgram</key>\n\t<string>Contents/MacOS/MemQL</string>",
			"<key>" + appServiceVariable + "</key>\n\t\t<string>app</string>",
		},
		menuAgentPlist: {
			"<string>com.znasllc.memql-cockpit-menubar</string>",
			"<key>BundleProgram</key>\n\t<string>Contents/Library/LoginItems/MemQL Menu.app/Contents/MacOS/MemQLCockpit</string>",
		},
	} {
		body, err := os.ReadFile(filepath.Join(agents, plist))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range wants {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s lacks %q", plist, want)
			}
		}
	}
	build, err := os.ReadFile(filepath.Join("..", "..", "scripts", "macos", "build-app.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(build), "native/macos/agents/\"com.znasllc.memql-{worker,cockpit-menubar}.plist") {
		t.Error("build-app.sh no longer bundles both agent plists")
	}
}
