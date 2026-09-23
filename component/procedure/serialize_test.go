package procedure

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// sameNode compares every field of two trees, which Equal deliberately does
// not: a round trip that lost a Form or a LitType would still be Equal, and
// would materialize something else. Slices compare by length and elements, so
// an empty slice and a nil one are the same value.
func sameNode(t *testing.T, path string, a, b *Node) {
	t.Helper()
	if (a == nil) != (b == nil) {
		t.Fatalf("%s: nil %v vs %v", path, a == nil, b == nil)
	}
	if a == nil {
		return
	}
	if a.Kind != b.Kind || a.Lit != b.Lit || a.LitType != b.LitType || a.Form != b.Form ||
		a.HoleId != b.HoleId || a.HoleType != b.HoleType {
		t.Fatalf("%s: %+v\n     vs %+v", path, *a, *b)
	}
	if len(a.Keys) != len(b.Keys) || len(a.Kids) != len(b.Kids) {
		t.Fatalf("%s: keys %v / %d kids vs keys %v / %d kids", path, a.Keys, len(a.Kids), b.Keys, len(b.Kids))
	}
	for i := range a.Keys {
		if a.Keys[i] != b.Keys[i] {
			t.Fatalf("%s: key %d %q vs %q", path, i, a.Keys[i], b.Keys[i])
		}
	}
	for i := range a.Kids {
		sameNode(t, path+"/"+string(rune('0'+i)), a.Kids[i], b.Kids[i])
	}
}

// everyFieldTemplate exercises every Node field and every Hole field.
func everyFieldTemplate() Template {
	argv := Arr(Lit("echo"), HoleNode("s0.command.1", "string"), Lit(">"), Lit("out.txt"))
	argv.Form = FormArgv
	rooted := Arr(Lit(""), Lit("cdn.example.com"), Lit("x"))
	rooted.Form = FormRootedPath
	rel := Arr(Lit("."), Lit("a"))
	rel.Form = FormPath
	doc := Obj(map[string]*Node{
		"n":    LitOf("12345678901234567890", "number"),
		"b":    LitOf("true", "bool"),
		"z":    LitOf("", "null"),
		"s":    LitOf("3", "string"),
		"list": Arr(Lit("x"), HoleNode("s1.payload.list.1", "number")),
	})
	doc.Form = FormJSON
	return Template{
		Steps: []TemplateStep{
			{Tool: "exec", Args: Obj(map[string]*Node{"command": argv, "cwd": rooted})},
			{Tool: "mcp", Args: Obj(map[string]*Node{"payload": doc, "target": rel, "empty": Obj(nil), "none": Arr()})},
		},
		Holes: []Hole{
			{Id: "s0.command.1", StepIndex: 0, Path: []string{"command", "1"}, Type: "string", Class: HoleFree, Evidence: 2},
			{Id: "s1.payload.list.1", StepIndex: 1, Path: []string{"payload", "list", "1"}, Type: "number",
				Class: HoleDataFlow, Ref: &DataFlowRef{StepIndex: 0, Path: []string{"id"}}, Evidence: 3,
				Derivation: `basename(ref(0, "path"))`},
			{Id: "s1.target.0", StepIndex: 1, Path: []string{}, Type: "string", Class: HoleConstant, Const: "0644"},
		},
	}
}

// TestMarshalTemplateRoundTripsEveryNodeAndHoleField: the stored procedure is
// what a replay executes, so everything Materialize and the renderer read --
// the Form, the literal types, the hole ids and types, every classification
// -- must come back exactly.
func TestMarshalTemplateRoundTripsEveryNodeAndHoleField(t *testing.T) {
	in := everyFieldTemplate()
	b, err := MarshalTemplate(in)
	if err != nil {
		t.Fatalf("MarshalTemplate: %v", err)
	}
	out, err := UnmarshalTemplate(b)
	if err != nil {
		t.Fatalf("UnmarshalTemplate: %v\n%s", err, b)
	}
	if len(out.Steps) != len(in.Steps) {
		t.Fatalf("%d steps back, want %d", len(out.Steps), len(in.Steps))
	}
	for i := range in.Steps {
		if out.Steps[i].Tool != in.Steps[i].Tool {
			t.Fatalf("step %d tool %q, want %q", i, out.Steps[i].Tool, in.Steps[i].Tool)
		}
		sameNode(t, "step"+string(rune('0'+i)), in.Steps[i].Args, out.Steps[i].Args)
	}
	if !reflect.DeepEqual(out.Holes, in.Holes) {
		t.Fatalf("holes differ:\n got %+v\nwant %+v", out.Holes, in.Holes)
	}
	again, _ := MarshalTemplate(out)
	if string(again) != string(b) {
		t.Fatalf("the encoding is not stable across a round trip:\n%s\n%s", b, again)
	}
}

// TestALearnedTemplateReplaysTheSameAfterARoundTrip: the property the payload
// exists for, end to end -- a template generalized from recordings, stored and
// read back, materializes the command the original would have.
func TestALearnedTemplateReplaysTheSameAfterARoundTrip(t *testing.T) {
	instances := [][]Action{{execStep("echo hi > a.txt")}, {execStep("echo hi > b.txt")}}
	tmpl := Generalize(instances)
	tmpl.Holes = Classify(tmpl, instances)
	b, err := MarshalTemplate(tmpl)
	if err != nil {
		t.Fatalf("MarshalTemplate: %v", err)
	}
	back, err := UnmarshalTemplate(b)
	if err != nil {
		t.Fatalf("UnmarshalTemplate: %v", err)
	}
	values := map[string]string{"s0.command.3": "c d.txt"}
	want, _ := Materialize(tmpl.Steps[0].Args, values)
	got, err := Materialize(back.Steps[0].Args, values)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("after a round trip Materialize = %v, %v; before it was %v", got, err, want)
	}
}

func TestMarshalTreeRoundTrips(t *testing.T) {
	model := tree(OpSeq,
		leaf("start"),
		tree(OpXor, tree(OpLoop, leaf("try"), leaf("fail")), tree(OpSeq)),
		tree(OpAnd, leaf("a"), seqOf("b1", "b2")),
		flower([]string{"x", "y"}),
		leaf("done"),
	)
	b, err := MarshalTree(model)
	if err != nil {
		t.Fatalf("MarshalTree: %v", err)
	}
	back, err := UnmarshalTree(b)
	if err != nil {
		t.Fatalf("UnmarshalTree: %v\n%s", err, b)
	}
	if treeString(back) != treeString(model) {
		t.Fatalf("round trip changed the tree:\n got %s\nwant %s", treeString(back), treeString(model))
	}
	trace := []string{"start", "try", "fail", "try", "b1", "a", "b2", "y", "x", "done"}
	if TokenReplay(back, trace) != TokenReplay(model, trace) {
		t.Fatal("the round-tripped tree replays differently")
	}
	if b, err := MarshalTree(nil); err != nil || string(b) != "null" {
		t.Fatalf("MarshalTree(nil) = %s, %v; want null", b, err)
	}
	if back, err := UnmarshalTree([]byte("null")); err != nil || back != nil {
		t.Fatalf("UnmarshalTree(null) = %v, %v; want nil", back, err)
	}
}

// TestTheWireShapeIsPinned: this encoding is stored on the construct, hashed
// into the procedureHash a promotion approval pins, and read by the OS to draw
// a procedure's steps. A renamed key is a stored payload nobody can read, so a
// change here must be a decision rather than a refactor.
func TestTheWireShapeIsPinned(t *testing.T) {
	argv := Arr(Lit("echo"), HoleNode("s0.command.1", "string"))
	argv.Form = FormArgv
	tmpl := Template{
		Steps: []TemplateStep{{Tool: "exec", Args: Obj(map[string]*Node{"command": argv, "n": LitOf("2", "number")})}},
		Holes: []Hole{{Id: "s0.command.1", StepIndex: 0, Path: []string{"command", "1"}, Type: "string", Class: HoleFree, Evidence: 2}},
	}
	b, err := MarshalTemplate(tmpl)
	if err != nil {
		t.Fatalf("MarshalTemplate: %v", err)
	}
	want := `{"steps":[{"tool":"exec","args":{"kind":"object","keys":["command","n"],"kids":[` +
		`{"kind":"array","form":"argv","kids":[{"kind":"lit","lit":"echo"},{"kind":"hole","holeId":"s0.command.1","holeType":"string"}]},` +
		`{"kind":"lit","lit":"2","litType":"number"}]}}],` +
		`"holes":[{"id":"s0.command.1","stepIndex":0,"path":["command","1"],"type":"string","class":"free","evidence":2}]}`
	if string(b) != want {
		t.Fatalf("template wire shape changed:\n got %s\nwant %s", b, want)
	}
	tb, err := MarshalTree(tree(OpLoop, leaf("s0"), tree(OpSeq)))
	if err != nil {
		t.Fatalf("MarshalTree: %v", err)
	}
	if want := `{"op":"loop","children":[{"op":"leaf","symbol":"s0"},{"op":"seq"}]}`; string(tb) != want {
		t.Fatalf("tree wire shape changed:\n got %s\nwant %s", tb, want)
	}
	sb, err := json.Marshal(Symbol{Id: "s0", Tool: "exec", Template: argv})
	if err != nil {
		t.Fatalf("marshal symbol: %v", err)
	}
	if want := `{"id":"s0","tool":"exec","template":{"kind":"array","form":"argv","kids":[{"kind":"lit","lit":"echo"},{"kind":"hole","holeId":"s0.command.1","holeType":"string"}]}}`; string(sb) != want {
		t.Fatalf("symbol wire shape changed:\n got %s\nwant %s", sb, want)
	}
}

// TestUnmarshalRefusesWhatItCannotReplay: a stored procedure is executed, and
// a node or a tree this code does not understand -- a kind, a form, a type or
// an operator from a newer writer, or a malformed one -- is refused rather
// than read as something close to it. A replay that ignored an unknown Form
// would send a list where a command line was recorded.
func TestUnmarshalRefusesWhatItCannotReplay(t *testing.T) {
	for _, bad := range []string{
		`{"steps":[{"tool":"exec","args":{"kind":"tuple"}}]}`,
		`{"steps":[{"tool":"exec","args":{"kind":"array","form":"url","kids":[]}}]}`,
		`{"steps":[{"tool":"exec","args":{"kind":"lit","lit":"1","litType":"integer"}}]}`,
		`{"steps":[{"tool":"exec","args":{"kind":"hole","holeId":"h","holeType":"date"}}]}`,
		`{"steps":[{"tool":"exec","args":{"kind":"object","keys":["a","b"],"kids":[{"kind":"lit"}]}}]}`,
		`{"steps":[{"tool":"exec","args":{"kind":"object","keys":["a"],"kids":[null]}}]}`,
		`{"steps":[{"tool":"exec","args":{"kind":"lit","lit":"x","kids":[{"kind":"lit"}]}}]}`,
		`{"steps":[{"tool":"exec","args":{"kind":"hole","holeType":"string"}}]}`,
	} {
		if _, err := UnmarshalTemplate([]byte(bad)); err == nil {
			t.Errorf("UnmarshalTemplate accepted %s", bad)
		}
	}
	for _, bad := range []string{
		`{"op":"star","children":[]}`,
		`{"op":"leaf"}`,
		`{"op":"leaf","symbol":"a","children":[{"op":"leaf","symbol":"b"}]}`,
		`{"op":"seq","symbol":"a"}`,
		`{"op":"seq","children":[null]}`,
		`{"op":"loop"}`,
	} {
		if _, err := UnmarshalTree([]byte(bad)); err == nil {
			t.Errorf("UnmarshalTree accepted %s", bad)
		}
	}
}

// TestMarshalRefusesANodeItCouldNotReadBack: the check runs on the way IN as
// well as out, so a malformed tree fails where it was built rather than on the
// replica that later tries to replay it.
func TestMarshalRefusesANodeItCouldNotReadBack(t *testing.T) {
	bad := Arr(Lit("x"))
	bad.Form = "url"
	if _, err := MarshalTemplate(Template{Steps: []TemplateStep{{Tool: "exec", Args: bad}}}); err == nil || !strings.Contains(err.Error(), "url") {
		t.Fatalf("MarshalTemplate must refuse an unknown Form naming it; got %v", err)
	}
	if _, err := MarshalTree(&ProcessTree{Op: "star"}); err == nil {
		t.Fatal("MarshalTree must refuse an unknown operator")
	}
}
