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
	if a.Kind != b.Kind || a.Lit != b.Lit || a.LitType != b.LitType || a.Raw != b.Raw || a.Form != b.Form ||
		a.HoleId != b.HoleId || a.HoleType != b.HoleType || !reflect.DeepEqual(a.Seps, b.Seps) {
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
	argv := Arr(Lit("echo"), HoleNode("s0.command.1", "string"), Lit(">"), &Node{Kind: KindLit, Lit: "out file.txt", Raw: `"out file.txt"`})
	argv.Form = FormArgv
	argv.Seps = []string{"", "  ", " ", " ", "\n"}
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
			{Id: "s0.command.1", StepIndex: 0, Path: []string{"command", "1"}, Type: "string", Class: HoleFree, Evidence: 2,
				Shape: &HoleShape{Dash: true, DotDot: true}},
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

	// A command line canonicalized from a recording carries its spelling:
	// each token's `raw` beside its `lit`, and the argv's `seps` beside its
	// `kids`. Both are omitted when absent, so every payload written before
	// them encodes, and decodes, exactly as it did.
	spelled := execStep(`echo  "a b"`).Args
	pb, err := json.Marshal(spelled)
	if err != nil {
		t.Fatalf("marshal a spelled command: %v", err)
	}
	if want := `{"kind":"object","keys":["command"],"kids":[{"kind":"array","form":"argv",` +
		`"kids":[{"kind":"lit","lit":"echo","raw":"echo"},{"kind":"lit","lit":"a b","raw":"\"a b\""}],"seps":["","  ",""]}]}`; string(pb) != want {
		t.Fatalf("spelled wire shape changed:\n got %s\nwant %s", pb, want)
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

// TestAPayloadWrittenBeforeTheSpellingDecodesAsBefore: a stored argv with no
// `raw` and no `seps` is a payload from before them. It decodes with neither,
// and replays as it always did -- its tokens re-quoted by the lenient rule and
// joined by single spaces.
func TestAPayloadWrittenBeforeTheSpellingDecodesAsBefore(t *testing.T) {
	old := `{"steps":[{"tool":"exec","args":{"kind":"object","keys":["command"],"kids":[` +
		`{"kind":"array","form":"argv","kids":[{"kind":"lit","lit":"echo"},{"kind":"lit","lit":"a b"}]}]}}],"holes":[]}`
	tmpl, err := UnmarshalTemplate([]byte(old))
	if err != nil {
		t.Fatalf("UnmarshalTemplate: %v", err)
	}
	argv, _ := tmpl.Steps[0].Args.At([]string{"command"})
	if argv.Seps != nil || argv.Kids[1].Raw != "" {
		t.Fatalf("an old payload decoded with a spelling it never carried: %q / %q", argv.Seps, argv.Kids[1].Raw)
	}
	if cmd := materializedCommand(t, tmpl.Steps[0].Args, nil); cmd != "echo 'a b'" {
		t.Fatalf("command = %q, want the tokens re-quoted and joined as before", cmd)
	}
}

// TestUnmarshalRefusesASpellingThatDisagreesWithItsTree: the spelling is what
// a replay SENDS, and the tree is what a person approved and what Bind reads.
// A `raw` that does not read back as its own `lit` -- one word with that value
// -- or `seps` that are not pure separator text, or not one more than the
// tokens, would send a command the tree does not describe; it is refused on
// the way in and on the way out.
func TestUnmarshalRefusesASpellingThatDisagreesWithItsTree(t *testing.T) {
	for _, bad := range []string{
		// A spelling that is two words, or another value.
		`{"kind":"array","form":"argv","kids":[{"kind":"lit","lit":"x","raw":"x; rm -rf ~"}]}`,
		`{"kind":"array","form":"argv","kids":[{"kind":"lit","lit":"x","raw":"'y'"}]}`,
		// A spelling on something that is not an argv token.
		`{"kind":"array","form":"path","kids":[{"kind":"lit","lit":"x","raw":"x"}]}`,
		`{"kind":"hole","holeId":"h","raw":"x"}`,
		// Separators of the wrong count, carrying text, or empty between tokens.
		`{"kind":"array","form":"argv","kids":[{"kind":"lit","lit":"a"}],"seps":[""]}`,
		`{"kind":"array","form":"argv","kids":[{"kind":"lit","lit":"a"},{"kind":"lit","lit":"b"}],"seps":["","; rm -rf ~ ",""]}`,
		`{"kind":"array","form":"argv","kids":[{"kind":"lit","lit":"a"},{"kind":"lit","lit":"b"}],"seps":["","",""]}`,
		`{"kind":"array","form":"path","kids":[{"kind":"lit","lit":"a"}],"seps":["",""]}`,
	} {
		var n Node
		if err := json.Unmarshal([]byte(bad), &n); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	good := `{"kind":"array","form":"argv","kids":[{"kind":"lit","lit":"a b","raw":"a\\ b"},{"kind":"lit","lit":"c"}],"seps":[" ","\\\n\t",""]}`
	var n Node
	if err := json.Unmarshal([]byte(good), &n); err != nil {
		t.Fatalf("a consistent spelling was refused: %v", err)
	}
	bad := &Node{Kind: KindArray, Form: FormArgv, Kids: []*Node{{Kind: KindLit, Lit: "x", Raw: "$(id)"}}}
	if _, err := json.Marshal(bad); err == nil {
		t.Fatal("MarshalJSON must refuse a spelling that disagrees with its value")
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

// TestDigestIsTheSha256PrefixedHexOfItsBytes pins the version digest's
// spelling. It is compared byte for byte -- a promotion approval's
// artifactHash against the construct's procedureHash -- so a second spelling
// of the same bytes would read as a different version and refuse every
// approval. The fixed value is sha256("abc"), which anyone can check with
// sha256sum.
func TestDigestIsTheSha256PrefixedHexOfItsBytes(t *testing.T) {
	const want = "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := Digest([]byte("abc")); got != want {
		t.Fatalf("Digest(abc) = %s, want %s", got, want)
	}
	if Digest([]byte("abc")) == Digest([]byte("abd")) {
		t.Fatal("two different inputs share a digest")
	}
	if got := Digest(nil); !strings.HasPrefix(got, "sha256:") || len(got) != len("sha256:")+64 {
		t.Fatalf("Digest(nil) = %q, want sha256: and 64 hex digits", got)
	}
}

// TestDecodingIsStrictAboutWhatAnOlderReplicaCannotRead (E5): a stored
// procedure is replayed by whichever replica serves the goal, and a replica
// older than the writer must REFUSE what it cannot check rather than read it
// as something close:
//
//   - a hole class it does not know -- read as nothing in particular, the
//     hole would reach the replay as an unexplained guess;
//   - an object whose keys are not sorted, or repeat -- Bind pairs keys
//     positionally against Obj's sorted order, so either would bind one
//     argument's value into another's position;
//   - a precondition it does not know -- skipped, it would be a predicate the
//     recordings needed and the replay never checked.
func TestDecodingIsStrictAboutWhatAnOlderReplicaCannotRead(t *testing.T) {
	for _, bad := range []string{
		`{"steps":[],"holes":[{"id":"h","stepIndex":0,"path":[],"class":"guessed"}]}`,
		`{"steps":[{"tool":"exec","args":{"kind":"object","keys":["b","a"],"kids":[{"kind":"lit"},{"kind":"lit"}]}}],"holes":[]}`,
		`{"steps":[{"tool":"exec","args":{"kind":"object","keys":["a","a"],"kids":[{"kind":"lit"},{"kind":"lit"}]}}],"holes":[]}`,
	} {
		if _, err := UnmarshalTemplate([]byte(bad)); err == nil {
			t.Errorf("UnmarshalTemplate accepted %s", bad)
		}
	}
	for _, class := range []HoleClass{"", HoleDataFlow, HoleConstant, HoleFree, HoleUnexplained} {
		b, err := MarshalTemplate(Template{Holes: []Hole{{Id: "h", Class: class}}})
		if err != nil {
			t.Fatalf("class %q: %v", class, err)
		}
		if _, err := UnmarshalTemplate(b); err != nil {
			t.Errorf("class %q does not round-trip: %v", class, err)
		}
	}
	if _, err := MarshalTemplate(Template{Holes: []Hole{{Id: "h", Class: "guessed"}}}); err == nil {
		t.Error("MarshalTemplate accepted an unknown hole class")
	}
	unsorted := &Node{Kind: KindObject, Keys: []string{"b", "a"}, Kids: []*Node{Lit("1"), Lit("2")}}
	if _, err := json.Marshal(unsorted); err == nil {
		t.Error("MarshalJSON accepted an object whose keys are not sorted")
	}

	var p Preconditions
	if err := json.Unmarshal([]byte(`{"tools":{"node":"22.1.0"},"kernel":{"min":"6.1"}}`), &p); err == nil {
		t.Error("a precondition this replica cannot check was skipped rather than refused")
	}
	if err := json.Unmarshal([]byte(`{"platform":{"os":"darwin"},"tools":{"node":"22.1.0"},"variables":{"PATH":"unset"},"emptyWorkspace":true}`), &p); err != nil {
		t.Fatalf("every known predicate must still decode: %v", err)
	}
	if p.Tools["node"] != "22.1.0" || p.EmptyWorkspace == nil || !*p.EmptyWorkspace {
		t.Fatalf("decoded %+v", p)
	}
}
