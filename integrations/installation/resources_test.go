package installation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/argocd"
	"github.com/znasllc-io/memql/integrations/argocd/gen"
	"google.golang.org/grpc"
)

const oldImage = "registry.example/memql/engine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const nextImage = "registry.example/memql/engine@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type inventoryAPI struct {
	response map[string]string
	reads    []string
	err      error
}

func (a *inventoryAPI) Do(_ context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	if method != http.MethodGet || contentType != "" || len(body) != 0 {
		return nil, errors.New("resource observation attempted a write")
	}
	a.reads = append(a.reads, path)
	if a.err != nil {
		return nil, a.err
	}
	value, ok := a.response[path]
	if !ok {
		return nil, errors.New("unexpected cluster path")
	}
	return []byte(value), nil
}

func resourceAPIFixture() *inventoryAPI {
	return &inventoryAPI{response: map[string]string{
		"apis/apps/v1": `{"groupVersion":"apps/v1","resources":[{"name":"deployments","kind":"Deployment","namespaced":true},{"name":"deployments/status","kind":"Deployment","namespaced":true}]}`,
		"api/v1":       `{"groupVersion":"v1","resources":[{"name":"configmaps","kind":"ConfigMap","namespaced":true},{"name":"secrets","kind":"Secret","namespaced":true},{"name":"namespaces","kind":"Namespace","namespaced":false}]}`,
	}}
}

type inventoryRenderer struct{ manifests []string }

func (r inventoryRenderer) Invoke(_ context.Context, _ string, in, out any, _ ...grpc.CallOption) error {
	req := in.(*gen.ManifestRequest)
	*out.(*gen.ManifestResponse) = gen.ManifestResponse{Revision: req.Revision, SourceType: "Kustomize", Manifests: r.manifests}
	return nil
}

func renderInventoryFixture(t *testing.T, revision string, manifests []string) argocd.RenderedRevision {
	t.Helper()
	spec := argocd.RenderSpec{
		Source:  json.RawMessage(`{"repoURL":"https://github.com/example/installation.git","path":"deploy/overlay","targetRevision":"` + revision + `"}`),
		AppName: "installation", AppLabelKey: "app.kubernetes.io/instance", Namespace: "memql", ProjectName: "installation", ProjectSourceRepos: []string{"https://github.com/example/installation.git"}, TrackingMethod: "annotation+label", InstallationID: "installation", KubeVersion: "1.32.13", APIVersions: []string{"apps/v1/Deployment", "v1/Secret"},
	}
	r, err := argocd.RenderRevision(context.Background(), inventoryRenderer{manifests}, spec, argocd.RepositoryCredentials{})
	require.NoError(t, err)
	return r
}

func publishedFixture(t *testing.T) pipelines.VerifiedPublishedRelease {
	t.Helper()
	digest := "sha256:" + strings.Repeat("b", 64)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	release := pipelines.PublishedRelease{FormatVersion: 1, Publisher: "fixture", CandidateID: digest, ApprovalID: "approval", WorkflowDigest: digest, ProvenanceDigest: digest, Components: []pipelines.PublishedComponent{{Name: "engine", Version: "0.25.0", Repository: "example/engine", Commit: strings.Repeat("d", 40), Artifacts: []pipelines.PublishedArtifact{{Name: "bff", Kind: "oci", Platform: "linux/arm64", Digest: digest, Size: 100, ImageDigest: digest, Locations: []pipelines.PublishedLocation{{Kind: "oci", Origin: "https://registry.example", Repository: "memql/engine"}}}}}}}
	envelope, err := pipelines.SignPublishedRelease(release, "key", key)
	require.NoError(t, err)
	verified, err := pipelines.VerifyPublishedRelease(envelope, "fixture", map[string]ed25519.PublicKey{"key": pub})
	require.NoError(t, err)
	return verified
}

func resourceManifests(image string) []string {
	return []string{
		`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"bff","namespace":"memql"},"spec":{"template":{"spec":{"containers":[{"name":"engine","image":"` + image + `"}]}}}}`,
		`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"settings","namespace":"memql"},"data":{"value":"before"}}`,
		`{"apiVersion":"v1","kind":"Secret","metadata":{"name":"credentials","namespace":"memql"},"data":{"token":"private-fixture-value"}}`,
		`{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"memql"}}`,
	}
}

func resourceBindings() []imageBinding {
	return []imageBinding{{Slot: imageSlot{Resource: resourceIdentity{"apps", "Deployment", "memql", "bff"}, Field: "containers", Container: "engine"}, Component: "engine", Artifact: "bff"}}
}

func TestResourceEvidenceBindsSignedImagesAndTheEntireDiff(t *testing.T) {
	before := renderInventoryFixture(t, strings.Repeat("a", 40), resourceManifests(oldImage))
	manifests := resourceManifests(nextImage)
	manifests[1] = strings.ReplaceAll(manifests[1], "before", "after")
	manifests = append(manifests[:2], manifests[3:]...) // deletion must remain in the proof
	manifests = append(manifests, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"new-settings"},"data":{"enabled":"true"}}`)
	after := renderInventoryFixture(t, strings.Repeat("b", 40), manifests)
	publication := publishedFixture(t)
	api := resourceAPIFixture()
	evidence, err := verifyImagesAndDiff(context.Background(), api, publication, before, after, "linux/arm64", resourceBindings())
	require.NoError(t, err)
	require.Len(t, api.reads, 2, "API versions are discovered once per bounded invocation")
	require.Len(t, evidence.changes, 4, "Deployment, ConfigMap update/create and Secret deletion are all represented")
	require.Equal(t, publication.Digest(), evidence.publication)
	require.Equal(t, before.Digest(), evidence.before)
	require.Equal(t, after.Digest(), evidence.after)
	require.NotContains(t, evidence.String(), "private-fixture-value")
	again, err := verifyImagesAndDiff(context.Background(), resourceAPIFixture(), publication, before, after, "linux/arm64", resourceBindings())
	require.NoError(t, err)
	require.Equal(t, evidence.digest, again.digest, "a second verifier reproduces the resource proof without the first's state")
	require.Equal(t, evidence.changes, again.changes)
	require.True(t, freshPreservationObservation(evidence.observed, time.Now()))
	require.True(t, freshPreservationObservation(again.observed, time.Now()))
	var deleted bool
	for _, change := range evidence.changes {
		if change.Resource.Kind == "Secret" {
			require.Equal(t, "delete", change.Operation)
			deleted = true
		}
	}
	require.True(t, deleted)
}

func TestResourceImagesRefuseUnpublishedMutableOrUnboundInputs(t *testing.T) {
	publication := publishedFixture(t)
	for _, scenario := range []struct {
		name, old, new, platform string
		bindings                 []imageBinding
	}{
		{"tag in candidate", oldImage, "registry.example/memql/engine:latest", "linux/arm64", resourceBindings()},
		{"tag in rollback", "registry.example/memql/engine:previous", nextImage, "linux/arm64", resourceBindings()},
		{"different digest", oldImage, oldImage, "linux/arm64", resourceBindings()},
		{"different repository", oldImage, strings.Replace(nextImage, "/memql/", "/another/", 1), "linux/arm64", resourceBindings()},
		{"wrong platform", oldImage, nextImage, "linux/amd64", resourceBindings()},
		{"no binding", oldImage, nextImage, "linux/arm64", nil},
		{"duplicate binding", oldImage, nextImage, "linux/arm64", append(resourceBindings(), resourceBindings()...)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			before := renderInventoryFixture(t, strings.Repeat("a", 40), resourceManifests(scenario.old))
			after := renderInventoryFixture(t, strings.Repeat("b", 40), resourceManifests(scenario.new))
			evidence, err := verifyImagesAndDiff(context.Background(), resourceAPIFixture(), publication, before, after, scenario.platform, scenario.bindings)
			require.Error(t, err)
			require.Empty(t, evidence.digest)
		})
	}
	before := renderInventoryFixture(t, strings.Repeat("a", 40), resourceManifests(oldImage))
	manifests := resourceManifests(nextImage)
	manifests[0] = strings.Replace(manifests[0], `"containers":[`, `"initContainers":[{"name":"unbound","image":"`+nextImage+`"}],"containers":[`, 1)
	after := renderInventoryFixture(t, strings.Repeat("b", 40), manifests)
	_, err := verifyImagesAndDiff(context.Background(), resourceAPIFixture(), publication, before, after, "linux/arm64", resourceBindings())
	require.ErrorContains(t, err, "no published artifact binding")
	_, err = verifyImagesAndDiff(context.Background(), resourceAPIFixture(), pipelines.VerifiedPublishedRelease{}, before, after, "linux/arm64", resourceBindings())
	require.Error(t, err, "an unsigned or caller-constructed candidate cannot qualify an image")
}

func TestResourceDiscoveryUsesActualScopeAndRejectsUnknownControllers(t *testing.T) {
	publication := publishedFixture(t)
	before := renderInventoryFixture(t, strings.Repeat("a", 40), resourceManifests(oldImage))
	after := renderInventoryFixture(t, strings.Repeat("b", 40), resourceManifests(nextImage))
	for _, fault := range []string{"missing kind", "wrong group", "wrong scope", "ambiguous kind", "private error"} {
		t.Run(fault, func(t *testing.T) {
			api := resourceAPIFixture()
			switch fault {
			case "missing kind":
				api.response["apis/apps/v1"] = `{"groupVersion":"apps/v1","resources":[]}`
			case "wrong group":
				api.response["apis/apps/v1"] = strings.Replace(api.response["apis/apps/v1"], `"apps/v1"`, `"another/v1"`, 1)
			case "wrong scope":
				api.response["apis/apps/v1"] = strings.ReplaceAll(api.response["apis/apps/v1"], `"namespaced":true`, `"namespaced":false`)
			case "ambiguous kind":
				api.response["apis/apps/v1"] = strings.Replace(api.response["apis/apps/v1"], "deployments/status", "otherdeployments", 1)
			case "private error":
				api.err = errors.New("private-cluster-diagnostic")
			}
			evidence, err := verifyImagesAndDiff(context.Background(), api, publication, before, after, "linux/arm64", resourceBindings())
			require.Error(t, err)
			require.Empty(t, evidence.digest)
			require.NotContains(t, err.Error(), "private-cluster-diagnostic")
		})
	}
	manifests := resourceManifests(nextImage)
	manifests = append(manifests, `{"apiVersion":"argoproj.io/v1alpha1","kind":"Application","metadata":{"name":"nested"},"spec":{"source":{"repoURL":"https://unverified.example/source"}}}`)
	after = renderInventoryFixture(t, strings.Repeat("b", 40), manifests)
	api := resourceAPIFixture()
	api.response["apis/argoproj.io/v1alpha1"] = `{"groupVersion":"argoproj.io/v1alpha1","resources":[{"name":"applications","kind":"Application","namespaced":true}]}`
	_, err := verifyImagesAndDiff(context.Background(), api, publication, before, after, "linux/arm64", resourceBindings())
	require.ErrorContains(t, err, "qualified installation codec")
}

func TestDatabaseDigestRetainsPostgresTagWithoutChangingArtifactIdentity(t *testing.T) {
	ref, err := immutableImage("registry.example/memql/db:16@sha256:" + strings.Repeat("a", 64))
	require.NoError(t, err)
	require.Equal(t, "registry.example/memql/db@sha256:"+strings.Repeat("a", 64), ref)
}

func TestDatabaseExtensionAndCatalogImagesCannotEscapeVerification(t *testing.T) {
	publication := publishedFixture(t)
	for _, extra := range []string{
		`"postgresql":{"extensions":[{"name":"vector","image":{"reference":"registry.example/unverified/extension:latest"}}]}`,
		`"postgresql":{"extensions":[{"name":"vector","image":{"reference":"` + nextImage + `"}}]}`,
		`"postgresql":{"extensions":[{"name":"vector"}]}`,
		`"imageCatalogRef":{"apiGroup":"postgresql.cnpg.io","kind":"ClusterImageCatalog","name":"mutable-catalog","major":18}`,
		`"newImageFeature":{"image":{"reference":"` + nextImage + `"}}`,
	} {
		for _, where := range []string{"candidate", "rollback"} {
			t.Run(where+"/"+extra, func(t *testing.T) {
				database := `{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster","metadata":{"name":"db","namespace":"memql"},"spec":{"imageName":"` + oldImage + `"}}`
				withExtra := strings.Replace(database, `"imageName":`, extra+`,"imageName":`, 1)
				old, next := append(resourceManifests(oldImage), database), append(resourceManifests(nextImage), database)
				if where == "candidate" {
					next[len(next)-1] = withExtra
				} else {
					old[len(old)-1] = withExtra
				}
				before := renderInventoryFixture(t, strings.Repeat("a", 40), old)
				after := renderInventoryFixture(t, strings.Repeat("b", 40), next)
				api := resourceAPIFixture()
				api.response["apis/postgresql.cnpg.io/v1"] = `{"groupVersion":"postgresql.cnpg.io/v1","resources":[{"name":"clusters","kind":"Cluster","namespaced":true}]}`
				evidence, err := verifyImagesAndDiff(context.Background(), api, publication, before, after, "linux/arm64", resourceBindings())
				require.ErrorContains(t, err, "qualified installation codec")
				require.Empty(t, evidence.digest)
			})
		}
	}
}

func TestResourceEvidenceBindsUnchangedObjectDiscoveryAddresses(t *testing.T) {
	publication := publishedFixture(t)
	manifests := resourceManifests(oldImage)
	// No explicit namespace: discovery determines whether this unchanged
	// object's address is namespaced or cluster-wide.
	manifests[1] = strings.ReplaceAll(manifests[1], `,"namespace":"memql"`, "")
	before := renderInventoryFixture(t, strings.Repeat("a", 40), manifests)
	manifests[0] = strings.ReplaceAll(manifests[0], oldImage, nextImage)
	after := renderInventoryFixture(t, strings.Repeat("b", 40), manifests)
	baseline, err := verifyImagesAndDiff(context.Background(), resourceAPIFixture(), publication, before, after, "linux/arm64", resourceBindings())
	require.NoError(t, err)
	for _, fault := range []string{"scope", "path"} {
		t.Run(fault, func(t *testing.T) {
			api := resourceAPIFixture()
			if fault == "scope" {
				api.response["api/v1"] = strings.ReplaceAll(api.response["api/v1"], `"kind":"ConfigMap","namespaced":true`, `"kind":"ConfigMap","namespaced":false`)
			} else {
				api.response["api/v1"] = strings.ReplaceAll(api.response["api/v1"], `"name":"configmaps"`, `"name":"otherconfigmaps"`)
			}
			changed, err := verifyImagesAndDiff(context.Background(), api, publication, before, after, "linux/arm64", resourceBindings())
			require.NoError(t, err)
			require.Equal(t, baseline.changes, changed.changes)
			require.NotEqual(t, baseline.digest, changed.digest, "the complete native inventory belongs to evidence even when the diff is identical")
		})
	}
}

func TestImageVolumesRequireImmutablePublishedBindings(t *testing.T) {
	publication := publishedFixture(t)
	before := renderInventoryFixture(t, strings.Repeat("a", 40), resourceManifests(oldImage))
	for _, scenario := range []struct {
		name, image string
		bind, pass  bool
	}{
		{"mutable volume", "registry.example/memql/engine:latest", true, false},
		{"unbound volume", nextImage, false, false},
		{"wrong artifact", oldImage, true, false},
		{"signed volume", nextImage, true, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			manifests := resourceManifests(nextImage)
			manifests[0] = strings.Replace(manifests[0], `"containers":[`, `"volumes":[{"name":"bundle","image":{"reference":"`+scenario.image+`"}}],"containers":[`, 1)
			after := renderInventoryFixture(t, strings.Repeat("b", 40), manifests)
			bindings := resourceBindings()
			if scenario.bind {
				binding := bindings[0]
				binding.Slot.Field, binding.Slot.Container = "volumes", "bundle"
				bindings = append(bindings, binding)
			}
			evidence, err := verifyImagesAndDiff(context.Background(), resourceAPIFixture(), publication, before, after, "linux/arm64", bindings)
			if scenario.pass {
				require.NoError(t, err)
				require.NotEmpty(t, evidence.digest)
			} else {
				require.Error(t, err)
				require.Empty(t, evidence.digest)
			}
		})
	}
}
