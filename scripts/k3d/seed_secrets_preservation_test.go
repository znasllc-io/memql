package k3d

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestSeedSecretsAppliesProtectedCredentialsInOneWrite(t *testing.T) {
	for _, tc := range []struct {
		name, options, want string
		refused             bool
	}{
		{name: "missing", want: "Prune=false"},
		{name: "retain-deletion-guard", options: "Delete=false", want: "Delete=false,Prune=false"},
		{name: "already-protected", options: "Prune=false,Delete=false", want: "Prune=false,Delete=false"},
		{name: "contradictory-prune", options: "Prune=false,Prune=true", refused: true},
		{name: "duplicate-prune", options: "Prune=false,Prune=false", refused: true},
		{name: "destructive-force", options: "Force=true", refused: true},
		{name: "destructive-replace", options: "Replace=true", refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var applied []byte
			key := strings.Repeat("ac", 32)
			stdout, stderr, calls, code := runSeedSecretsFull(t, scenario{
				secretState: "present", clusterKey: key, protectedSecret: &applied,
				syncOptions: tc.options, compareOptions: "IgnoreExtraneous",
			})
			if tc.refused {
				require.Equal(t, 3, code, stdout+stderr)
				require.Empty(t, applied)
				for _, call := range calls {
					for _, mutation := range []string{"create ", "apply ", "annotate ", "patch "} {
						require.False(t, strings.HasPrefix(call, mutation), "refused metadata must prevent seeding: "+call)
					}
				}
				return
			}
			require.Zero(t, code, stdout+stderr)
			require.NotEmpty(t, applied)
			var document struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					ResourceVersion string            `yaml:"resourceVersion"`
					Name            string            `yaml:"name"`
					Namespace       string            `yaml:"namespace"`
					Annotations     map[string]string `yaml:"annotations"`
				} `yaml:"metadata"`
				Data map[string]string `yaml:"data"`
			}
			require.NoError(t, yaml.Unmarshal(applied, &document))
			require.Equal(t, "Secret", document.Kind)
			require.Equal(t, "memql-secrets", document.Metadata.Name)
			require.Equal(t, "memql", document.Metadata.Namespace)
			require.Equal(t, "7", document.Metadata.ResourceVersion)
			require.Equal(t, tc.want, document.Metadata.Annotations["argocd.argoproj.io/sync-options"])
			require.Equal(t, "IgnoreExtraneous", document.Metadata.Annotations["argocd.argoproj.io/compare-options"])
			require.Equal(t, base64.StdEncoding.EncodeToString([]byte(key)), document.Data["MEMQL_MASTER_KEY"])
			require.NotContains(t, stdout+stderr, key)
			patches := 0
			for _, call := range calls {
				require.False(t, strings.HasPrefix(call, "annotate secret "), "protection must already be present in the applied document")
				if strings.HasPrefix(call, "patch secret memql-secrets ") {
					require.Contains(t, call, "--type=merge --patch-file=/dev/stdin")
					patches++
				}
			}
			require.Equal(t, 1, patches, "apply can omit an unchanged resourceVersion; retain an explicit atomic precondition")

		})
	}
}
