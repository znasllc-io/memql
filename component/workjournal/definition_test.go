package workjournal

import (
	"context"
	"slices"
	"testing"
)

func TestDefinitionFingerprintCoversDeclaredExecution(t *testing.T) {
	steps := []StepDecl{{Key: "first", Call: map[string]any{"command": "one", "image": "pinned"}}, {Key: "second", DependsOn: []string{"first"}}}
	base, err := DefinitionFingerprint("build", steps)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func([]StepDecl){
		"command":    func(s []StepDecl) { s[0].Call = map[string]any{"command": "two", "image": "pinned"} },
		"image":      func(s []StepDecl) { s[0].Call = map[string]any{"command": "one", "image": "different"} },
		"type":       func(s []StepDecl) { s[0].StepType = "exec" },
		"kind":       func(s []StepDecl) { s[0].Kind = "reasoning" },
		"dependency": func(s []StepDecl) { s[1].DependsOn = nil },
		"order":      func(s []StepDecl) { s[0], s[1] = s[1], s[0] },
	} {
		t.Run(name, func(t *testing.T) {
			changed := slices.Clone(steps)
			change(changed)
			got, err := DefinitionFingerprint("build", changed)
			if err != nil || got == base {
				t.Fatalf("changed definition accepted: %s %v", got, err)
			}
		})
	}
	// Defaults are semantic values, and JSON map insertion order is irrelevant.
	equivalent := slices.Clone(steps)
	equivalent[0].Kind, equivalent[0].StepType = KindDeterministic, defaultStepType
	equivalent[0].Call = map[string]any{"image": "pinned", "command": "one"}
	got, err := DefinitionFingerprint("build", equivalent)
	if err != nil || got != base {
		t.Fatalf("equivalent definition changed: %s %v", got, err)
	}
}

func TestInvalidDefinitionWritesNothing(t *testing.T) {
	engine := &lockedEngine{}
	w := Work{OwnerUserID: "owner", Template: "build", Steps: []StepDecl{{Key: "bad", Call: map[string]any{"unsupported": make(chan int)}}}}
	if _, err := New(engine, nil, "node").Begin(context.Background(), w); err == nil {
		t.Fatal("unencodable definition accepted")
	}
	if got := engine.snapshot(); len(got) != 0 {
		t.Fatalf("invalid definition wrote %d rows", len(got))
	}
}
