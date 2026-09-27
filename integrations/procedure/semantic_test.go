package procedure

import (
	"testing"

	proc "github.com/znasllc-io/memql/component/procedure"
)

// TestAWordTheModelWroteForOneCallIsNotAParameter: two recordings of the same
// command whose Bash descriptions differ must generalize to a template with no
// hole at all. The description is the model's note to a person; left in, it is
// a free parameter no goal input maps to, and every canary start refuses it.
func TestAWordTheModelWroteForOneCallIsNotAParameter(t *testing.T) {
	a := proc.Canonicalize([]proc.Step{{StepType: "exec", Input: semanticArgs("exec", map[string]any{
		"command": "npm test", "description": "Run the test suite", "timeout": 120000.0,
	})}})
	b := proc.Canonicalize([]proc.Step{{StepType: "exec", Input: semanticArgs("exec", map[string]any{
		"command": "npm test", "description": "Check that everything still passes",
	})}})
	tmpl := proc.Generalize([][]proc.Action{a, b})
	if len(tmpl.Holes) != 0 {
		t.Fatalf("the template has holes %v; a description and a timeout are not the call", tmpl.Holes)
	}
}

// TestSemanticArgsKeepsWhatExecutionReads: the command, a background flag the
// replayable predicate refuses, and every key of a tool the table does not
// name all survive -- an unknown key is a hole the ladder can see, never a
// silent difference.
func TestSemanticArgsKeepsWhatExecutionReads(t *testing.T) {
	got := semanticArgs("exec", map[string]any{"command": "ls", "run_in_background": true, "description": "x"})
	if _, ok := got["description"]; ok || got["command"] != "ls" || got["run_in_background"] != true {
		t.Fatalf("exec args = %v", got)
	}
	fetch := semanticArgs("fetch", map[string]any{"url": "https://example.com", "prompt": "summarise"})
	if _, ok := fetch["prompt"]; ok || fetch["url"] != "https://example.com" {
		t.Fatalf("fetch args = %v", fetch)
	}
	write := map[string]any{"file_path": "./a", "content": "x", "description": "kept: fs_write names none"}
	if got := semanticArgs("fs_write", write); len(got) != 3 {
		t.Fatalf("fs_write args = %v; a step type the table does not name keeps every key", got)
	}
	orig := map[string]any{"command": "ls", "description": "x"}
	_ = semanticArgs("exec", orig)
	if _, ok := orig["description"]; !ok {
		t.Fatal("semanticArgs modified its input")
	}
}
