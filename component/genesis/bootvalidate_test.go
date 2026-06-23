package genesis

import (
	"reflect"
	"testing"
)

// lookupFromMap builds an os.LookupEnv-shaped function over a map.
func lookupFromMap(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

func TestResolveNodeType(t *testing.T) {
	t.Setenv("MEMQL_NODE_TYPE", "")
	if got := ResolveNodeType(); got != DefaultNodeType {
		t.Errorf("unset -> %q, want %q", got, DefaultNodeType)
	}
	t.Setenv("MEMQL_NODE_TYPE", "  voice  ")
	if got := ResolveNodeType(); got != "voice" {
		t.Errorf("trimmed -> %q, want voice", got)
	}
}

func TestMissingRequired(t *testing.T) {
	// A hand-built manifest exercising every axis: an "all" var, two
	// node-specific vars, a defaulted "all" var, and an unrelated
	// optional var that no node requires.
	m := &Manifest{
		Secrets: []ManifestEntry{
			{Name: "DB_DSN", Required: []string{"all"}},
			{Name: "LIVEKIT_URL", Required: []string{"voice"}},
			{Name: "LIVEKIT_KEY", Required: []string{"voice"}},
		},
		Variables: []ManifestEntry{
			{Name: "GRPC_ADDR", Required: []string{"all"}, Default: ":50051"},
			{Name: "UNRELATED"},
		},
	}

	cases := []struct {
		name     string
		nodeType string
		env      map[string]string
		want     []string
	}{
		{
			name:     "bff all present",
			nodeType: "bff",
			env:      map[string]string{"DB_DSN": "postgres://x"},
			want:     nil, // GRPC_ADDR satisfied by its default
		},
		{
			name:     "bff missing all-var",
			nodeType: "bff",
			env:      map[string]string{},
			want:     []string{"DB_DSN"}, // GRPC_ADDR defaulted, LiveKit not required for bff
		},
		{
			name:     "voice missing its node-specific vars",
			nodeType: "voice",
			env:      map[string]string{"DB_DSN": "postgres://x"},
			want:     []string{"LIVEKIT_URL", "LIVEKIT_KEY"},
		},
		{
			name:     "voice fully satisfied",
			nodeType: "voice",
			env: map[string]string{
				"DB_DSN":      "postgres://x",
				"LIVEKIT_URL": "wss://lk",
				"LIVEKIT_KEY": "k",
			},
			want: nil,
		},
		{
			name:     "empty-string value counts as missing",
			nodeType: "bff",
			env:      map[string]string{"DB_DSN": "   "},
			want:     []string{"DB_DSN"},
		},
		{
			name:     "node-specific var not required by other node",
			nodeType: "identity",
			env:      map[string]string{"DB_DSN": "postgres://x"},
			want:     nil, // "all" covers identity too; LiveKit is voice-only
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := MissingRequired(c.nodeType, m, lookupFromMap(c.env))
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("MissingRequired(%q) = %v, want %v", c.nodeType, got, c.want)
			}
		})
	}
}

func TestMissingRequired_NilManifest(t *testing.T) {
	if got := MissingRequired("bff", nil, lookupFromMap(nil)); got != nil {
		t.Errorf("nil manifest -> %v, want nil", got)
	}
}

func TestMissingRequiredError(t *testing.T) {
	if got := MissingRequiredError("bff", nil); got != "" {
		t.Errorf("no missing -> %q, want empty", got)
	}
	got := MissingRequiredError("voice", []string{"LIVEKIT_URL", "LIVEKIT_KEY"})
	want := `node type "voice" requires: LIVEKIT_URL, LIVEKIT_KEY (set them in the environment / genesis envelope)`
	if got != want {
		t.Errorf("MissingRequiredError = %q, want %q", got, want)
	}
}

// TestMissingRequired_EmbeddedRegistry exercises the real embedded
// registry: the default/bff path boots with only the DB DSN set (no
// over-requiring), while a voice node with the same env still fails on
// its LiveKit vars.
func TestMissingRequired_EmbeddedRegistry(t *testing.T) {
	t.Setenv("MEMQL_MANIFEST_PATH", "")
	t.Setenv("MEMQL_REPO", "")
	m, err := LoadManifest("")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	dbOnly := lookupFromMap(map[string]string{"MEMQL_DATABASE_DSN": "postgres://x"})

	if got := MissingRequired("bff", m, dbOnly); got != nil {
		t.Errorf("bff with DB DSN should boot, missing = %v", got)
	}

	got := MissingRequired("voice", m, dbOnly)
	want := map[string]bool{
		"MEMQL_POLYPHON_LIVEKIT_URL":        true,
		"MEMQL_POLYPHON_LIVEKIT_API_KEY":    true,
		"MEMQL_POLYPHON_LIVEKIT_API_SECRET": true,
	}
	if len(got) != len(want) {
		t.Fatalf("voice missing = %v, want the LiveKit vars %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected voice-required var reported missing: %q", n)
		}
	}
}
