package release

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/installation"
	"gopkg.in/yaml.v3"
)

func overlayCompositePublication(t *testing.T) pipelines.VerifiedPublishedRelease {
	t.Helper()
	base, err := overlayPublication(t).Release()
	require.NoError(t, err)
	digest := "sha256:" + strings.Repeat("8", 64)
	base.Components = append(base.Components, pipelines.PublishedComponent{Name: "product", Version: "1.2.3", Repository: "acme/install", Commit: strings.Repeat("7", 40), Artifacts: []pipelines.PublishedArtifact{{Name: "bundle", Kind: "oci", Platform: "linux/arm64", Digest: digest, ImageDigest: digest, Size: 100, Locations: []pipelines.PublishedLocation{{Kind: "oci", Origin: "https://registry.example", Repository: "memql/bundle"}}}}})
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	envelope, err := pipelines.SignPublishedRelease(base, "key", key)
	require.NoError(t, err)
	verified, err := pipelines.VerifyPublishedRelease(envelope, "fixture", map[string]ed25519.PublicKey{"key": pub})
	require.NoError(t, err)
	return verified
}
func overlayV2Configuration() overlayConfiguration {
	images := func(name string) []overlayImage {
		return []overlayImage{{Name: name, Component: "engine", Artifact: "bff", Repository: "registry.example/memql/engine"}, {Name: "bundle", Component: "product", Artifact: "bundle", Repository: "registry.example/memql/bundle"}}
	}
	return overlayConfiguration{FormatVersion: 2, InstallationID: "installation", Repository: "acme/install", OwnerUserID: "owner", CredentialID: "credential", BaseBranch: "main", ProposalWorkflow: "installationOverlayProposalWorkflow", ObservationWorkflow: "installationOverlayObservationWorkflow", Files: []overlayFile{
		{Path: "deploy/instance/kustomization.yaml", Codec: "kustomizeImages", Images: images("engine")},
		{Path: "deploy/instance/mesh/kustomization.yaml", Codec: "kustomizeGitReferences", GitReferences: []overlayGitReference{{Field: "resources", Repository: "https://github.com/acme/engine.git", Path: "deploy/k8s/base", Component: "engine"}}},
		{Path: "deploy/product/kustomization.yaml", Codec: "kustomizeImages", Images: images("bff-product")},
		{Path: "product.env", Codec: "envValue", Env: &overlayEnvValue{Key: "ENGINE_REF", Component: "engine", Value: "componentCommit"}},
	}}
}
func overlayV2Files() map[string]string {
	images := func(name string) string {
		return "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nimages:\n- name: " + name + "\n  newTag: old\n- name: bundle\n  newTag: old\n"
	}
	return map[string]string{
		"deploy/instance/kustomization.yaml":      images("engine"),
		"deploy/product/kustomization.yaml":       images("bff-product"),
		"deploy/instance/mesh/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- https://github.com/acme/engine//deploy/k8s/base?timeout=120s&ref=" + strings.Repeat("a", 40) + "&submodules=false\ncomponents:\n- https://github.com/acme/engine.git//deploy/k8s/components/capacity?ref=" + strings.Repeat("6", 40) + "&timeout=120s&submodules=false\n",
		"product.env": "# version comment stays v0.1.0\nENGINE_REF=v0.1.0\nPRODUCT=example\n",
	}
}
func overlayV2Fixture(t *testing.T) (*overlayScope, *overlayHub) {
	t.Helper()
	s, h := overlayFixture(t)
	cfg := overlayV2Configuration()
	cfg.OwnerUserID = s.owner
	s.configuration = cfg
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	s.integration.resolver.systemVariable = func(context.Context, string) (string, error) { return string(raw), nil }
	s.source = &installation.PublicationSource{InstallationID: cfg.InstallationID, RepositoryURL: "https://github.com/acme/install.git", Applications: []installation.PublicationApplication{{Key: "engine", Path: "deploy/instance"}, {Key: "product", Path: "deploy/product"}}, Publication: overlayCompositePublication(t)}
	s.request.CatalogDigest = s.source.Publication.Digest()
	h.beforeFiles = overlayV2Files()
	return s, h
}
func TestInstallationOverlayV2CodecsKeepAllPinsCoherent(t *testing.T) {
	cfg := overlayV2Configuration()
	require.NoError(t, validateOverlayConfiguration(cfg))
	before := overlayV2Files()
	publication := overlayCompositePublication(t)
	for _, file := range cfg.Files {
		output, err := buildOverlayFile([]byte(before[file.Path]), file, publication)
		require.NoError(t, err)
		switch file.Codec {
		case "kustomizeImages":
			var value map[string]any
			require.NoError(t, yaml.Unmarshal(output, &value))
			images := value["images"].([]any)
			require.Equal(t, "sha256:"+strings.Repeat("b", 64), images[0].(map[string]any)["digest"])
			require.Equal(t, "sha256:"+strings.Repeat("8", 64), images[1].(map[string]any)["digest"])
			require.NotContains(t, string(output), "newTag")
		case "kustomizeGitReferences":
			require.Contains(t, string(output), "timeout=120s&ref="+strings.Repeat("d", 40)+"&submodules=false")
			require.Contains(t, string(output), "capacity?ref="+strings.Repeat("6", 40), "unbound separately pinned dependency is unchanged")
		case "envValue":
			require.Equal(t, strings.Replace(before[file.Path], "ENGINE_REF=v0.1.0", "ENGINE_REF="+strings.Repeat("d", 40), 1), string(output))
		}
	}
	version := *cfg.Files[3].Env
	version.Value = "componentVersion"
	output, err := pinInstallationEnvValue([]byte(before["product.env"]), version, publication)
	require.NoError(t, err)
	require.Contains(t, string(output), "ENGINE_REF=v0.25.0\n")
}
func TestInstallationOverlayV2RejectsAmbiguousOrUnboundConfiguration(t *testing.T) {
	cases := map[string]func(*overlayConfiguration){
		"legacy fields":           func(c *overlayConfiguration) { c.Path = "kustomization.yaml" },
		"duplicate files":         func(c *overlayConfiguration) { c.Files[1].Path = c.Files[0].Path },
		"unsorted files":          func(c *overlayConfiguration) { c.Files[0], c.Files[1] = c.Files[1], c.Files[0] },
		"outside source":          func(c *overlayConfiguration) { c.Files[0].Path = "another/kustomization.yaml" },
		"source prefix confusion": func(c *overlayConfiguration) { c.Files[0].Path = "deploy/instance-foreign/kustomization.yaml" },
		"traversal":               func(c *overlayConfiguration) { c.Files[0].Path = "../kustomization.yaml" },
		"mixed codec":             func(c *overlayConfiguration) { c.Files[0].Env = c.Files[3].Env },
		"general patch":           func(c *overlayConfiguration) { c.Files[0].Codec = "jsonPatch" },
		"other env key":           func(c *overlayConfiguration) { c.Files[3].Env.Key = "SECRET" },
		"other env file":          func(c *overlayConfiguration) { c.Files[3].Path = "secrets.env" },
		"implicit value":          func(c *overlayConfiguration) { c.Files[3].Env.Value = "" },
		"unqualified repo": func(c *overlayConfiguration) {
			c.Files[1].GitReferences[0].Repository = "https://attacker.example/acme/engine.git"
		},
		"other field": func(c *overlayConfiguration) { c.Files[1].GitReferences[0].Field = "patches" },
	}
	for name, modify := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := overlayV2Configuration()
			modify(&cfg)
			err := validateOverlaySource(cfg, installation.PublicationSource{InstallationID: cfg.InstallationID, RepositoryURL: "https://github.com/acme/install", Applications: []installation.PublicationApplication{{Path: "deploy/instance"}, {Path: "deploy/product"}}})
			require.Error(t, err)
		})
	}
}
func TestInstallationOverlayGitReferenceCodecRefusesAmbiguity(t *testing.T) {
	cfg := overlayV2Configuration()
	file := cfg.Files[1]
	before := overlayV2Files()[file.Path]
	pub := overlayCompositePublication(t)
	for name, input := range map[string]string{
		"mutable ref":       strings.Replace(before, strings.Repeat("a", 40), "main", 1),
		"duplicate ref":     strings.Replace(before, "timeout=120s&ref=", "timeout=120s&ref="+strings.Repeat("a", 40)+"&ref=", 1),
		"duplicate slot":    strings.Replace(before, "components:", "- https://github.com/acme/engine.git//deploy/k8s/base?ref="+strings.Repeat("a", 40)+"&timeout=120s&submodules=false\ncomponents:", 1),
		"duplicate YAML":    before + "resources: []\n",
		"alias":             strings.Replace(before, "resources:", "resources: &remote", 1) + "extras: *remote\n",
		"unknown query":     strings.Replace(before, "timeout=120s", "timeout=120s&private=true", 1),
		"wrong repository":  strings.ReplaceAll(before, "acme/engine", "other/engine"),
		"missing file slot": "kind: Kustomization\nresources: [base]\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := pinInstallationGitReferences([]byte(input), file.GitReferences, pub)
			require.Error(t, err)
		})
	}
	wrong := file.GitReferences[0]
	wrong.Component = "product"
	_, err := pinInstallationGitReferences([]byte(before), []overlayGitReference{wrong}, pub)
	require.Error(t, err)
}
func TestInstallationOverlayEnvironmentCodecIsLiteralAndSingleKey(t *testing.T) {
	binding := *overlayV2Configuration().Files[3].Env
	pub := overlayCompositePublication(t)
	for _, input := range []string{"ENGINE_REF=$(run)\n", "ENGINE_REF=v1.0.0\nENGINE_REF=v1.0.1\n", "ENGINE_REF=v1.0.0\nexport ENGINE_REF=v2.0.0\n", " ENGINE_REF=v1.0.0\n", "ENGINE_REF=\"v1.0.0\"\n", "PRODUCT=example\n", "ENGINE_REF=v1.0.0\nreadonly ENGINE_REF=v2.0.0\n", "ENGINE_REF=v1.0.0\nsource another.env\n"} {
		_, err := pinInstallationEnvValue([]byte(input), binding, pub)
		require.Error(t, err)
	}
	input := "# comment\r\nENGINE_REF=v1.0.0\r\nPRODUCT=example\r\n"
	out, err := pinInstallationEnvValue([]byte(input), binding, pub)
	require.NoError(t, err)
	require.Equal(t, strings.Replace(input, "v1.0.0", strings.Repeat("d", 40), 1), string(out))
}
func TestInstallationOverlayV2AtomicIntentRecoversAcrossReplicas(t *testing.T) {
	s, h := overlayV2Fixture(t)
	ctx := ownerCtx()
	_, err := s.build(ctx, nil)
	require.NoError(t, err)
	_, err = s.propose(ctx, nil)
	require.ErrorContains(t, err, "durable")
	require.Empty(t, h.afterFiles)
	_, err = s.reserve(ctx, nil)
	require.NoError(t, err)
	saved, err := s.load(ctx, s.id)
	require.NoError(t, err)
	require.Len(t, saved.Record.Replacements, 4)
	require.Equal(t, githubapp.BranchReplacement{}, saved.Record.Replacement)
	_, err = s.propose(ctx, nil)
	require.NoError(t, err)
	require.Len(t, h.afterFiles, 4)
	require.Equal(t, 1, h.refs)
	require.Equal(t, 1, h.prs)
	replica := &Integration{resolver: s.integration.resolver, candidateDB: s.integration.candidateDB}
	replica.installation.Store(&installation.Preparer{})
	require.NoError(t, replica.ConfigureInstallationOverlay(s.integration.installationOverlay.Load().access))
	other := &overlayScope{integration: replica, db: s.db, configuration: s.configuration, request: s.request, owner: s.owner, id: s.id, snapshot: saved.Record.Snapshot, record: &saved.Record, source: s.source}
	// Lost create-ref and PR replies reconcile on another host with no local bytes.
	_, err = other.build(ctx, nil)
	require.NoError(t, err)
	_, err = other.reserve(ctx, nil)
	require.NoError(t, err)
	_, err = other.propose(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, 1, h.refs)
	require.Equal(t, 1, h.prs)
	h.merged = true
	for _, file := range saved.Record.Replacements {
		h.mergeOverrides = map[string]string{file.Path: file.Content + "# changed after review\n"}
		_, err = other.observe(ctx, nil)
		require.ErrorContains(t, err, "differs")
		current, readErr := other.load(ctx, s.id)
		require.NoError(t, readErr)
		require.Empty(t, current.MergedCommit)
	}
	h.mergeOverrides = nil
	_, err = other.observe(ctx, nil)
	require.NoError(t, err)
	current, err := other.load(ctx, s.id)
	require.NoError(t, err)
	require.Equal(t, "merged", current.summary().State)
}
func TestInstallationOverlayV2RefusesMissingOrChangedFileBeforeEffects(t *testing.T) {
	s, h := overlayV2Fixture(t)
	ctx := ownerCtx()
	missing := h.beforeFiles["product.env"]
	delete(h.beforeFiles, "product.env")
	_, err := s.build(ctx, nil)
	require.ErrorContains(t, err, "could not be read")
	require.Empty(t, s.replacements)
	require.Zero(t, h.writes)
	h.beforeFiles["product.env"] = missing
	_, err = s.build(ctx, nil)
	require.NoError(t, err)
	_, err = s.reserve(ctx, nil)
	require.NoError(t, err)
	h.beforeFiles["product.env"] += "# immutable base tampered\n"
	_, err = s.build(ctx, nil)
	require.ErrorContains(t, err, "bytes changed")
	// Even if old in-memory bytes remain, provider before hashes refuse tampering.
	_, err = s.propose(ctx, nil)
	require.Error(t, err)
	require.Zero(t, h.refs)
	require.Zero(t, h.prs)
}
func TestInstallationOverlayV2RefusesPartialBranchAndRevokedWriter(t *testing.T) {
	s, h := overlayV2Fixture(t)
	ctx := ownerCtx()
	_, err := s.build(ctx, nil)
	require.NoError(t, err)
	_, err = s.reserve(ctx, nil)
	require.NoError(t, err)
	h.branch = strings.Repeat("4", 40)
	_, err = s.propose(ctx, nil)
	require.Error(t, err)
	require.Zero(t, h.refs)
	require.Zero(t, h.prs)
	h.branch = ""
	h.revoked = true
	_, err = s.propose(ctx, nil)
	require.Error(t, err)
	require.Zero(t, h.refs)
	require.Zero(t, h.prs)
}
func TestInstallationOverlayV1KeepsHistoricalConfigurationBytes(t *testing.T) {
	legacy := `{"formatVersion":1,"installationId":"installation","repository":"acme/install","ownerUserId":"owner","credentialId":"credential","baseBranch":"main","path":"deploy/kustomization.yaml","proposalWorkflow":"proposal","observationWorkflow":"observe","images":[{"name":"engine","component":"engine","artifact":"bff","repository":"registry.example/memql/engine"}]}`
	var cfg overlayConfiguration
	require.NoError(t, decodeCandidateObject([]byte(legacy), &cfg))
	require.NoError(t, validateOverlayConfiguration(cfg))
	out, err := json.Marshal(cfg)
	require.NoError(t, err)
	require.Equal(t, legacy, string(out))
}
