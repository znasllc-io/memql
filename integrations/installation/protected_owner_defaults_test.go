package installation

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProtectedExternalSecretDefaultsDoNotHideMaterialDrift(t *testing.T) {
	desired, err := resourceJSON([]byte(`{"apiVersion":"external-secrets.io/v1","kind":"ExternalSecret","metadata":{"name":"database","namespace":"memql"},"spec":{"refreshInterval":"1h","secretStoreRef":{"name":"vault","kind":"SecretStore"},"target":{"name":"database-credentials","template":{"type":"kubernetes.io/basic-auth"}},"data":[{"secretKey":"password","remoteRef":{"key":"database-password"}}]}}`))
	require.NoError(t, err)
	original, _ := json.Marshal(desired)
	for _, drift := range []string{"defaults", "vault key", "target name", "secret type", "deletion policy", "decoding policy", "null-byte policy", "unknown field", "unknown API"} {
		t.Run(drift, func(t *testing.T) {
			live, err := resourceJSON(original)
			require.NoError(t, err)
			spec := resourceMap(live, "spec")
			target := resourceMap(spec, "target")
			remote := resourceMap(spec["data"].([]any)[0].(map[string]any), "remoteRef")
			spec["refreshInterval"] = "1h0m0s"
			target["creationPolicy"], target["deletionPolicy"] = "Owner", "Retain"
			template := resourceMap(target, "template")
			template["engineVersion"], template["mergePolicy"] = "v2", "Replace"
			remote["conversionStrategy"], remote["decodingStrategy"], remote["metadataPolicy"], remote["nullBytePolicy"] = "Default", "None", "None", "Ignore"
			switch drift {
			case "vault key":
				remote["key"] = "different-password"
			case "target name":
				target["name"] = "different-secret"
			case "secret type":
				template["type"] = "Opaque"
			case "deletion policy":
				target["deletionPolicy"] = "Delete"
			case "decoding policy":
				remote["decodingStrategy"] = "Base64"
			case "null-byte policy":
				remote["nullBytePolicy"] = "Reject"
			case "unknown field":
				spec["unknownDefault"] = true
			case "unknown API":
				live["apiVersion"] = "external-secrets.io/v999"
			}
			require.Equal(t, drift == "defaults", protectedOwnerMatchesLive(desired, live))
			unchanged, _ := json.Marshal(desired)
			require.Equal(t, string(original), string(unchanged), "normalization must not edit the approved inventory")
		})
	}
}

func TestProtectedSecretStoreDefaultsKeepAuthorityVisible(t *testing.T) {
	desired, err := resourceJSON([]byte(`{"apiVersion":"external-secrets.io/v1","kind":"SecretStore","metadata":{"name":"vault"},"spec":{"provider":{"azurekv":{"authType":"WorkloadIdentity","vaultUrl":"https://example.vault.azure.net","serviceAccountRef":{"name":"vault-reader"}}}}}`))
	require.NoError(t, err)
	encoded, _ := json.Marshal(desired)
	for _, drift := range []string{"defaults", "vault", "identity", "SDK", "unknown provider default"} {
		t.Run(drift, func(t *testing.T) {
			live, err := resourceJSON(encoded)
			require.NoError(t, err)
			azure := resourceMap(resourceMap(resourceMap(live, "spec"), "provider"), "azurekv")
			azure["environmentType"], azure["useAzureSDK"] = "PublicCloud", false
			switch drift {
			case "vault":
				azure["vaultUrl"] = "https://other.vault.azure.net"
			case "identity":
				resourceMap(azure, "serviceAccountRef")["name"] = "other-reader"
			case "SDK":
				azure["useAzureSDK"] = true
			case "unknown provider default":
				azure["futureOption"] = true
			}
			require.Equal(t, drift == "defaults", protectedOwnerMatchesLive(desired, live))
		})
	}
}
