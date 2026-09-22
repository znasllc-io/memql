package planner

import (
	"strings"
	"testing"

	proc "github.com/znasllc-io/memql/component/procedure"
)

func speller(tool string) (string, string, bool) {
	switch tool {
	case "exec":
		return "mutation", "runCommand", true
	case "fs_write":
		return "mutation", "writeFile", true
	}
	return "", "", false
}

// TestRenderTemplateAutomation_AHolelessTemplateRendersIdenticallyToTheTranscriptPath
// is what keeps the extension an EXTENSION. A template with no holes is a
// transcript, and if the two paths drift the one-off capture quietly changes
// shape on a refactor nobody reviewed as such.
func TestRenderTemplateAutomation_AHolelessTemplateRendersIdenticallyToTheTranscriptPath(t *testing.T) {
	calls := []toolCall{
		{Name: "exec", Args: `{"command":"ls","retries":2}`},
		{Name: "fs_write", Args: `{"path":"/tmp/x","body":"hello"}`},
	}
	wantSource, wantEvery := renderTranscriptAutomation("p", "do the thing", calls, speller)

	tmpl := proc.Template{Steps: []proc.TemplateStep{
		{Tool: "exec", Args: proc.Obj(map[string]*proc.Node{
			"command": proc.Lit("ls"), "retries": proc.LitOf("2", "number"),
		})},
		{Tool: "fs_write", Args: proc.Obj(map[string]*proc.Node{
			"path": proc.Lit("/tmp/x"), "body": proc.Lit("hello"),
		})},
	}}
	gotSource, gotEvery := RenderTemplateAutomation("p", "do the thing", tmpl, TemplateProvenance{}, speller)

	if gotEvery != wantEvery {
		t.Fatalf("everyStepWritten = %v, transcript path says %v", gotEvery, wantEvery)
	}
	// The template path adds a provenance header and a different description;
	// the STATEMENTS are what must match byte for byte.
	if got, want := statementsOf(gotSource), statementsOf(wantSource); got != want {
		t.Fatalf("statements drifted from the transcript path:\n got: %s\nwant: %s", got, want)
	}
}

func statementsOf(src string) string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "call") || strings.HasPrefix(trimmed, "// call") {
			out = append(out, trimmed)
		}
	}
	return strings.Join(out, "\n")
}

func TestRenderTemplateAutomation_FreeParametersBecomeArgsAndDataFlowHolesBecomeStepReferences(t *testing.T) {
	tmpl := proc.Template{
		Steps: []proc.TemplateStep{
			{Tool: "exec", Args: proc.Obj(map[string]*proc.Node{"command": proc.Lit("mkid")})},
			{Tool: "fs_write", Args: proc.Obj(map[string]*proc.Node{
				"path": proc.HoleNode("s1.path", "string"),
				"name": proc.HoleNode("s1.name", "string"),
				"mode": proc.HoleNode("s1.mode", "string"),
			})},
		},
		Holes: []proc.Hole{
			{Id: "s1.path", StepIndex: 1, Path: []string{"path"}, Type: "string", Class: proc.HoleFree},
			{Id: "s1.name", StepIndex: 1, Path: []string{"name"}, Type: "string",
				Class: proc.HoleDataFlow, Ref: &proc.DataFlowRef{StepIndex: 0, Path: []string{"id"}}},
			{Id: "s1.mode", StepIndex: 1, Path: []string{"mode"}, Type: "string",
				Class: proc.HoleConstant, Const: "0644"},
		},
	}
	src, every := RenderTemplateAutomation("p", "", tmpl, TemplateProvenance{RunIds: []string{"r1", "r2"}, Uses: 2, Net: 7}, speller)
	if !every {
		t.Fatalf("every step should have a statement form:\n%s", src)
	}
	for _, want := range []string{
		"args {",                // the free parameter opened an args block
		"path1 string",          // ...named after its position
		"args.path1",            // ...and read as args.<name>
		"name: call0.id",        // the data-flow hole is a step reference
		`mode: "0644"`,          // the constant stayed literal
		"// From runs: r1, r2.", // D9's stamp is IN the source
		"// Accepted on 2 use(s), net compression 7.",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("rendered source is missing %q:\n%s", want, src)
		}
	}
	if strings.Contains(src, "args.name") || strings.Contains(src, "args.mode") {
		t.Errorf("only FREE holes become parameters:\n%s", src)
	}
}

func TestRenderTemplateAutomation_AnUnexplainedHoleIsRefusedRatherThanGuessed(t *testing.T) {
	tmpl := proc.Template{
		Steps: []proc.TemplateStep{{Tool: "exec", Args: proc.Obj(map[string]*proc.Node{
			"command": proc.HoleNode("s0.command", "string"),
		})}},
		Holes: []proc.Hole{{Id: "s0.command", StepIndex: 0, Path: []string{"command"},
			Type: "string", Class: proc.HoleUnexplained}},
	}
	src, every := RenderTemplateAutomation("p", "", tmpl, TemplateProvenance{}, speller)
	if every {
		t.Fatal("an unexplained hole has no spelling; the step must fall back to a comment line")
	}
	if strings.Contains(src, "args.") {
		t.Fatalf("an unexplained hole must not be silently written as a parameter:\n%s", src)
	}
}

func TestRenderTemplateAutomation_ANumberIsNotQuotedAndAStringIs(t *testing.T) {
	tmpl := proc.Template{Steps: []proc.TemplateStep{{Tool: "exec", Args: proc.Obj(map[string]*proc.Node{
		"retries": proc.LitOf("3", "number"),
		"label":   proc.LitOf("3", "string"),
	})}}}
	src, _ := RenderTemplateAutomation("p", "", tmpl, TemplateProvenance{}, speller)
	if !strings.Contains(src, "retries: 3") {
		t.Errorf("a number must render bare:\n%s", src)
	}
	if !strings.Contains(src, `label: "3"`) {
		t.Errorf("a string must keep its quotes even when it looks like a number:\n%s", src)
	}
}
