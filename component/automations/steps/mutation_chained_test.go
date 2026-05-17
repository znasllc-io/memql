package steps

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

// makeEvaluatorWithStep creates an Evaluator whose step "getFirstStep" holds
// a single node. The node's payload.targetData is set to targetData.
func makeEvaluatorWithStep(targetData any) *automations.Evaluator {
	node := map[string]any{
		"payload": map[string]any{
			"stepId":        "step-tour-0",
			"sessionId":     "tour-abc",
			"stepNumber":    float64(0),
			"stepType":      "explain",
			"title":         "Welcome",
			"description":   "Welcome to the tour",
			"targetElement": nil,
			"targetData":    targetData,
			"status":        "pending",
		},
	}

	eval := automations.NewEvaluator()
	eval.SetStepResult("getFirstStep", &automations.StepResult{
		StepId: "getFirstStep",
		Status: "success",
		Result: map[string]any{
			"Bundle": map[string]any{
				"nodes": []any{node},
			},
		},
		Metadata: map[string]any{"itemCount": 1},
	})
	return eval
}

// TestEvaluateValue_FirstChainedAccess_Object verifies that
// first(getFirstStep).payload.targetData returns an object (map), not a string.
func TestEvaluateValue_FirstChainedAccess_Object(t *testing.T) {
	targetData := map[string]any{"duration": float64(3000)}
	eval := makeEvaluatorWithStep(targetData)

	exec := &MutationExecutor{}
	result, err := exec.evaluateValue(eval, "first(getFirstStep).payload.targetData")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T (%v)", result, result)
	}
	if m["duration"] != float64(3000) {
		t.Fatalf("expected duration=3000, got %v", m["duration"])
	}
}

// TestEvaluateValue_FirstChainedAccess_Null verifies that
// first(getFirstStep).payload.targetData returns nil when targetData is null.
func TestEvaluateValue_FirstChainedAccess_Null(t *testing.T) {
	eval := makeEvaluatorWithStep(nil)

	exec := &MutationExecutor{}
	result, err := exec.evaluateValue(eval, "first(getFirstStep).payload.targetData")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil, got %T (%v)", result, result)
	}
}

// TestEvaluateValue_FirstChainedAccess_String verifies that
// first(getFirstStep).payload.title returns a plain string value.
func TestEvaluateValue_FirstChainedAccess_String(t *testing.T) {
	eval := makeEvaluatorWithStep(nil)

	exec := &MutationExecutor{}
	result, err := exec.evaluateValue(eval, "first(getFirstStep).payload.title")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, ok := result.(string)
	if !ok {
		t.Fatalf("expected string, got %T (%v)", result, result)
	}
	if s != "Welcome" {
		t.Fatalf("expected %q, got %q", "Welcome", s)
	}
}

// TestEvaluateValue_FirstChainedAccess_NestedPath verifies deep property access
// like first(step).payload.targetData.duration.
func TestEvaluateValue_FirstChainedAccess_NestedPath(t *testing.T) {
	targetData := map[string]any{
		"config": map[string]any{
			"spaceName": "Demo Workspace",
		},
	}
	eval := makeEvaluatorWithStep(targetData)

	exec := &MutationExecutor{}
	result, err := exec.evaluateValue(eval, "first(getFirstStep).payload.targetData.config.spaceName")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, ok := result.(string)
	if !ok {
		t.Fatalf("expected string, got %T (%v)", result, result)
	}
	if s != "Demo Workspace" {
		t.Fatalf("expected %q, got %q", "Demo Workspace", s)
	}
}

// TestEvaluateValue_FirstWithoutChain verifies that first(step) without
// chained property access still works as before (regression check).
func TestEvaluateValue_FirstWithoutChain(t *testing.T) {
	eval := makeEvaluatorWithStep(nil)

	exec := &MutationExecutor{}
	result, err := exec.evaluateValue(eval, "first(getFirstStep)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T (%v)", result, result)
	}
	payload, ok := m["payload"].(map[string]any)
	if !ok {
		t.Fatalf("expected payload to be map, got %T", m["payload"])
	}
	if payload["title"] != "Welcome" {
		t.Fatalf("expected title=%q, got %v", "Welcome", payload["title"])
	}
}

// TestEvaluateValue_ConcatWithoutChain verifies that concat() still works
// when no chained path is present (regression check).
func TestEvaluateValue_ConcatWithoutChain(t *testing.T) {
	eval := automations.NewEvaluator()
	exec := &MutationExecutor{}

	result, err := exec.evaluateValue(eval, `concat("hello", " ", "world")`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, ok := result.(string)
	if !ok {
		t.Fatalf("expected string, got %T (%v)", result, result)
	}
	if s != "hello world" {
		t.Fatalf("expected %q, got %q", "hello world", s)
	}
}

// TestSplitFuncCallAndPath_Table tests the helper function with various inputs.
func TestSplitFuncCallAndPath_Table(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantFunc  string
		wantPath  string
		wantOk    bool
	}{
		{
			name:     "first with chained access",
			input:    "first(getFirstStep).payload.targetData",
			wantFunc: "first(getFirstStep)",
			wantPath: "payload.targetData",
			wantOk:   true,
		},
		{
			name:     "index with chained access",
			input:    "index(getSteps, 0).payload.name",
			wantFunc: "index(getSteps, 0)",
			wantPath: "payload.name",
			wantOk:   true,
		},
		{
			name:     "nested functions with chained access",
			input:    "coalesce(first(a), first(b)).payload.id",
			wantFunc: "coalesce(first(a), first(b))",
			wantPath: "payload.id",
			wantOk:   true,
		},
		{
			name:   "no chained path",
			input:  "first(getFirstStep)",
			wantOk: false,
		},
		{
			name:   "simple concat no chain",
			input:  `concat("a", "b")`,
			wantOk: false,
		},
		{
			name:   "bare path no function",
			input:  "item.payload.name",
			wantOk: false,
		},
		{
			name:   "dollar expression",
			input:  "$steps.getFirstStep.result",
			wantOk: false,
		},
		{
			name:   "empty string",
			input:  "",
			wantOk: false,
		},
		{
			name:   "literal string",
			input:  "hello",
			wantOk: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			funcCall, chainedPath, ok := splitFuncCallAndPath(tt.input)
			if ok != tt.wantOk {
				t.Fatalf("ok: got %v, want %v", ok, tt.wantOk)
			}
			if !tt.wantOk {
				return
			}
			if funcCall != tt.wantFunc {
				t.Errorf("funcCall: got %q, want %q", funcCall, tt.wantFunc)
			}
			if chainedPath != tt.wantPath {
				t.Errorf("chainedPath: got %q, want %q", chainedPath, tt.wantPath)
			}
		})
	}
}

// TestResolveChainedPath_Table tests the path resolver with various inputs.
func TestResolveChainedPath_Table(t *testing.T) {
	obj := map[string]any{
		"payload": map[string]any{
			"name": "test",
			"nested": map[string]any{
				"deep": "value",
			},
			"nullField": nil,
		},
	}

	tests := []struct {
		name   string
		obj    any
		path   string
		want   any
	}{
		{
			name: "simple field",
			obj:  obj,
			path: "payload.name",
			want: "test",
		},
		{
			name: "deep nested",
			obj:  obj,
			path: "payload.nested.deep",
			want: "value",
		},
		{
			name: "null field",
			obj:  obj,
			path: "payload.nullField",
			want: nil,
		},
		{
			name: "missing field",
			obj:  obj,
			path: "payload.nonexistent",
			want: nil,
		},
		{
			name: "nil object",
			obj:  nil,
			path: "payload.name",
			want: nil,
		},
		{
			name: "object at path",
			obj:  obj,
			path: "payload.nested",
			want: map[string]any{"deep": "value"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := resolveChainedPath(tt.obj, tt.path)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// Use JSON comparison for maps
			if m, ok := tt.want.(map[string]any); ok {
				rm, rok := result.(map[string]any)
				if !rok {
					t.Fatalf("expected map, got %T", result)
				}
				for k, v := range m {
					if rm[k] != v {
						t.Errorf("key %q: got %v, want %v", k, rm[k], v)
					}
				}
			} else {
				if result != tt.want {
					t.Fatalf("got %v (%T), want %v (%T)", result, result, tt.want, tt.want)
				}
			}
		})
	}
}
