package k3d

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCallsMockLocalGuardAndAtomicPreservation(t *testing.T) {
	for _, tc := range []struct {
		name, context, mode, days, family string
		apply, conflict, refused          bool
	}{
		{name: "preview", context: "k3d-memql"},
		{name: "apply", context: "k3d-memql", apply: true},
		{name: "already-mock", context: "k3d-memql", mode: "bW9jaw==", apply: true},
		{name: "cloud-refused", context: "cloud", apply: true, refused: true},
		{name: "sandbox-refused", context: "k3d-memql", mode: "c2FuZGJveA==", apply: true, refused: true},
		{name: "unbounded-expiry-refused", context: "k3d-memql", days: "365", apply: true, refused: true},
		{name: "concurrent-change", context: "k3d-memql", apply: true, conflict: true, refused: true},
		{name: "family-preview", context: "k3d-memql", mode: "bW9jaw==", family: "mock"},
		{name: "family-enable-only", context: "k3d-memql", mode: "bW9jaw==", family: "mock", apply: true},
		{name: "family-disable-only", context: "k3d-memql", mode: "bW9jaw==", family: "disabled", apply: true},
		{name: "family-requires-existing-mock", context: "k3d-memql", family: "mock", apply: true, refused: true},
		{name: "family-rejects-live", context: "k3d-memql", mode: "bW9jaw==", family: "live", apply: true, refused: true},
		{name: "family-cannot-renew-envelope", context: "k3d-memql", mode: "bW9jaw==", family: "mock", days: "30", apply: true, refused: true},
		{name: "family-concurrent-change", context: "k3d-memql", mode: "bW9jaw==", family: "mock", apply: true, conflict: true, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			patchPath := filepath.Join(dir, "patch.json")
			logPath := filepath.Join(dir, "kubectl.log")
			kubectl := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$MOCK_LOG"
if [[ "$*" == "config current-context" ]]; then printf '%s' "$MOCK_CONTEXT"; exit 0; fi
[[ "$1" == "--context=k3d-memql" ]] || exit 90
shift
if [[ "$*" == *"jsonpath={.metadata.resourceVersion}"* ]]; then printf '7|Prune=false,Delete=false|IgnoreExtraneous'; exit 0; fi
if [[ "$*" == *"jsonpath={.data.MEMQL_CALLS_COMMERCE_MODE}"* ]]; then printf '%s' "$MOCK_MODE"; exit 0; fi
if [[ "$*" == "patch secret memql-secrets --namespace=memql --type=merge --patch-file=/dev/stdin" ]]; then
  cat > "$MOCK_PATCH"
  [[ "$MOCK_CONFLICT" != true ]] || { printf 'Conflict\n' >&2; exit 1; }
  printf 'secret/memql-secrets patched\n'; exit 0
fi
printf 'unexpected kubectl operation\n' >&2; exit 91
`
			require.NoError(t, os.WriteFile(filepath.Join(dir, "kubectl"), []byte(kubectl), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "k3d"), []byte("#!/usr/bin/env bash\nprintf '[{\"name\":\"memql\",\"serversRunning\":1}]'\n"), 0o755))
			args := []string{filepath.Join(repoRoot(t), "scripts", "k3d", "calls-mock.sh")}
			if tc.apply {
				args = append(args, "--apply")
			}
			if tc.days != "" {
				args = append(args, "--valid-days="+tc.days)
			}
			if tc.family != "" {
				args = append(args, "--family="+tc.family)
			}
			cmd := exec.Command("bash", args...)
			conflict := "false"
			if tc.conflict {
				conflict = "true"
			}
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "MOCK_LOG="+logPath, "MOCK_PATCH="+patchPath, "MOCK_CONTEXT="+tc.context, "MOCK_MODE="+tc.mode, "MOCK_CONFLICT="+conflict)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if tc.refused {
				require.Error(t, err, stdout.String()+stderr.String())
			} else {
				require.NoError(t, err, stdout.String()+stderr.String())
			}
			patch, readErr := os.ReadFile(patchPath)
			if !tc.apply || tc.refused && !tc.conflict {
				require.True(t, os.IsNotExist(readErr), "refusal/preview must not patch the cluster")
				return
			}
			require.NoError(t, readErr)
			var doc struct {
				Metadata struct {
					ResourceVersion string            `json:"resourceVersion"`
					Annotations     map[string]string `json:"annotations"`
				} `json:"metadata"`
				StringData map[string]string `json:"stringData"`
			}
			require.NoError(t, json.Unmarshal(patch, &doc))
			require.Equal(t, "7", doc.Metadata.ResourceVersion)
			require.Equal(t, "Prune=false,Delete=false", doc.Metadata.Annotations["argocd.argoproj.io/sync-options"])
			if tc.family != "" {
				require.Equal(t, map[string]string{"MEMQL_CALLS_FAMILY_MODE": tc.family}, doc.StringData, "family changes must not reset or renew the commerce envelope")
			} else {
				require.Equal(t, "mock", doc.StringData["MEMQL_CALLS_COMMERCE_MODE"])
				require.Len(t, doc.StringData, 15)
				for name := range doc.StringData {
					require.True(t, strings.HasPrefix(name, "MEMQL_CALLS_COMMERCE_"), "must preserve unrelated credentials and family mode")
				}
				valid, err := time.Parse(time.RFC3339, doc.StringData["MEMQL_CALLS_COMMERCE_VALID_UNTIL"])
				require.NoError(t, err)
				require.WithinDuration(t, time.Now().UTC().Add(30*24*time.Hour), valid, time.Minute)
			}
			log, err := os.ReadFile(logPath)
			require.NoError(t, err)
			require.Equal(t, 1, strings.Count(string(log), "patch secret"))
			require.NotContains(t, string(log), "deployment")
			require.NotContains(t, string(log), "rollout")
		})
	}
}
