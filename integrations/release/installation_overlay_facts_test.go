package release

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/integrations/installation"
)

func overlayFactsFixture() overlayScalarFacts {
	return overlayScalarFacts{Version: "1.2.3", Commit: strings.Repeat("a", 40), CompanionDigest: "sha256:" + strings.Repeat("b", 64), CompanionSize: 8123}
}

func TestInstallationOverlayJSONFactsPreserveUnboundValues(t *testing.T) {
	before := []byte(`{"version":"1.2.2","commit":"old","sha256":"old","size":1,"keep":{"integer":9007199254740993,"array":[true,null,{"secretRef":"unchanged"}]}}`)
	bindings := []overlayJSONScalar{{"/version", "componentVersion"}, {"/commit", "componentCommit"}, {"/sha256", "companionSHA256"}, {"/size", "companionSize"}}
	after, err := pinOverlayJSONScalars(before, bindings, overlayFactsFixture())
	require.NoError(t, err)
	require.JSONEq(t, `{"version":"1.2.3","commit":"`+strings.Repeat("a", 40)+`","sha256":"`+strings.Repeat("b", 64)+`","size":8123,"keep":{"integer":9007199254740993,"array":[true,null,{"secretRef":"unchanged"}]}}`, string(after))
	require.Contains(t, string(after), "9007199254740993", "unbound numbers cannot be rounded through float64")
	repeated, err := pinOverlayJSONScalars(after, bindings, overlayFactsFixture())
	require.NoError(t, err)
	require.Equal(t, after, repeated)
	_, err = pinOverlayJSONScalars([]byte(`{"a/b":{"c~d":"old"}}`), []overlayJSONScalar{{"/a~1b/c~0d", "componentVersion"}}, overlayFactsFixture())
	require.NoError(t, err, "JSON object-key escapes remain literal")
}

func TestInstallationOverlayJSONFactsRefuseUnboundEdits(t *testing.T) {
	for name, input := range map[string]string{
		"duplicate key":   `{"version":"1","version":"2"}`,
		"case duplicate":  `{"version":"1","Version":"2"}`,
		"missing slot":    `{}`,
		"object slot":     `{"version":{"value":"old"}}`,
		"array slot":      `{"version":["old"]}`,
		"null slot":       `{"version":null}`,
		"numeric slot":    `{"version":1}`,
		"trailing object": `{"version":"old"} {}`,
		"root array":      `[{"version":"old"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := pinOverlayJSONScalars([]byte(input), []overlayJSONScalar{{"/version", "componentVersion"}}, overlayFactsFixture())
			require.Error(t, err)
		})
	}
	for name, bindings := range map[string][]overlayJSONScalar{
		"arbitrary fact":   {{"/version", "caller.value"}},
		"duplicate":        {{"/version", "componentVersion"}, {"/version", "componentCommit"}},
		"ancestor":         {{"/version", "componentVersion"}, {"/version/nested", "componentCommit"}},
		"descendant":       {{"/version/nested", "componentCommit"}, {"/version", "componentVersion"}},
		"relative pointer": {{"version", "componentVersion"}},
		"invalid escape":   {{"/ver~2sion", "componentVersion"}},
		"empty key":        {{"/version/", "componentVersion"}},
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, validateOverlayJSONScalars(bindings))
		})
	}
	for _, size := range []string{`"1"`, "true", "null", "-1", "1.2", "9223372036854775808"} {
		_, err := pinOverlayJSONScalars([]byte(`{"size":`+size+`}`), []overlayJSONScalar{{"/size", "companionSize"}}, overlayFactsFixture())
		require.Error(t, err)
	}
	for name, mutate := range map[string]func(*overlayScalarFacts){
		"mutable source":    func(f *overlayScalarFacts) { f.Commit = "main" },
		"version traversal": func(f *overlayScalarFacts) { f.Version = "../1.2.3" },
		"unsigned digest":   func(f *overlayScalarFacts) { f.CompanionDigest = "unverified" },
		"empty file":        func(f *overlayScalarFacts) { f.CompanionSize = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			facts := overlayFactsFixture()
			mutate(&facts)
			_, err := pinOverlayJSONScalars([]byte(`{"version":"old"}`), []overlayJSONScalar{{"/version", "componentVersion"}}, facts)
			require.Error(t, err)
		})
	}
}

func overlayV3Configuration() overlayConfiguration {
	c := overlayV2Configuration()
	c.FormatVersion = 3
	c.NativeEvidence = &overlayNativeConfiguration{Component: "engine", CompanionComponent: "engine", CompanionArtifact: "docs", Images: []overlayImage{{Name: "bff", Component: "engine", Artifact: "bff", Repository: "registry.example/memql/engine"}}}
	c.Files = append([]overlayFile{{Path: "clients/docs-release.json", Codec: "jsonScalars", JSONScalars: []overlayJSONScalar{{"/version", "componentVersion"}, {"/sha256", "companionSHA256"}, {"/size", "companionSize"}}}}, c.Files...)
	c.Files = append(c.Files[:len(c.Files)-1], overlayFile{Path: "deploy/releases/component-{version}.json", Codec: "nativeManifest"}, c.Files[len(c.Files)-1])
	return c
}

func TestInstallationOverlayV3RequiresExplicitAuxiliaryBindings(t *testing.T) {
	c := overlayV3Configuration()
	source := installation.PublicationSource{InstallationID: c.InstallationID, RepositoryURL: "https://github.com/acme/install", Applications: []installation.PublicationApplication{{Path: "deploy/instance"}, {Path: "deploy/product"}}}
	require.NoError(t, validateOverlaySource(c, source))
	for name, modify := range map[string]func(*overlayConfiguration){
		"legacy format":           func(c *overlayConfiguration) { c.FormatVersion = 2 },
		"missing native binding":  func(c *overlayConfiguration) { c.NativeEvidence = nil },
		"missing companion":       func(c *overlayConfiguration) { c.NativeEvidence.CompanionArtifact = "" },
		"arbitrary file codec":    func(c *overlayConfiguration) { c.Files[0].Codec = "jsonPatch" },
		"mixed image authority":   func(c *overlayConfiguration) { c.Files[0].Images = c.Files[1].Images },
		"new script":              func(c *overlayConfiguration) { c.Files[0].Path = "clients/run.sh" },
		"unversioned evidence":    func(c *overlayConfiguration) { c.Files[4].Path = "deploy/releases/component.json" },
		"ambiguous evidence path": func(c *overlayConfiguration) { c.Files[4].Path = "deploy/releases/{version}-{version}.json" },
	} {
		t.Run(name, func(t *testing.T) {
			c := overlayV3Configuration()
			modify(&c)
			require.Error(t, validateOverlaySource(c, source))
		})
	}
	// Adding opt-in fields must not change any legacy configuration identity.
	legacy := overlayV2Configuration()
	encoded, err := json.Marshal(legacy)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "nativeEvidence")
	require.NotContains(t, string(encoded), "jsonScalars")
	for _, file := range legacy.Files {
		require.NoError(t, validateOverlaySource(legacy, source))
		require.Empty(t, file.JSONScalars)
	}
}

// Notifications and published docs intentionally use different version forms.
func TestInstallationOverlayJSONFactsDistinguishTagFromVersion(t *testing.T) {
	facts := overlayFactsFixture()
	notifications, err := pinOverlayJSONScalars([]byte(`{"version":"v1.2.2","probes":[{"status":200}],"instance":"unchanged"}`), []overlayJSONScalar{{"/version", "componentTag"}}, facts)
	require.NoError(t, err)
	require.JSONEq(t, `{"version":"v1.2.3","probes":[{"status":200}],"instance":"unchanged"}`, string(notifications))
	docs, err := pinOverlayJSONScalars([]byte(`{"schema":1,"version":"1.2.2"}`), []overlayJSONScalar{{"/version", "componentVersion"}}, facts)
	require.NoError(t, err)
	require.JSONEq(t, `{"schema":1,"version":"1.2.3"}`, string(docs))
}
