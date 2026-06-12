package memql

// Tests for the voice-session avatar-persona resolution helpers (#1428): the
// pure catalog-row selection (vendor gate + gender preference) and the
// engine-payload row normalization the resolver runs on. The resolver itself
// is a thin engine-query orchestration over these.

import (
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestPickAvatarPersonaId_VendorGate(t *testing.T) {
	rows := []map[string]any{
		{"vendor": "anam", "personaId": "anam-face-1"},
		{"vendor": "simli", "personaId": "simli-face-1"},
	}
	if got := pickAvatarPersonaId(rows, "simli", ""); got != "simli-face-1" {
		t.Fatalf("simli pick = %q, want simli-face-1", got)
	}
	if got := pickAvatarPersonaId(rows, "anam", ""); got != "anam-face-1" {
		t.Fatalf("anam pick = %q, want anam-face-1", got)
	}
	// No row for the requested vendor -> "" (audio-only), never a cross-vendor
	// id (a Simli client given an Anam id just fails the vendor REST call).
	if got := pickAvatarPersonaId(rows, "heygen", ""); got != "" {
		t.Fatalf("unknown-vendor pick = %q, want empty", got)
	}
}

func TestPickAvatarPersonaId_GenderPreference(t *testing.T) {
	rows := []map[string]any{
		{"vendor": "simli", "personaId": "simli-male", "gender": "male"},
		{"vendor": "simli", "personaId": "simli-female", "gender": "female"},
	}
	if got := pickAvatarPersonaId(rows, "simli", "female"); got != "simli-female" {
		t.Fatalf("gender pick = %q, want simli-female", got)
	}
	// Unknown gender falls back to the first qualifying row.
	if got := pickAvatarPersonaId(rows, "simli", "nonbinary"); got != "simli-male" {
		t.Fatalf("fallback pick = %q, want simli-male", got)
	}
	// No gender preference -> first qualifying row.
	if got := pickAvatarPersonaId(rows, "simli", ""); got != "simli-male" {
		t.Fatalf("no-gender pick = %q, want simli-male", got)
	}
}

func TestPickAvatarPersonaId_SkipsUnusableRows(t *testing.T) {
	rows := []map[string]any{
		{"vendor": "simli", "personaId": "   "},      // blank id
		{"vendor": "SIMLI", "personaId": "kept-one"}, // vendor case-insensitive
	}
	if got := pickAvatarPersonaId(rows, "simli", ""); got != "kept-one" {
		t.Fatalf("pick = %q, want kept-one", got)
	}
	if got := pickAvatarPersonaId(nil, "simli", ""); got != "" {
		t.Fatalf("nil rows pick = %q, want empty", got)
	}
}

// TestPickAvatarPersonaId_NestedPayload covers the shape()-style rows where
// the fields nest under "payload".
func TestPickAvatarPersonaId_NestedPayload(t *testing.T) {
	rows := []map[string]any{
		{"id": "v1:agents:avatarPersona:sofia", "payload": map[string]any{
			"vendor": "simli", "personaId": "sofia-face", "gender": "female",
		}},
	}
	if got := pickAvatarPersonaId(rows, "simli", "female"); got != "sofia-face" {
		t.Fatalf("nested pick = %q, want sofia-face", got)
	}
}

// TestRowsFromEnginePayload_GraphBundle: named queries can surface as a
// GraphBundle; the normalizer must flatten node payloads into row maps so the
// picker sees the vendor/personaId fields.
func TestRowsFromEnginePayload_GraphBundle(t *testing.T) {
	payload, err := structpb.NewStruct(map[string]any{
		"vendor":    "simli",
		"personaId": "sofia-face",
	})
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	bundle := &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
		nil, // nil nodes must be skipped, not panic
		{Id: "v1:agents:avatarPersona:sofia", Concept: "v1:agents:avatarPersona", Payload: payload},
	}}
	rows := rowsFromEnginePayload(bundle)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if got := pickAvatarPersonaId(rows, "simli", ""); got != "sofia-face" {
		t.Fatalf("bundle pick = %q, want sofia-face", got)
	}
}

// TestRowsFromEnginePayload_PlainShapes: the non-bundle shapes defer to
// normalizeResultRows.
func TestRowsFromEnginePayload_PlainShapes(t *testing.T) {
	if rows := rowsFromEnginePayload(nil); rows != nil {
		t.Fatalf("nil payload rows = %v, want nil", rows)
	}
	rows := rowsFromEnginePayload([]any{
		map[string]any{"vendor": "simli", "personaId": "x"},
		"not-a-map",
	})
	if len(rows) != 1 || rows[0]["personaId"] != "x" {
		t.Fatalf("list rows = %v, want one map with personaId x", rows)
	}
}
