package actiontrace

import (
	"reflect"
	"testing"
)

// findArg returns the binding for a named arg in a traced call, or fails.
func findArg(t *testing.T, tc TracedCall, name string) ArgBinding {
	t.Helper()
	for _, a := range tc.Args {
		if a.Name == name {
			return a
		}
	}
	t.Fatalf("call %d (%s) has no arg %q", tc.Index, tc.Capability, name)
	return ArgBinding{}
}

func TestTrace_ValueProvenance_StepInput(t *testing.T) {
	calls := []CapturedCall{{
		Index:      0,
		Capability: "fs.writeFile",
		Args:       map[string]any{"path": "/tmp/out", "content": "hello"},
	}}
	sources := ProvenanceSources{
		StepInput: map[string]any{"dir": "/tmp/out"},
	}
	got := Trace(calls, sources)

	path := findArg(t, got.Calls[0], "path")
	if path.Source.Kind != SourceStepInput || path.Source.Ref != "dir" {
		t.Fatalf("path: want stepInput@dir, got %+v", path.Source)
	}
	// content has no witnessed source -> literal (we never invent a binding).
	content := findArg(t, got.Calls[0], "content")
	if content.Source.Kind != SourceLiteral {
		t.Fatalf("content: want literal, got %+v", content.Source)
	}
}

func TestTrace_ValueProvenance_UpstreamResultAndPlanInput(t *testing.T) {
	calls := []CapturedCall{{
		Index:      0,
		Capability: "fs.writeFile",
		Args: map[string]any{
			"content": "RENDERED",
			"mode":    "0644",
		},
	}}
	sources := ProvenanceSources{
		UpstreamResults: map[string]any{"render": map[string]any{"result": "RENDERED"}},
		PlanInput:       map[string]any{"mode": "0644"},
	}
	got := Trace(calls, sources)

	content := findArg(t, got.Calls[0], "content")
	if content.Source.Kind != SourceUpstreamResult || content.Source.Ref != "render.result" {
		t.Fatalf("content: want upstreamResult@render.result, got %+v", content.Source)
	}
	mode := findArg(t, got.Calls[0], "mode")
	if mode.Source.Kind != SourcePlanInput || mode.Source.Ref != "mode" {
		t.Fatalf("mode: want planInput@mode, got %+v", mode.Source)
	}
}

func TestTrace_Precedence_StepInputBeatsOthers(t *testing.T) {
	// Same value present in all three sources: stepInput wins (deterministic).
	calls := []CapturedCall{{
		Index:      0,
		Capability: "fs.writeFile",
		Args:       map[string]any{"path": "/shared"},
	}}
	sources := ProvenanceSources{
		StepInput:       map[string]any{"a": "/shared"},
		UpstreamResults: map[string]any{"b": "/shared"},
		PlanInput:       map[string]any{"c": "/shared"},
	}
	got := Trace(calls, sources)
	path := findArg(t, got.Calls[0], "path")
	if path.Source.Kind != SourceStepInput {
		t.Fatalf("path: want stepInput (precedence), got %+v", path.Source)
	}
}

func TestTrace_FragmentBinding(t *testing.T) {
	// arg is a templated string with a witnessed value embedded in it.
	calls := []CapturedCall{{
		Index:      0,
		Capability: "fs.writeFile",
		Args:       map[string]any{"path": "/work/myproj/config.yaml"},
	}}
	sources := ProvenanceSources{StepInput: map[string]any{"dir": "/work/myproj"}}
	got := Trace(calls, sources)
	path := findArg(t, got.Calls[0], "path")
	if path.Source.Kind != SourceStepInput || !path.Source.Fragment || path.Source.Ref != "dir" {
		t.Fatalf("path: want stepInput@dir fragment=true, got %+v", path.Source)
	}
}

func TestTrace_ShortFragmentStaysLiteral(t *testing.T) {
	// A 2-char witnessed value is below minFragmentLen; coincidental
	// substrings must NOT bind.
	calls := []CapturedCall{{
		Index:      0,
		Capability: "shell.exec",
		Args:       map[string]any{"cmd": "go build ./..."},
	}}
	sources := ProvenanceSources{StepInput: map[string]any{"x": "go"}}
	got := Trace(calls, sources)
	cmd := findArg(t, got.Calls[0], "cmd")
	if cmd.Source.Kind != SourceLiteral {
		t.Fatalf("cmd: short fragment must stay literal, got %+v", cmd.Source)
	}
}

func TestTrace_NoSynthesizedTransforms(t *testing.T) {
	// A transformed value (uppercased) does not equal the witnessed source,
	// so it must stay literal -- we never synthesize a transform.
	calls := []CapturedCall{{
		Index:      0,
		Capability: "fs.writeFile",
		Args:       map[string]any{"content": "HELLO"},
	}}
	sources := ProvenanceSources{StepInput: map[string]any{"name": "hello"}}
	got := Trace(calls, sources)
	content := findArg(t, got.Calls[0], "content")
	if content.Source.Kind != SourceLiteral {
		t.Fatalf("content: transformed value must stay literal, got %+v", content.Source)
	}
}

func TestTrace_NonStringExactMatch(t *testing.T) {
	calls := []CapturedCall{{
		Index:      0,
		Capability: "http.fetch",
		Args:       map[string]any{"retries": float64(3), "verify": true},
	}}
	sources := ProvenanceSources{
		StepInput: map[string]any{"retries": float64(3)},
		PlanInput: map[string]any{"verify": true},
	}
	got := Trace(calls, sources)
	if r := findArg(t, got.Calls[0], "retries"); r.Source.Kind != SourceStepInput {
		t.Fatalf("retries: want stepInput, got %+v", r.Source)
	}
	if v := findArg(t, got.Calls[0], "verify"); v.Source.Kind != SourcePlanInput {
		t.Fatalf("verify: want planInput, got %+v", v.Source)
	}
}

func TestTrace_ResourceEdge_WriteThenRead(t *testing.T) {
	calls := []CapturedCall{
		{Index: 0, Capability: "fs.writeFile", Args: map[string]any{"path": "/tmp/config.yaml", "content": "x"}},
		{Index: 1, Capability: "fs.readFile", Args: map[string]any{"path": "/tmp/config.yaml"}},
	}
	got := Trace(calls, ProvenanceSources{})
	want := []ResourceEdge{{FromCallIndex: 0, ToCallIndex: 1, Resource: "/tmp/config.yaml"}}
	if !reflect.DeepEqual(got.ResourceEdges, want) {
		t.Fatalf("resource edges: want %+v, got %+v", want, got.ResourceEdges)
	}
}

func TestTrace_ResourceEdge_DistinctResourcesNoEdge(t *testing.T) {
	calls := []CapturedCall{
		{Index: 0, Capability: "fs.writeFile", Args: map[string]any{"path": "/a"}},
		{Index: 1, Capability: "fs.readFile", Args: map[string]any{"path": "/b"}},
	}
	got := Trace(calls, ProvenanceSources{})
	if len(got.ResourceEdges) != 0 {
		t.Fatalf("distinct resources must not couple, got %+v", got.ResourceEdges)
	}
}

func TestTrace_ResourceEdge_CrossKeySrcDest(t *testing.T) {
	// A copy writes dest=/b, a later read of path=/b couples to it.
	calls := []CapturedCall{
		{Index: 0, Capability: "fs.copy", Args: map[string]any{"src": "/a", "dest": "/b"}},
		{Index: 1, Capability: "fs.readFile", Args: map[string]any{"path": "/b"}},
	}
	got := Trace(calls, ProvenanceSources{})
	want := []ResourceEdge{{FromCallIndex: 0, ToCallIndex: 1, Resource: "/b"}}
	if !reflect.DeepEqual(got.ResourceEdges, want) {
		t.Fatalf("cross-key edge: want %+v, got %+v", want, got.ResourceEdges)
	}
}

func TestTrace_PreservesOrderAndError(t *testing.T) {
	calls := []CapturedCall{
		{Index: 0, Capability: "fs.mkdir", Args: map[string]any{"path": "/p"}},
		{Index: 1, Capability: "fs.writeFile", Args: map[string]any{"path": "/p/f"}, IsError: true},
	}
	got := Trace(calls, ProvenanceSources{})
	if len(got.Calls) != 2 || got.Calls[0].Index != 0 || got.Calls[1].Index != 1 {
		t.Fatalf("order not preserved: %+v", got.Calls)
	}
	if !got.Calls[1].IsError {
		t.Fatalf("isError not preserved on call 1")
	}
}

func TestTrace_EmptyAndNilSafe(t *testing.T) {
	got := Trace(nil, ProvenanceSources{})
	if len(got.Calls) != 0 || len(got.ResourceEdges) != 0 {
		t.Fatalf("nil input should yield empty trace, got %+v", got)
	}
	// Call with no args, nil sources: no panic, literal-free.
	got = Trace([]CapturedCall{{Index: 0, Capability: "noop"}}, ProvenanceSources{})
	if len(got.Calls) != 1 || len(got.Calls[0].Args) != 0 {
		t.Fatalf("want one arg-less call, got %+v", got.Calls)
	}
}

func TestTrace_DeterministicArgOrder(t *testing.T) {
	// Args map iteration is randomized in Go; the tracer must emit a stable
	// (sorted) arg order so candidate rows are reproducible.
	calls := []CapturedCall{{
		Index:      0,
		Capability: "fs.writeFile",
		Args:       map[string]any{"zeta": "1", "alpha": "2", "mu": "3"},
	}}
	got := Trace(calls, ProvenanceSources{})
	names := make([]string, 0, 3)
	for _, a := range got.Calls[0].Args {
		names = append(names, a.Name)
	}
	want := []string{"alpha", "mu", "zeta"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("arg order: want %v, got %v", want, names)
	}
}
