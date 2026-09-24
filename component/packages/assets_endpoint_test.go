package packages

import (
	"context"
	"encoding/json"
	"testing"
	"testing/fstest"
)

func TestPackageAnalyzeEndpointValidatesAssetRepositoryBeforeReview(t *testing.T) {
	for _, scenario := range []struct {
		name, sourceKind, repository string
		wantOK                       bool
	}{
		{"same repository", "repo", "https://github.com/acme/widget", true},
		{"foreign repository", "repo", "https://github.com/another/project", false},
		{"artifact source", "artifact", "", false},
		{"artifact with leftover repository", "artifact", "https://github.com/acme/widget", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			asset := assetFixture([]byte("movie"))
			manifest := Manifest{FormatVersion: 1, Name: "acme", Deployables: []ManifestDeployable{{Name: "web", Path: "web", Kind: KindStatic, Assets: []ManifestAsset{asset}}}}
			encoded, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			tree := fstest.MapFS{ManifestName: &fstest.MapFile{Data: encoded}, "web/package.json": file(`{}`)}
			pkg := ownerPackage()
			pkg["sourceKind"], pkg["repoUrl"] = scenario.sourceKind, scenario.repository
			h := newHarness(t, tree, pkg)
			importer := &recordingAssetImporter{raw: []byte("movie")}
			h.deps.Assets = importer
			integration := NewIntegration(h.engine, discardLogger())
			integration.depsOnce.Do(func() { integration.deps = h.deps })
			rows, err := integration.handleAnalyze(context.Background(), map[string]any{"packageId": "pkg"}, 0)
			if scenario.wantOK && err != nil {
				t.Fatalf("valid source refused: %v", err)
			}
			if !scenario.wantOK && RefusalCode(err) != CodeManifestInvalid {
				t.Fatalf("invalid source accepted: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("missing report: %+v", rows)
			}
			var response struct {
				OK     bool    `json:"ok"`
				Report *Report `json:"report"`
			}
			if err := json.Unmarshal(rows[0].Payload, &response); err != nil {
				t.Fatal(err)
			}
			if response.OK != scenario.wantOK || response.Report == nil || response.Report.OK != scenario.wantOK {
				t.Fatalf("report verdict differs: %+v", response)
			}
			if !scenario.wantOK && (response.Report.FirstFatal() == nil || response.Report.FirstFatal().Code != CodeManifestInvalid) {
				t.Fatal("report omitted repository refusal")
			}
			if importer.calls != 0 || len(h.builder.built) != 0 || len(h.publisher.published) != 0 {
				t.Fatal("analysis fetched assets, built or published")
			}
		})
	}
}
