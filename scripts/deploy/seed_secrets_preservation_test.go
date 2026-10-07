package deploy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/deploycontrol"
	"gopkg.in/yaml.v3"
)

// Only kubectl's client-side codecs are real. Every API operation goes through
// this fixture; an empty kubeconfig also prevents accidental cluster access.
const preservationKubectl = `#!/bin/bash
set -euo pipefail
function main() {
  printf '%s\n' "$*" > "$PRESERVATION_CALLS.$$"
  case "$*" in
    'create secret generic memql-secrets '*--dry-run=client*)
      exec "$PRESERVATION_CLIENT" "$@" ;;
    'annotate --local '*) exec "$PRESERVATION_CLIENT" "$@" ;;
    'create -f -')
      cat > "$PRESERVATION_STATE"
      return ;;
    'get secret memql-secrets '*jsonpath*)
      [[ "$PRESERVATION_FAIL" != read ]] || return 1
      printf '%s' "$PRESERVATION_ANNOTATIONS"
      return ;;
    'get secret memql-secrets '*)
      [[ -f "$PRESERVATION_STATE" ]] && return 0
      printf 'Error from server (NotFound)\n' >&2
      return 1 ;;
    'annotate secret memql-secrets '*)
      [[ "$PRESERVATION_FAIL" != annotate ]] || return 1
      local args=() arg
      for arg in "$@"; do
        case "$arg" in argocd.argoproj.io/*=*) args+=("$arg");; esac
      done
      "$PRESERVATION_CLIENT" annotate --local --overwrite -f "$PRESERVATION_STATE" "${args[@]}" -o json > "$PRESERVATION_STATE.next"
      mv "$PRESERVATION_STATE.next" "$PRESERVATION_STATE"
      return ;;
    'get namespace '*|'get secret memql-db-app-creds '*) return 0 ;;
    *) printf 'unexpected cluster operation\n' >&2; return 90 ;;
  esac
}
main "$@"
`

const preservationAzure = `#!/bin/bash
set -euo pipefail
function main() {
  case "$*" in
    'account show '*) printf 'fixture-subscription\n' ;;
    'keyvault show '*) return 0 ;;
    'keyvault secret show '*--query*) printf 'postgres://fixture:fixture-password@database.invalid:5432/fixture\n' ;;
    'keyvault secret show '*) return 0 ;;
    *) printf 'unexpected vault write\n' >&2; return 90 ;;
  esac
}
main "$@"
`

type preservationSecret struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
	Metadata   struct {
		Name            string            `json:"name" yaml:"name"`
		Namespace       string            `json:"namespace" yaml:"namespace"`
		UID             string            `json:"uid,omitempty" yaml:"uid"`
		ResourceVersion string            `json:"resourceVersion,omitempty" yaml:"resourceVersion"`
		Annotations     map[string]string `json:"annotations" yaml:"annotations"`
	} `json:"metadata" yaml:"metadata"`
	Data map[string]string `json:"data" yaml:"data"`
	Type string            `json:"type" yaml:"type"`
}

func TestSeedInstanceSecretProtectionPreservesCredentials(t *testing.T) {
	client, err := exec.LookPath("kubectl")
	if err != nil {
		t.Skip("kubectl client-side codecs unavailable")
	}
	for _, tc := range []struct {
		name, fail        string
		syncOptions       string
		wantSync          string
		refused           bool
		exists, protected bool
		dry, skip         bool
	}{
		{name: "first-create"},
		{name: "repair-existing", exists: true},
		{name: "already-protected", exists: true, protected: true},
		{name: "dry-existing", exists: true, dry: true},
		{name: "dry-absent", dry: true},
		{name: "skip-cluster", exists: true, skip: true},
		{name: "read-failure", exists: true, fail: "read"},
		{name: "annotation-failure", exists: true, fail: "annotate"},
		{name: "protected-with-deletion-guard", exists: true, protected: true, syncOptions: "Prune=false,Delete=false", wantSync: "Prune=false,Delete=false"},
		{name: "retain-deletion-guard", exists: true, syncOptions: "Delete=false", wantSync: "Delete=false,Prune=false"},
		{name: "contradictory-prune", exists: true, syncOptions: "Prune=false,Prune=true", refused: true},
		{name: "duplicate-prune", exists: true, syncOptions: "Prune=false,Prune=false", refused: true},
		{name: "destructive-force", exists: true, syncOptions: "Force=true", refused: true},
		{name: "destructive-replace", exists: true, syncOptions: "Replace=true", refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			state, calls := filepath.Join(dir, "secret.json"), filepath.Join(dir, "calls")
			const fixtureValue = "fixture-encryption-key-do-not-rotate"
			original := preservationSecret{APIVersion: "v1", Kind: "Secret", Type: "Opaque", Data: map[string]string{"MEMQL_MASTER_KEY": base64.StdEncoding.EncodeToString([]byte(fixtureValue))}}
			original.Metadata.Name, original.Metadata.Namespace, original.Metadata.UID = "memql-secrets", "installation-fixture", "same-secret-uid"
			original.Metadata.ResourceVersion = "7"
			original.Metadata.Annotations = map[string]string{"example.com/retained": "unchanged"}
			if tc.protected {
				original.Metadata.Annotations["argocd.argoproj.io/sync-options"] = "Prune=false"
				original.Metadata.Annotations["argocd.argoproj.io/compare-options"] = "IgnoreExtraneous"
			}
			if tc.syncOptions != "" {
				original.Metadata.Annotations["argocd.argoproj.io/sync-options"] = tc.syncOptions
			}
			annotations := "7|" + original.Metadata.Annotations["argocd.argoproj.io/sync-options"] + "|" + original.Metadata.Annotations["argocd.argoproj.io/compare-options"]
			if tc.exists {
				body, err := json.Marshal(original)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(state, body, 0600))
			}
			for name, script := range map[string]string{"kubectl": preservationKubectl, "az": preservationAzure} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0700))
			}
			kubeconfig := filepath.Join(dir, "empty-kubeconfig")
			require.NoError(t, os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n"), 0600))
			args := []string{aksScript(t, "seed-instance-secrets.sh"), "--keyVaultName=fixture-vault", "--namespace=installation-fixture"}
			if tc.dry {
				args = append(args, "--dryRun=true")
			}
			if tc.skip {
				args = append(args, "--skipCluster=true")
			}
			cmd := exec.CommandContext(t.Context(), "bash", args...)
			cmd.Env = []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"), "HOME=" + dir, "KUBECONFIG=" + kubeconfig,
				"PRESERVATION_CLIENT=" + client, "PRESERVATION_STATE=" + state, "PRESERVATION_CALLS=" + calls,
				"PRESERVATION_ANNOTATIONS=" + annotations, "PRESERVATION_FAIL=" + tc.fail}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err = cmd.Run()
			envelope, parseErr := deploycontrol.ParseCapabilityResult(stdout.Bytes())
			require.NoError(t, parseErr, stdout.String())
			if tc.fail != "" || tc.refused {
				require.Error(t, err)
				require.False(t, envelope.OK)
				require.NotNil(t, envelope.Error)
				wantCode := 5
				if tc.refused {
					wantCode = 3
				}
				require.Equal(t, wantCode, envelope.Error.Code)
			} else {
				require.NoError(t, err, stderr.String())
				require.True(t, envelope.OK)
				require.Equal(t, !tc.dry && !tc.skip && !tc.protected, envelope.Changed)
			}
			require.NotContains(t, stdout.String()+stderr.String(), fixtureValue)
			require.NotContains(t, stdout.String()+stderr.String(), original.Data["MEMQL_MASTER_KEY"])
			paths, err := filepath.Glob(calls + ".*")
			require.NoError(t, err)
			var operations []string
			for _, path := range paths {
				body, err := os.ReadFile(path)
				require.NoError(t, err)
				operations = append(operations, strings.TrimSpace(string(body)))
			}
			if tc.skip {
				require.Empty(t, operations)
			}
			for _, operation := range operations {
				if tc.exists || tc.dry || tc.skip {
					require.False(t, strings.HasPrefix(operation, "create "), operation)
				}
				if tc.dry || tc.skip || tc.protected || tc.fail == "read" || tc.refused {
					require.False(t, strings.HasPrefix(operation, "annotate "), operation)
				}
				if strings.HasPrefix(operation, "annotate secret ") {
					require.Contains(t, operation, "--resource-version=7")
				}
			}
			body, err := os.ReadFile(state)
			if !tc.exists && tc.dry {
				require.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			var final preservationSecret
			require.NoError(t, yaml.Unmarshal(body, &final))
			require.Equal(t, "memql-secrets", final.Metadata.Name)
			require.Equal(t, "installation-fixture", final.Metadata.Namespace)
			if tc.exists {
				require.Equal(t, original.Data, final.Data)
				require.Equal(t, original.Metadata.UID, final.Metadata.UID)
				require.Equal(t, original.Type, final.Type)
				require.Equal(t, "unchanged", final.Metadata.Annotations["example.com/retained"])
			} else {
				require.Empty(t, final.Data)
			}
			if tc.fail == "" && !tc.dry && !tc.skip && !tc.refused {
				wantSync := tc.wantSync
				if wantSync == "" {
					wantSync = "Prune=false"
				}
				require.Equal(t, wantSync, final.Metadata.Annotations["argocd.argoproj.io/sync-options"])
				require.Equal(t, "IgnoreExtraneous", final.Metadata.Annotations["argocd.argoproj.io/compare-options"])
			} else {
				require.Equal(t, original.Metadata.Annotations, final.Metadata.Annotations)
			}
		})
	}
}
