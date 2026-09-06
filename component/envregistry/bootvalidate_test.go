package envregistry

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
	t.Setenv("MEMQL_NODE_TYPE", "  agent  ")
	if got := ResolveNodeType(); got != "agent" {
		t.Errorf("trimmed -> %q, want agent", got)
	}
}

func TestMissingRequired(t *testing.T) {
	// A hand-built manifest exercising every axis: an "all" var, two
	// node-specific vars, a defaulted "all" var, and an unrelated
	// optional var that no node requires.
	m := &Manifest{
		Secrets: []ManifestEntry{
			{Name: "DB_DSN", Required: []string{"all"}},
			{Name: "IDENTITY_BASE_URL", Required: []string{"identity"}},
			{Name: "IDENTITY_KEY", Required: []string{"identity"}},
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
			want:     []string{"DB_DSN"}, // GRPC_ADDR defaulted, the identity pair not required for bff
		},
		{
			name:     "identity missing its node-specific vars",
			nodeType: "identity",
			env:      map[string]string{"DB_DSN": "postgres://x"},
			want:     []string{"IDENTITY_BASE_URL", "IDENTITY_KEY"},
		},
		{
			name:     "identity fully satisfied",
			nodeType: "identity",
			env: map[string]string{
				"DB_DSN":            "postgres://x",
				"IDENTITY_BASE_URL": "https://identity.example",
				"IDENTITY_KEY":      "k",
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
			nodeType: "agent",
			env:      map[string]string{"DB_DSN": "postgres://x"},
			want:     nil, // "all" covers agent too; the identity pair is identity-only
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
	got := MissingRequiredError("identity", []string{"IDENTITY_BASE_URL", "IDENTITY_KEY"})
	want := `node type "identity" requires: IDENTITY_BASE_URL, IDENTITY_KEY (set them in the environment / memql-secrets Secret)`
	if got != want {
		t.Errorf("MissingRequiredError = %q, want %q", got, want)
	}
}

// TestMissingRequired_EmbeddedRegistry exercises the real embedded
// registry: the default/bff path boots with only the DB DSN set (no
// over-requiring), while an identity node with the same env still fails on
// its own public origin.
//
// The node-specific axis used to be asserted on the voice node's LiveKit trio.
// Epic memql#4988 retired the voice node type and its whole env surface, so
// the claim is made on the one node-specific requirement the shipped manifest
// still carries -- and the manifest, not this list, is the source of truth: the
// expectation is READ from it rather than restated, so a second node-specific
// entry appearing later widens this test instead of silently escaping it.
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

	want := map[string]bool{}
	for _, e := range m.AllEntries() {
		if e.RequiredFor("identity") && !e.RequiredFor("bff") {
			want[e.Name] = true
		}
	}
	// The reachable positive: an empty want would make the loop below vacuous.
	if len(want) == 0 {
		t.Fatal("no manifest entry is required for identity alone; this test would pass " +
			"against a registry with no per-node-type requiredness at all")
	}

	got := MissingRequired("identity", m, dbOnly)
	if len(got) != len(want) {
		t.Fatalf("identity missing = %v, want the identity-only vars %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected identity-required var reported missing: %q", n)
		}
	}
}
