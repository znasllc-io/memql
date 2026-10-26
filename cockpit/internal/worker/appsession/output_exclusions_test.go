package appsession

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Browser user-data directories can have arbitrary task-chosen names. Their
// runtime files must not displace the screenshots and receipts beside them.
func TestCollectProducedExcludesBrowserRuntime(t *testing.T) {
	for _, marker := range []string{"Default/Preferences", "Profile 2/Preferences", "DevToolsActivePort"} {
		t.Run(marker, func(t *testing.T) {
			ws := t.TempDir()
			write := func(rel string) string {
				t.Helper()
				path := filepath.Join(ws, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				return path
			}
			before := snapshotWorkspace(ws)
			write("proof/runtime-17/Local State")
			write("proof/runtime-17/" + marker)
			cache := write("proof/runtime-17/Default/Cache/data")
			write("proof/screenshot.png")
			write("proof/results.json")
			// A coincidental filename alone is not a browser profile.
			write("documentation/Local State")
			files, skipped := collectProduced(ws, before, map[string]bool{cache: true})
			if len(files) != 3 || len(skipped) != 0 {
				t.Fatalf("browser runtime entered output selection: files=%v skipped=%v", files, skipped)
			}
			for rel := range snapshotWorkspace(ws) {
				if strings.HasPrefix(filepath.ToSlash(rel), "proof/runtime-17/") {
					t.Fatalf("browser runtime entered the next session snapshot: %s", rel)
				}
			}
			info, err := os.Stat(cache)
			if err != nil {
				t.Fatal("excluding runtime must not delete it", err)
			}
			candidate := producedFile{path: cache, rel: "proof/runtime-17/Default/Cache/data", size: info.Size(), info: info}
			body, digest, held, err := newContentPolicy(ws, newRedactor(testBearer)).readOutput(candidate)
			if err == nil || body != nil || digest != "" || held {
				t.Fatalf("direct output upload escaped runtime exclusion: bytes=%d digest=%q held=%t err=%v", len(body), digest, held, err)
			}
		})
	}
}
