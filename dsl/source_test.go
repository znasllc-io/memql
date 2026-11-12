package dsl

import (
	"testing"
	"testing/fstest"
)

// SourceOf answers from the layer that serves a path, so a domain's name can
// never make it the engine's.
func TestSourceOfAnswersFromTheLayerThatServesThePath(t *testing.T) {
	const pack, runtime = "zzsourcepack", "zzsourceruntime"
	tree := fstest.MapFS{"automations.memql": {Data: []byte("// empty\n")}}
	RegisterTree(pack, tree)
	t.Cleanup(func() { UnregisterTree(pack) })
	registerTree(runtime, tree, true)
	t.Cleanup(func() { UnregisterTree(runtime) })

	live := Tree()
	for path, want := range map[string]Source{
		"campaigns/automations.memql":    SourceEngine,
		"campaigns/nested/x.memql":       SourceEngine,
		pack + "/automations.memql":      SourcePack,
		runtime + "/automations.memql":   SourceRuntime,
		"zznotmounted/automations.memql": SourceUnknown,
		"automations.memql":              SourceUnknown,
		"":                               SourceUnknown,
	} {
		if got := SourceOf(live, path); got != want {
			t.Errorf("SourceOf(Tree(), %q) = %s, want %s", path, got, want)
		}
	}

	// The engine's tree with nothing registered is the embed itself.
	if got := SourceOf(embedFS, "campaigns/automations.memql"); got != SourceEngine {
		t.Errorf("SourceOf(embedFS, campaigns/...) = %s, want engine", got)
	}

	// A tree this package did not build is never the engine's, whatever its
	// directories are called.
	foreign := fstest.MapFS{"campaigns/automations.memql": {Data: []byte("// empty\n")}}
	if got := SourceOf(foreign, "campaigns/automations.memql"); got != SourceUnknown {
		t.Errorf("SourceOf(a test map, campaigns/...) = %s, want unknown", got)
	}

	// A package analysis' candidate domain is a runtime domain, as it will be
	// once staged.
	candidate, _, _, err := CandidateTree(fstest.MapFS{"zzcandidate/automations.memql": {Data: []byte("// empty\n")}})
	if err != nil {
		t.Fatalf("CandidateTree: %v", err)
	}
	for path, want := range map[string]Source{
		"zzcandidate/automations.memql": SourceRuntime,
		"campaigns/automations.memql":   SourceEngine,
		pack + "/automations.memql":     SourcePack,
		runtime + "/automations.memql":  SourceRuntime,
	} {
		if got := SourceOf(candidate, path); got != want {
			t.Errorf("SourceOf(candidate, %q) = %s, want %s", path, got, want)
		}
	}
}
