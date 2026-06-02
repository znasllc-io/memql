package deploycontrol

import "testing"

const stagingOverlayFixture = `# Staging overlay -- the SINGLE image authority for aks-memql-staging.
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

namespace: memql

resources:
  - ../../base

images:
  - name: acrmemql.azurecr.io/memql-identity
    digest: sha256:5c5ced82097f8d1248081b4054a72f4f7c51265e5efde25fc07135c9cf3da5da
  - name: acrmemql.azurecr.io/memql-bff-copresent
    digest: sha256:c3e3dd89fe052842e13d563731ec9b5a4d0d46f8f5416aae6c47122ffa86fa0a
  - name: acrmemql.azurecr.io/memql-cognition
    digest: sha256:3d3eae79e9f97b2bc334410b07f340b6e279bb13b750401e5f11da8469313003
  - name: acrmemql.azurecr.io/memql-voice
    digest: sha256:bc858386a975e37ee74f212a58e6b6399b50c49c8c342ffdaf5919fdc0a6aca1
  - name: acrmemql.azurecr.io/memql-agent
    digest: sha256:36418df78e1ecd587ed1e195d8ea9ccc5a2cb07556a1ad9bc8b923dbcc00cb3c
  - name: acrmemql.azurecr.io/memql-planner
    digest: sha256:350ea124a7a57b9d9bce2bbac50ed673bcfe6c53e4355aa62d4c32a4b98f793f
  - name: acrmemql.azurecr.io/memql-workbench
    digest: sha256:45dd55a955351eb569b337802bec91706cb8929293c2407d31ee9903e0a887c0
  - name: acrmemql.azurecr.io/copresent
    digest: sha256:dcca35c767456c744491fa205c60b723a715ca261c00450326efddcf8ba8f25d
`

const prodOverlayFixture = `# Promoted from releases/0.9.9.yaml by scripts/release/promote.sh -- DIGEST COPY, no rebuild (#702).
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

namespace: memql

resources:
  - ../../base

images:
  - name: acrmemql.azurecr.io/memql-identity
    digest: sha256:5c5ced82097f8d1248081b4054a72f4f7c51265e5efde25fc07135c9cf3da5da
  - name: acrmemql.azurecr.io/copresent
    digest: sha256:dcca35c767456c744491fa205c60b723a715ca261c00450326efddcf8ba8f25d
`

func TestParseOverlayStaging(t *testing.T) {
	ov, err := ParseOverlay([]byte(stagingOverlayFixture))
	if err != nil {
		t.Fatalf("ParseOverlay: %v", err)
	}
	if ov.Namespace != "memql" {
		t.Errorf("namespace = %q, want memql", ov.Namespace)
	}
	if ov.PromotedVersion != "" {
		t.Errorf("staging PromotedVersion = %q, want empty", ov.PromotedVersion)
	}
	if len(ov.Images) != 8 {
		t.Fatalf("got %d images, want 8", len(ov.Images))
	}
	first := ov.Images[0]
	if first.Name != "acrmemql.azurecr.io/memql-identity" {
		t.Errorf("first image name = %q", first.Name)
	}
	if first.Digest != "sha256:5c5ced82097f8d1248081b4054a72f4f7c51265e5efde25fc07135c9cf3da5da" {
		t.Errorf("first image digest = %q", first.Digest)
	}
	if first.ShortName() != "memql-identity" {
		t.Errorf("ShortName = %q, want memql-identity", first.ShortName())
	}
}

func TestParseOverlayProdPromotedVersion(t *testing.T) {
	ov, err := ParseOverlay([]byte(prodOverlayFixture))
	if err != nil {
		t.Fatalf("ParseOverlay: %v", err)
	}
	if ov.PromotedVersion != "0.9.9" {
		t.Errorf("PromotedVersion = %q, want 0.9.9", ov.PromotedVersion)
	}
	if len(ov.Images) != 2 {
		t.Fatalf("got %d images, want 2", len(ov.Images))
	}
}

func TestParseOverlayShortNames(t *testing.T) {
	ov, err := ParseOverlay([]byte(stagingOverlayFixture))
	if err != nil {
		t.Fatalf("ParseOverlay: %v", err)
	}
	want := map[string]bool{
		"memql-identity": true, "memql-bff-copresent": true, "memql-cognition": true,
		"memql-voice": true, "memql-agent": true, "memql-planner": true,
		"memql-workbench": true, "copresent": true,
	}
	got := map[string]bool{}
	for _, img := range ov.Images {
		got[img.ShortName()] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("missing short name %q", name)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d distinct short names, want %d", len(got), len(want))
	}
}

func TestParseOverlayBadYAML(t *testing.T) {
	if _, err := ParseOverlay([]byte("images: [this is : not valid")); err == nil {
		t.Fatal("expected error on malformed yaml")
	}
}
