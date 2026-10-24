package installation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/integrations/argocd"
)

func TestRenderInventoryMissingProofRefusesBeforeClusterReads(t *testing.T) {
	f := newAdmissionFixture(t)
	var revision *argocd.RenderedRevision
	var group *argocd.RenderedSet
	missing := []struct {
		name  string
		value argocd.RenderInventory
	}{{"nil", nil}, {"typed nil revision", revision}, {"typed nil set", group}, {"empty revision", argocd.RenderedRevision{}}, {"empty set", argocd.RenderedSet{}}}
	for _, test := range missing {
		t.Run(test.name, func(t *testing.T) {
			require.False(t, validRenderInventory(test.value))
			api := resourceAPIFixture()
			for _, pair := range [][2]argocd.RenderInventory{{test.value, f.after}, {f.before, test.value}} {
				_, err := newArtifactAdmission(t.Context(), api, f.config, f.old, f.next, pair[0], pair[1])
				require.Error(t, err)
				_, err = verifyStoragePreservation(t.Context(), api, pair[0], pair[1])
				require.Error(t, err)
				_, err = verifySensitivePreservation(t.Context(), api, pair[0], pair[1], []resourceIdentity{{Kind: "Secret", Namespace: "memql", Name: "keys"}})
				require.Error(t, err)
				_, err = verifyImagesAndDiff(t.Context(), api, f.next, pair[0], pair[1], "linux/arm64", resourceBindings())
				require.Error(t, err)
				require.Error(t, sameRenderInventoryScope(pair[0], pair[1]))
			}
			_, err := resourceInventory(t.Context(), api, test.value, map[string]map[string]resourceType{})
			require.Error(t, err)
			_, err = newWorkloadAdmission(t.Context(), api, test.value, artifactEvidence{})
			require.Error(t, err)
			digest := "memql-id:" + strings.Repeat("a", 64)
			evidence := workloadEvidence{digest: digest, scope: digest, render: f.before.Digest(), artifacts: digest, configuration: digest, platform: "linux/arm64", workflow: digest, operator: "operator", observed: time.Now(), expires: time.Now().Add(time.Minute)}
			require.Error(t, evidence.require(test.value, artifactEvidence{}, digest, "operator"))
			require.Empty(t, api.reads)
		})
	}
}

func TestRenderInventoryMembersPreserveNativeProofAndCannotReplaceSet(t *testing.T) {
	member := renderInventoryFixture(t, strings.Repeat("a", 40), resourceManifests(oldImage))
	set, err := argocd.CombineRenderedRevisions([]argocd.RenderedRevision{member})
	require.NoError(t, err)
	for _, inventory := range []argocd.RenderInventory{member, &member, set, &set} {
		require.True(t, validRenderInventory(inventory))
		members := inventory.RenderMembers()
		require.Len(t, members, 1)
		require.Equal(t, member.Digest(), members[0].Digest())
		members[0] = argocd.RenderedRevision{}
		require.Equal(t, member.Digest(), inventory.RenderMembers()[0].Digest())
	}
	api := resourceAPIFixture()
	_, err = resourceInventory(context.Background(), api, &member, map[string]map[string]resourceType{})
	require.NoError(t, err, "valid pointer-valued native proof remains usable")
}
