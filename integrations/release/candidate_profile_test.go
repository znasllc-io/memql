package release

import (
	"encoding/json"
	"strings"
	"testing"
)

func profileTestAuthority() candidateProfile {
	return candidateProfile{Name: "engine", Resolver: "fixtureProfile", Components: []candidateProfileComponent{{candidateVersionSource: candidateVersionSource{Component: "engine", Repository: "acme/engine", Path: "VERSION"}, Runs: []string{"build"}}},
		Registries: []candidateRegistryTarget{{ID: "registry", Component: "engine", Artifact: "image", Origin: "https://registry.example.com", Repository: "acme/engine"}}}
}

func TestCandidateProfileAuthorityBounds(t *testing.T) {
	if err := profileTestAuthority().validate(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"extra authority", "duplicate alias", "unsafe version", "static tag", "foreign files", "unknown JSON"} {
		t.Run(name, func(t *testing.T) {
			p := profileTestAuthority()
			switch name {
			case "extra authority":
				p.Components = append(p.Components, p.Components[0])
			case "duplicate alias":
				p.Components[0].Runs = []string{"build", "BUILD"}
			case "unsafe version":
				p.VersionFacts = []candidateProfileVersionFact{{Name: "sdk", Component: "engine", Path: "../package.json"}}
			case "static tag":
				p.Registries[0].Tag = "moving"
			case "foreign files":
				p.ReleaseAssets = []candidateProfileFileAuthority{{ID: "file", Component: "engine", Artifact: "docs", Repository: "other/repo", TagPrefix: "v", APIOrigin: "https://api.github.com", UploadOrigin: "https://uploads.github.com", UseGitHubApp: true}}
			case "unknown JSON":
				var got candidateProfileSelection
				if err := profileDecode(map[string]any{"components": []any{}, "publications": []any{map[string]any{"targetId": "registry", "origin": "https://unapproved.example"}}}, &got); err == nil {
					t.Fatal("resolver granted network authority")
				}
				return
			}
			if err := p.validate(); err == nil {
				t.Fatal("invalid profile authority accepted")
			}
		})
	}
}

func TestCandidateProfileDigestScopesOneRecipe(t *testing.T) {
	p := profileTestAuthority()
	first, err := profileDigest(p)
	if err != nil || !strings.HasPrefix(first, "sha256:") || len(first) != 71 {
		t.Fatal(first, err)
	}
	body, _ := json.Marshal(p)
	var again candidateProfile
	if err := decodeCandidateObject(body, &again); err != nil {
		t.Fatal(err)
	}
	second, _ := profileDigest(again)
	if first != second {
		t.Fatal("equivalent configuration changed identity")
	}
	again.Registries[0].Repository = "acme/other"
	changed, _ := profileDigest(again)
	if changed == first {
		t.Fatal("changed authority retained identity")
	}
}
