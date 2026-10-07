package k3d

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestSeedSecretsAppliesProtectedCredentialsInOneWrite(t *testing.T) {
	var applied []byte
	key := strings.Repeat("ac", 32)
	stdout, stderr, calls, code := runSeedSecretsFull(t, scenario{
		secretState: "present", clusterKey: key, protectedSecret: &applied,
	})
	require.Zero(t, code, stdout+stderr)
	require.NotEmpty(t, applied)
	var document struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name        string            `yaml:"name"`
			Namespace   string            `yaml:"namespace"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
		Data map[string]string `yaml:"data"`
	}
	require.NoError(t, yaml.Unmarshal(applied, &document))
	require.Equal(t, "Secret", document.Kind)
	require.Equal(t, "memql-secrets", document.Metadata.Name)
	require.Equal(t, "memql", document.Metadata.Namespace)
	require.Equal(t, "Prune=false", document.Metadata.Annotations["argocd.argoproj.io/sync-options"])
	require.Equal(t, "IgnoreExtraneous", document.Metadata.Annotations["argocd.argoproj.io/compare-options"])
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte(key)), document.Data["MEMQL_MASTER_KEY"])
	require.NotContains(t, stdout+stderr, key)
	for _, call := range calls {
		require.False(t, strings.HasPrefix(call, "annotate secret "), "protection must already be present in the applied document")
	}
}
