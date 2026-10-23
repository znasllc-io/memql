package installation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/integrations/argocd"
)

func TestInstallationControllerDependenciesMustRemainUnchanged(t *testing.T) {
	for _, kind := range []string{
		"external-secrets.io/v1/ExternalSecret", "external-secrets.io/v1/SecretStore",
		"capacity.memql.io/v1alpha1/BackgroundCapacityPolicy", "kueue.x-k8s.io/v1beta2/ClusterQueue",
		"kueue.x-k8s.io/v1beta2/LocalQueue", "kueue.x-k8s.io/v1beta2/ResourceFlavor", "kueue.x-k8s.io/v1beta2/WorkloadPriorityClass",
		"admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy", "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicyBinding",
	} {
		parts := strings.Split(kind, "/")
		version := strings.Join(parts[:2], "/")
		for _, change := range []string{"unchanged", "create", "delete", "specification", "metadata", "hidden image", "unqualified version"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				api := resourceAPIFixture()
				api.response["apis/"+version] = fmt.Sprintf(`{"groupVersion":%q,"resources":[{"name":"controllers","kind":%q,"namespaced":true}]}`, version, parts[2])
				controller := fmt.Sprintf(`{"apiVersion":%q,"kind":%q,"metadata":{"name":"dependency","namespace":"memql"},"spec":{"value":"fixed"}}`, version, parts[2])
				old, next := controller, controller
				switch change {
				case "create":
					old = ""
				case "delete":
					next = ""
				case "specification":
					next = strings.Replace(next, `"fixed"`, `"changed"`, 1)
				case "metadata":
					next = strings.Replace(next, `"name":"dependency"`, `"name":"dependency","annotations":{"argocd.argoproj.io/sync-options":"Replace=true"}`, 1)
				case "hidden image":
					old = strings.Replace(old, `"value":"fixed"`, `"nested":{"image":"`+oldImage+`"}`, 1)
					next = old
				case "unqualified version":
					old = strings.Replace(old, version, parts[0]+"/v999", 1)
					next = old
					api.response["apis/"+parts[0]+"/v999"] = strings.Replace(api.response["apis/"+version], version, parts[0]+"/v999", 1)
				}
				before, after := resourceManifests(oldImage), resourceManifests(nextImage)
				if old != "" {
					before = append(before, old)
				}
				if next != "" {
					after = append(after, next)
				}
				_, err := verifyImagesAndDiff(context.Background(), api, publishedFixture(t), renderInventoryFixture(t, strings.Repeat("a", 40), before), renderInventoryFixture(t, strings.Repeat("b", 40), after), "linux/arm64", resourceBindings())
				if change == "unchanged" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			})
		}
	}
}

func TestInstallationImageBindingMustBelongToItsApplication(t *testing.T) {
	makeSet := func(revision, image string) argocd.RenderedSet {
		base := renderInventoryFixture(t, revision, resourceManifests(image))
		spec := base.Spec()
		spec.AppName = "product"
		other, err := argocd.RenderRevision(context.Background(), inventoryRenderer{[]string{`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"product","namespace":"memql"},"data":{"value":"fixed"}}`}}, spec, argocd.RepositoryCredentials{})
		require.NoError(t, err)
		set, err := argocd.CombineRenderedRevisions([]argocd.RenderedRevision{base, other})
		require.NoError(t, err)
		return set
	}
	before, after := makeSet(strings.Repeat("a", 40), oldImage), makeSet(strings.Repeat("b", 40), nextImage)
	for _, application := range []string{"installation", "product", "missing", ""} {
		t.Run(application, func(t *testing.T) {
			bindings := resourceBindings()
			bindings[0].Application = application
			_, err := verifyImagesAndDiff(context.Background(), resourceAPIFixture(), publishedFixture(t), before, after, "linux/arm64", bindings)
			if application == "installation" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "declared application")
			}
		})
	}
	// Version-one receipt encodings do not gain an empty field after upgrade.
	body, err := json.Marshal(resourceBindings()[0])
	require.NoError(t, err)
	require.NotContains(t, string(body), "application")
}
