package installation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/integrations/argocd"
)

const protectedSecretPath = "api/v1/namespaces/memql/secrets/keys"

func sensitiveFixture(t *testing.T) (*inventoryAPI, argocd.RenderedRevision, argocd.RenderedRevision, []resourceIdentity) {
	t.Helper()
	manifests := func(image string) []string {
		out := resourceManifests(image)
		out[2] = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"memql"},"stringData":{"example":"fixture-value"}}`
		return out
	}
	before := renderInventoryFixture(t, strings.Repeat("a", 40), manifests(oldImage))
	after := renderInventoryFixture(t, strings.Repeat("b", 40), manifests(nextImage))
	api := resourceAPIFixture()
	api.response["api/v1/namespaces/memql"] = `{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"memql","uid":"namespace-uid","resourceVersion":"1"},"spec":{"finalizers":["kubernetes"]},"status":{"phase":"Active"}}`
	api.response["api/v1/namespaces/memql/secrets/credentials"] = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"memql","uid":"rendered-uid","resourceVersion":"1"},"type":"Opaque","data":{"example":"` + base64.StdEncoding.EncodeToString([]byte("fixture-value")) + `"}}`
	api.response[protectedSecretPath] = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"keys","namespace":"memql","uid":"secret-uid","resourceVersion":"2","annotations":{"argocd.argoproj.io/sync-options":"Prune=false","argocd.argoproj.io/compare-options":"IgnoreExtraneous","kubectl.kubernetes.io/last-applied-configuration":"sensitive-annotation-canary"}},"data":{"example":"Zml4dHVyZQ=="}}`
	return api, before, after, []resourceIdentity{{Kind: "Secret", Namespace: "memql", Name: "keys"}}
}

func editSensitiveRender(t *testing.T, rendered argocd.RenderedRevision, change func(map[string]any) bool) argocd.RenderedRevision {
	t.Helper()
	manifests := []string{}
	for _, body := range rendered.Resources() {
		value, err := resourceJSON(body)
		require.NoError(t, err)
		if !change(value) {
			continue
		}
		encoded, err := json.Marshal(value)
		require.NoError(t, err)
		manifests = append(manifests, string(encoded))
	}
	var source struct{ TargetRevision string }
	require.NoError(t, json.Unmarshal(rendered.Spec().Source, &source))
	return renderInventoryFixture(t, source.TargetRevision, manifests)
}

func TestSensitiveEvidenceReadsCredentialsWithoutRetainingValues(t *testing.T) {
	api, before, after, required := sensitiveFixture(t)
	evidence, err := verifySensitivePreservation(context.Background(), api, before, after, required)
	require.NoError(t, err)
	require.Len(t, evidence.observations, 3)
	require.NotEmpty(t, evidence.digest)
	require.Equal(t, before.Digest(), evidence.before)
	require.Equal(t, after.Digest(), evidence.after)
	encoded, err := json.Marshal(evidence.observations)
	require.NoError(t, err)
	for _, representation := range []string{string(encoded), fmt.Sprint(evidence), fmt.Sprintf("%#v", evidence)} {
		for _, forbidden := range []string{"fixture-value", "Zml4dHVyZQ==", "sensitive-annotation-canary", "last-applied"} {
			require.NotContains(t, representation, forbidden)
		}
	}
	other, _, _, _ := sensitiveFixture(t)
	again, err := verifySensitivePreservation(context.Background(), other, before, after, required)
	require.NoError(t, err)
	require.Equal(t, evidence.digest, again.digest, "a fresh replica must bind the same native resources")
	require.Equal(t, evidence.before, again.before)
	require.Equal(t, evidence.after, again.after)
	require.Equal(t, evidence.observations, again.observations)
	require.True(t, freshPreservationObservation(evidence.observed, time.Now()))
	require.True(t, freshPreservationObservation(again.observed, time.Now()))
	editStorageResponse(t, other, protectedSecretPath, func(value map[string]any) { resourceMap(value, "metadata")["uid"] = "replacement-uid" })
	replaced, err := verifySensitivePreservation(context.Background(), other, before, after, required)
	require.NoError(t, err)
	require.NotEqual(t, evidence.digest, replaced.digest, "replacement must invalidate earlier approval evidence")
}

func TestSensitiveResourcesRejectUnsafeLiveAndRenderedChanges(t *testing.T) {
	for _, fault := range []string{"missing secret", "deleting secret", "wrong namespace", "wrong UID", "missing prune protection", "contradictory prune options", "missing comparison protection", "force apply", "hook", "namespace terminating", "namespace removal", "namespace finalizers", "secret removal", "secret material", "secret type", "secret immutable", "secret owner", "live material"} {
		t.Run(fault, func(t *testing.T) {
			api, before, after, required := sensitiveFixture(t)
			switch fault {
			case "missing secret":
				delete(api.response, protectedSecretPath)
			case "deleting secret", "wrong namespace", "wrong UID", "missing prune protection", "contradictory prune options", "missing comparison protection", "force apply", "hook":
				editStorageResponse(t, api, protectedSecretPath, func(value map[string]any) {
					meta := resourceMap(value, "metadata")
					annotations := resourceMap(meta, "annotations")
					switch fault {
					case "deleting secret":
						meta["deletionTimestamp"] = "2026-10-07T09:00:00Z"
					case "wrong namespace":
						meta["namespace"] = "other"
					case "wrong UID":
						meta["uid"] = ""
					case "missing prune protection":
						delete(annotations, "argocd.argoproj.io/sync-options")
					case "contradictory prune options":
						annotations["argocd.argoproj.io/sync-options"] = "Prune=false,Prune=true"
					case "missing comparison protection":
						delete(annotations, "argocd.argoproj.io/compare-options")
					case "force apply":
						annotations["argocd.argoproj.io/sync-options"] = "Prune=false,Force=true,Replace=true"
					case "hook":
						annotations["argocd.argoproj.io/hook"] = "PostSync"
					}
				})
			case "namespace terminating":
				editStorageResponse(t, api, "api/v1/namespaces/memql", func(value map[string]any) { resourceMap(value, "status")["phase"] = "Terminating" })
			case "live material":
				editStorageResponse(t, api, "api/v1/namespaces/memql/secrets/credentials", func(value map[string]any) { resourceMap(value, "data")["example"] = "Y2hhbmdlZA==" })
			default:
				after = editSensitiveRender(t, after, func(value map[string]any) bool {
					switch resourceText(value, "kind") {
					case "Namespace":
						if fault == "namespace removal" {
							return false
						}
						if fault == "namespace finalizers" {
							value["spec"] = map[string]any{"finalizers": []any{}}
						}
					case "Secret":
						switch fault {
						case "secret removal":
							return false
						case "secret material":
							resourceMap(value, "stringData")["example"] = "changed"
						case "secret type":
							value["type"] = "another"
						case "secret immutable":
							value["immutable"] = true
						case "secret owner":
							resourceMap(value, "metadata")["ownerReferences"] = []any{}
						}
					}
					return true
				})
			}
			evidence, err := verifySensitivePreservation(context.Background(), api, before, after, required)
			require.Error(t, err)
			require.Empty(t, evidence.digest)
			require.NotContains(t, err.Error(), "fixture-value")
		})
	}
}

func TestSecretValueUsesKubernetesDataSemantics(t *testing.T) {
	a, err := secretValue(map[string]any{"data": map[string]any{"key": "b2xk"}, "stringData": map[string]any{"key": "new"}})
	require.NoError(t, err)
	b, err := secretValue(map[string]any{"type": "Opaque", "immutable": false, "data": map[string]any{"key": "bmV3"}})
	require.NoError(t, err)
	require.Equal(t, a, b)
	for _, value := range []map[string]any{
		{"type": 123}, {"immutable": "false"}, {"data": nil},
		{"data": map[string]any{"key": "not-base64"}},
		{"stringData": map[string]any{"../key": "value"}},
		{"stringData": map[string]any{"key": strings.Repeat("a", (1<<20)+1)}},
	} {
		_, err := secretValue(value)
		require.Error(t, err)
	}
}

func TestSensitiveConfigurationAndCancellationRefuseBeforeReads(t *testing.T) {
	api, before, after, required := sensitiveFixture(t)
	for _, keys := range [][]resourceIdentity{nil, append(required, required...), {{Kind: "ConfigMap", Name: "keys"}}} {
		_, err := verifySensitivePreservation(context.Background(), api, before, after, keys)
		require.Error(t, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := verifySensitivePreservation(ctx, api, before, after, required)
	require.Error(t, err)
	require.Empty(t, api.reads)
}

func TestSensitiveOwnershipIncludesFreshAncestorsAndScope(t *testing.T) {
	for _, fault := range []string{"none", "missing owner", "replaced owner", "owner removal", "owner mutation", "grandparent removal", "cluster-scoped owner", "cycle"} {
		t.Run(fault, func(t *testing.T) {
			api, before, after, required := sensitiveFixture(t)
			owner := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "settings", "uid": "owner-uid"}
			api.response["api/v1/namespaces/memql/configmaps/settings"] = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings","namespace":"memql","uid":"owner-uid","resourceVersion":"3"},"data":{"value":"before"}}`
			switch fault {
			case "missing owner":
				delete(api.response, "api/v1/namespaces/memql/configmaps/settings")
			case "replaced owner":
				owner["uid"] = "other-uid"
			case "owner removal", "owner mutation":
				after = editSensitiveRender(t, after, func(value map[string]any) bool {
					if resourceText(value, "kind") == "ConfigMap" {
						if fault == "owner removal" {
							return false
						}
						resourceMap(value, "data")["value"] = "changed"
					}
					return true
				})
			case "grandparent removal", "cycle":
				editStorageResponse(t, api, "api/v1/namespaces/memql/configmaps/settings", func(value map[string]any) {
					parent := map[string]any{"apiVersion": "v1", "kind": "Secret", "name": "credentials", "uid": "rendered-uid"}
					if fault == "cycle" {
						parent = owner
					}
					resourceMap(value, "metadata")["ownerReferences"] = []any{parent}
				})
				if fault == "grandparent removal" {
					after = editSensitiveRender(t, after, func(value map[string]any) bool { return resourceText(value, "kind") != "Secret" })
				}
			case "cluster-scoped owner":
				owner = map[string]any{"apiVersion": "v1", "kind": "Namespace", "name": "memql", "uid": "namespace-uid"}
			}
			editStorageResponse(t, api, protectedSecretPath, func(value map[string]any) { resourceMap(value, "metadata")["ownerReferences"] = []any{owner} })
			evidence, err := verifySensitivePreservation(context.Background(), api, before, after, required)
			if fault == "none" || fault == "cluster-scoped owner" {
				require.NoError(t, err)
				require.NotEmpty(t, evidence.digest)
			} else {
				require.Error(t, err)
				require.Empty(t, evidence.digest)
			}
		})
	}
}

func TestSensitiveRefusesPrunableOutOfBandNamespacesAndAncestors(t *testing.T) {
	for _, fault := range []string{"namespace", "ancestor"} {
		t.Run(fault, func(t *testing.T) {
			api, before, after, required := sensitiveFixture(t)
			omit := func(value map[string]any) bool {
				if fault == "namespace" {
					return resourceText(value, "kind") != "Namespace"
				}
				return resourceText(value, "kind") != "ConfigMap"
			}
			before, after = editSensitiveRender(t, before, omit), editSensitiveRender(t, after, omit)
			if fault == "ancestor" {
				api.response["api/v1/namespaces/memql/configmaps/settings"] = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings","namespace":"memql","uid":"owner-uid","resourceVersion":"3","annotations":{"argocd.argoproj.io/tracking-id":"installation:/ConfigMap:memql/settings"}},"data":{"value":"before"}}`
				editStorageResponse(t, api, protectedSecretPath, func(value map[string]any) {
					resourceMap(value, "metadata")["ownerReferences"] = []any{map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "settings", "uid": "owner-uid"}}
				})
			}
			evidence, err := verifySensitivePreservation(context.Background(), api, before, after, required)
			require.Error(t, err, "an out-of-band protected object may still belong to the Application's prunable live inventory")
			require.Empty(t, evidence.digest)
			path := "api/v1/namespaces/memql"
			if fault == "ancestor" {
				path += "/configmaps/settings"
			}
			editStorageResponse(t, api, path, func(value map[string]any) {
				resourceMap(value, "metadata")["annotations"] = map[string]any{"argocd.argoproj.io/sync-options": "Prune=false", "argocd.argoproj.io/compare-options": "IgnoreExtraneous"}
			})
			_, err = verifySensitivePreservation(context.Background(), api, before, after, required)
			require.NoError(t, err, "explicit protection permits stable out-of-band objects")
		})
	}
}

func TestSensitiveCertificateOwnerCannotRestoreADifferentLiveIssuer(t *testing.T) {
	api, before, after, required := sensitiveFixture(t)
	certificate := `{"apiVersion":"cert-manager.io/v1","kind":"Certificate","metadata":{"name":"identity","namespace":"memql"},"spec":{"secretName":"keys","issuerRef":{"name":"approved"}}}`
	appendCertificate := func(rendered argocd.RenderedRevision, revision string) argocd.RenderedRevision {
		manifests := []string{certificate}
		for _, body := range rendered.Resources() {
			manifests = append(manifests, string(body))
		}
		return renderInventoryFixture(t, revision, manifests)
	}
	before, after = appendCertificate(before, strings.Repeat("a", 40)), appendCertificate(after, strings.Repeat("b", 40))
	api.response["apis/cert-manager.io/v1"] = `{"groupVersion":"cert-manager.io/v1","resources":[{"name":"certificates","kind":"Certificate","namespaced":true}]}`
	path := "apis/cert-manager.io/v1/namespaces/memql/certificates/identity"
	api.response[path] = strings.Replace(certificate, `"namespace":"memql"`, `"namespace":"memql","uid":"certificate-uid","resourceVersion":"3"`, 1)
	editStorageResponse(t, api, protectedSecretPath, func(value map[string]any) {
		resourceMap(value, "metadata")["ownerReferences"] = []any{map[string]any{"apiVersion": "cert-manager.io/v1", "kind": "Certificate", "name": "identity", "uid": "certificate-uid"}}
	})
	_, err := verifySensitivePreservation(context.Background(), api, before, after, required)
	require.NoError(t, err)
	editStorageResponse(t, api, path, func(value map[string]any) {
		resourceMap(resourceMap(value, "spec"), "issuerRef")["name"] = "live-issuer"
	})
	evidence, err := verifySensitivePreservation(context.Background(), api, before, after, required)
	require.Error(t, err)
	require.Empty(t, evidence.digest)
}

func TestSensitiveRefusesUnchangedRenderedOwnerThatWouldOverwriteLiveMaterial(t *testing.T) {
	api, before, after, required := sensitiveFixture(t)
	api.response["api/v1/namespaces/memql/configmaps/settings"] = `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings","namespace":"memql","uid":"owner-uid","resourceVersion":"3"},"data":{"value":"live-changed"}}`
	editStorageResponse(t, api, protectedSecretPath, func(value map[string]any) {
		resourceMap(value, "metadata")["ownerReferences"] = []any{map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "name": "settings", "uid": "owner-uid"}}
	})
	evidence, err := verifySensitivePreservation(context.Background(), api, before, after, required)
	require.Error(t, err, "equal Git declarations can still overwrite different live owner material")
	require.Empty(t, evidence.digest)
}

// Production's database credentials are a Secret owned by an ExternalSecret
// that refreshes hourly. A refresh rewrites the owner's status, resourceVersion
// and managedFields and nothing it declares; binding them refused the 0.31.27
// plan's Apply and its completion on 2026-11-09 with no protected material
// changed. Owner evidence binds who the owner is, what it declares, what owns
// it and how Argo may prune it, so those changes still refuse. "Accepted"
// mirrors verifyProtected: a fresh read must succeed with the prepared digest.
func TestSensitiveOwnerEvidenceSurvivesControllerStatusButNotMaterialChanges(t *testing.T) {
	const ownerPath = "apis/external-secrets.io/v1/namespaces/memql/externalsecrets/keys"
	declared := `{"apiVersion":"external-secrets.io/v1","kind":"ExternalSecret","metadata":{"name":"keys","namespace":"memql","annotations":{"argocd.argoproj.io/compare-options":"IgnoreExtraneous"}},` +
		`"spec":{"refreshInterval":"1h","secretStoreRef":{"name":"keyvault","kind":"SecretStore"},"target":{"name":"keys","creationPolicy":"Owner","deletionPolicy":"Retain"},"data":[{"secretKey":"example","remoteRef":{"key":"memql-example"}}]}}`
	// The live object as the API server and the ESO controller leave it. The
	// owner reference to the Namespace exists only so a change of the owner's
	// own ownership has something to change.
	live := `{"apiVersion":"external-secrets.io/v1","kind":"ExternalSecret","metadata":{"name":"keys","namespace":"memql","uid":"external-secret-uid","resourceVersion":"40","generation":1,"creationTimestamp":"2026-11-01T00:00:00Z",` +
		`"annotations":{"argocd.argoproj.io/sync-options":"Prune=false","argocd.argoproj.io/compare-options":"IgnoreExtraneous"},` +
		`"ownerReferences":[{"apiVersion":"v1","kind":"Namespace","name":"memql","uid":"namespace-uid"}],` +
		`"managedFields":[{"manager":"external-secrets","operation":"Update","subresource":"status","time":"2026-11-09T12:33:00Z","fieldsType":"FieldsV1","fieldsV1":{"f:status":{"f:refreshTime":{}}}}]},` +
		`"spec":{"refreshInterval":"1h","secretStoreRef":{"name":"keyvault","kind":"SecretStore"},"target":{"name":"keys","creationPolicy":"Owner","deletionPolicy":"Retain"},"data":[{"secretKey":"example","remoteRef":{"key":"memql-example","conversionStrategy":"Default","decodingStrategy":"None","metadataPolicy":"None"}}]},` +
		`"status":{"binding":{"name":"keys"},"refreshTime":"2026-11-09T12:33:00Z","syncedResourceVersion":"1-0123abcd","conditions":[{"type":"Ready","status":"True","reason":"SecretSynced","message":"secret synced","lastTransitionTime":"2026-11-01T00:00:05Z"}]}}`
	editOwner := func(t *testing.T, api *inventoryAPI, change func(owner, meta map[string]any)) {
		editStorageResponse(t, api, ownerPath, func(owner map[string]any) { change(owner, resourceMap(owner, "metadata")) })
	}
	for _, test := range []struct {
		name     string
		change   func(*testing.T, *inventoryAPI)
		accepted bool
	}{
		{"hourly refresh", func(t *testing.T, api *inventoryAPI) {
			editOwner(t, api, func(owner, meta map[string]any) {
				meta["resourceVersion"] = "41"
				meta["managedFields"].([]any)[0].(map[string]any)["time"] = "2026-11-09T13:33:00Z"
				status := resourceMap(owner, "status")
				status["refreshTime"] = "2026-11-09T13:33:00Z"
				status["conditions"].([]any)[0].(map[string]any)["lastTransitionTime"] = "2026-11-09T13:33:01Z"
			})
		}, true},
		{"changed declaration", func(t *testing.T, api *inventoryAPI) {
			editOwner(t, api, func(owner, _ map[string]any) { resourceMap(owner, "spec")["refreshInterval"] = "5m" })
		}, false},
		{"replaced owner", func(t *testing.T, api *inventoryAPI) {
			editOwner(t, api, func(_, meta map[string]any) { meta["uid"] = "replacement-uid" })
			editStorageResponse(t, api, protectedSecretPath, func(secret map[string]any) {
				resourceMap(secret, "metadata")["ownerReferences"].([]any)[0].(map[string]any)["uid"] = "replacement-uid"
			})
		}, false},
		{"owner finalizers", func(t *testing.T, api *inventoryAPI) {
			editOwner(t, api, func(_, meta map[string]any) { meta["finalizers"] = []any{"example.com/hold"} })
		}, false},
		{"owner ownership", func(t *testing.T, api *inventoryAPI) {
			editOwner(t, api, func(_, meta map[string]any) {
				meta["ownerReferences"].([]any)[0].(map[string]any)["blockOwnerDeletion"] = true
			})
		}, false},
		{"prune protection", func(t *testing.T, api *inventoryAPI) {
			editOwner(t, api, func(_, meta map[string]any) {
				delete(resourceMap(meta, "annotations"), "argocd.argoproj.io/sync-options")
			})
		}, false},
	} {
		for _, rendered := range []bool{true, false} {
			name := test.name + "/out-of-band owner"
			if rendered {
				name = test.name + "/rendered owner"
			}
			t.Run(name, func(t *testing.T) {
				api, before, after, required := sensitiveFixture(t)
				if rendered {
					withOwner := func(r argocd.RenderedRevision, revision string) argocd.RenderedRevision {
						manifests := []string{declared}
						for _, body := range r.Resources() {
							manifests = append(manifests, string(body))
						}
						return renderInventoryFixture(t, revision, manifests)
					}
					before, after = withOwner(before, strings.Repeat("a", 40)), withOwner(after, strings.Repeat("b", 40))
				}
				api.response["apis/external-secrets.io/v1"] = `{"groupVersion":"external-secrets.io/v1","resources":[{"name":"externalsecrets","kind":"ExternalSecret","namespaced":true}]}`
				api.response[ownerPath] = live
				editStorageResponse(t, api, protectedSecretPath, func(secret map[string]any) {
					resourceMap(secret, "metadata")["ownerReferences"] = []any{map[string]any{"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret", "name": "keys", "uid": "external-secret-uid", "controller": true, "blockOwnerDeletion": true}}
				})
				prepared, err := verifySensitivePreservation(context.Background(), api, before, after, required)
				require.NoError(t, err)
				test.change(t, api)
				fresh, err := verifySensitivePreservation(context.Background(), api, before, after, required)
				if test.accepted {
					require.NoError(t, err)
					require.Equal(t, prepared.digest, fresh.digest, "controller bookkeeping is not protected material")
					return
				}
				require.False(t, err == nil && fresh.digest == prepared.digest, "a changed protected owner must not reuse its prepared evidence")
			})
		}
	}
}
