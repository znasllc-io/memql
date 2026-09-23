package procedure

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// execStep is one recorded exec action, canonicalized the way the corpus is.
func execStep(command string) Action {
	return Canonicalize([]Step{{StepType: "exec", Consumed: true, Input: map[string]any{"command": command}}})[0]
}

// echoTemplate is the template two recordings of `echo hi > <file>` generalize
// into: one hole, at the file name.
func echoTemplate(t *testing.T) Template {
	t.Helper()
	tmpl := Generalize([][]Action{{execStep("echo hi > a.txt")}, {execStep("echo hi > b.txt")}})
	if len(tmpl.Holes) != 1 || tmpl.Holes[0].Id != "s0.command.3" {
		t.Fatalf("fixture: Generalize produced holes %+v, want the one hole s0.command.3", tmpl.Holes)
	}
	return tmpl
}

// TestBindReadsEveryHoleOfAnInstanceTheTemplateFits: a shadow comparison
// replays the procedure with the values the APP just used, so binding is the
// question "what did this recording put in each hole".
func TestBindReadsEveryHoleOfAnInstanceTheTemplateFits(t *testing.T) {
	tmpl := echoTemplate(t)
	got, ok := Bind(tmpl, 0, execStep("echo hi > c.txt"))
	if !ok {
		t.Fatal("an instance differing from the recordings only at the hole must bind")
	}
	if want := map[string]string{"s0.command.3": "c.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bindings = %v, want %v", got, want)
	}
}

// TestBindRefusesAnInstanceWhoseLiteralDiffers: a literal position is part of
// what the procedure IS. An instance that differs there is a different action,
// and binding it anyway would compare the procedure against a run it never
// claimed to cover.
func TestBindRefusesAnInstanceWhoseLiteralDiffers(t *testing.T) {
	if got, ok := Bind(echoTemplate(t), 0, execStep("echo bye > c.txt")); ok {
		t.Fatalf("`echo bye` differs from the template at a literal; bound %v", got)
	}
}

func TestBindRefusesADifferentTool(t *testing.T) {
	a := execStep("echo hi > c.txt")
	a.Tool = "fs_write"
	if _, ok := Bind(echoTemplate(t), 0, a); ok {
		t.Fatal("the same arguments under a different tool are a different action")
	}
}

// TestBindRefusesADifferentShape: a template cannot express "absent", so an
// instance with an extra or a missing argument is one the template cannot
// reproduce, whatever its holes would read.
func TestBindRefusesADifferentShape(t *testing.T) {
	tmpl := echoTemplate(t)
	if _, ok := Bind(tmpl, 0, execStep("echo hi > c.txt extra")); ok {
		t.Fatal("an argv one element longer does not fit the template")
	}
	if _, ok := Bind(tmpl, 1, execStep("echo hi > c.txt")); ok {
		t.Fatal("a step index outside the template binds nothing")
	}
}

// TestBindNeverBindsAHoleToASubtree: a hole stands for a literal. A subtree
// has no one string to bind, and a caller that received its JSON would send
// that text where the recording sent a structure.
func TestBindNeverBindsAHoleToASubtree(t *testing.T) {
	tmpl := Template{Steps: []TemplateStep{{Tool: "mcp", Args: Obj(map[string]*Node{"q": HoleNode("s0.q", "mixed")})}}}
	a := Action{Tool: "mcp", Args: Obj(map[string]*Node{"q": Obj(map[string]*Node{"k": Lit("v")})})}
	if _, ok := Bind(tmpl, 0, a); ok {
		t.Fatal("a hole must not bind to an object")
	}
}

// TestBindInstanceRequiresEveryStep: an instance is the WHOLE procedure run
// once. A recording that took only some of its steps did something else.
func TestBindInstanceRequiresEveryStep(t *testing.T) {
	two := Generalize([][]Action{
		{execStep("mkdir out"), execStep("echo hi > out/a.txt")},
		{execStep("mkdir out"), execStep("echo hi > out/b.txt")},
	})
	if _, ok := BindInstance(two, []Action{execStep("mkdir out")}); ok {
		t.Fatal("an instance missing a step must not bind")
	}
	got, ok := BindInstance(two, []Action{execStep("mkdir out"), execStep("echo hi > out/c.txt")})
	if !ok {
		t.Fatal("an instance taking every step must bind")
	}
	if want := map[string]string{"s1.command.3": "out/c.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("bindings = %v, want %v", got, want)
	}
}

// TestBindInstanceRefusesOneHoleBoundTwoWays: a hole id names ONE value. Two
// steps sharing an id and disagreeing about it are not an instance of the
// template.
func TestBindInstanceRefusesOneHoleBoundTwoWays(t *testing.T) {
	shared := Template{Steps: []TemplateStep{
		{Tool: "exec", Args: Obj(map[string]*Node{"x": HoleNode("h", "string")})},
		{Tool: "exec", Args: Obj(map[string]*Node{"x": HoleNode("h", "string")})},
	}}
	a := Action{Tool: "exec", Args: Obj(map[string]*Node{"x": Lit("1")})}
	b := Action{Tool: "exec", Args: Obj(map[string]*Node{"x": Lit("2")})}
	if _, ok := BindInstance(shared, []Action{a, b}); ok {
		t.Fatal("one hole bound to two different values must refuse")
	}
	if got, ok := BindInstance(shared, []Action{a, a}); !ok || got["h"] != "1" {
		t.Fatalf("one hole bound to the same value twice is fine; got %v, %v", got, ok)
	}
}

// materializedCommand runs Materialize over a step's arguments and returns
// its command, failing the test on anything but a string.
func materializedCommand(t *testing.T, args *Node, values map[string]string) string {
	t.Helper()
	v, err := Materialize(args, values)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	cmd, ok := v.(map[string]any)["command"].(string)
	if !ok {
		t.Fatalf("command materialized as %T, want a string: %v", v.(map[string]any)["command"], v)
	}
	return cmd
}

// TestMaterializeRoundTripsARecordedCommand: the replay sends the string, and
// the only claim a canonical tree can support about a string is that it READS
// BACK as the same tree. The exact text is pinned too, because it is what a
// shell will run.
func TestMaterializeRoundTripsARecordedCommand(t *testing.T) {
	a := execStep(`git commit -m "a b"`)
	cmd := materializedCommand(t, a.Args, nil)
	if want := `git commit -m 'a b'`; cmd != want {
		t.Fatalf("command = %q, want %q", cmd, want)
	}
	orig, _ := a.Args.At([]string{"command"})
	if back := Arr(splitArgv(cmd)...); !back.Equal(orig) {
		t.Fatalf("the materialized command does not read back as the recorded tree:\n %q", cmd)
	}
}

// TestMaterializeQuotesExactlyTheArgumentsThatNeedIt pins the quoting rule. An
// argument with whitespace, a quote or a backslash, or an empty one, is single
// quoted (an embedded single quote closes, escapes and reopens); everything
// else is bare -- which keeps a recorded redirect an operator rather than an
// argument.
func TestMaterializeQuotesExactlyTheArgumentsThatNeedIt(t *testing.T) {
	argv := Arr(Lit("printf"), Lit(""), Lit("it's"), Lit(`a"b`), Lit(`c\d`), Lit("tab\there"), Lit(">"), Lit("out.txt"))
	argv.Form = FormArgv
	cmd := materializedCommand(t, Obj(map[string]*Node{"command": argv}), nil)
	want := `printf '' 'it'\''s' 'a"b' 'c\d' 'tab	here' > out.txt`
	if cmd != want {
		t.Fatalf("command = %q\n     want %q", cmd, want)
	}
	if back := Arr(splitArgv(cmd)...); !back.Equal(argv) {
		t.Fatalf("the quoted command does not read back as its arguments: %q", cmd)
	}
}

// TestMaterializeRestoresTheLeadingSlash: the root is the Form, not a segment,
// and a path that lost it would name a file relative to wherever the replay
// happened to run.
func TestMaterializeRestoresTheLeadingSlash(t *testing.T) {
	a := Canonicalize([]Step{{StepType: "fs_write", Consumed: true, Input: map[string]any{
		"path":   "/tmp/x/y",
		"target": "./a/b",
	}}})[0]
	v, err := Materialize(a.Args, nil)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	m := v.(map[string]any)
	if m["path"] != "/tmp/x/y" {
		t.Errorf("rooted path = %q, want /tmp/x/y", m["path"])
	}
	if m["target"] != "./a/b" {
		t.Errorf("relative path = %q, want ./a/b", m["target"])
	}
}

// TestMaterializeWritesBackEveryByteOfAPath is the reason splitPath keeps its
// empty segments: a string that merely starts like a path is split too, and a
// replay writing a source file must write the file that was recorded.
func TestMaterializeWritesBackEveryByteOfAPath(t *testing.T) {
	for _, s := range []string{
		"//cdn.example.com/lib.js",
		"out/",
		"/",
		"// Package main\n// a/b//c\n",
		"./x//y/",
	} {
		a := Canonicalize([]Step{{StepType: "fs_write", Consumed: true, Input: map[string]any{"path": s}}})[0]
		v, err := Materialize(a.Args, nil)
		if err != nil {
			t.Fatalf("%q: Materialize: %v", s, err)
		}
		if got := v.(map[string]any)["path"]; got != s {
			t.Errorf("path %q materialized as %q", s, got)
		}
	}
}

// TestMaterializeReEncodesJSON: a JSON document that arrived as a string goes
// back as JSON text -- keys sorted and compact, because canonicalization kept
// the document's meaning and not its layout -- with nothing HTML-escaped and
// every number exactly as recorded. A float64 would turn an id like
// 12345678901234567890 into a different id.
func TestMaterializeReEncodesJSON(t *testing.T) {
	doc := `{"b": 2, "a": [1, "x > y", null, true], "id": 12345678901234567890, "nested": "{\"k\":1}"}`
	a := Canonicalize([]Step{{StepType: "mcp", Consumed: true, Input: map[string]any{"payload": doc}}})[0]
	v, err := Materialize(a.Args, nil)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	got, ok := v.(map[string]any)["payload"].(string)
	if !ok {
		t.Fatalf("a JSON document must materialize as JSON TEXT; got %T", v.(map[string]any)["payload"])
	}
	want := `{"a":[1,"x > y",null,true],"b":2,"id":12345678901234567890,"nested":"{\"k\":1}"}`
	if got != want {
		t.Fatalf("payload = %s\n     want %s", got, want)
	}
	var gotV, wantV any
	if err := json.Unmarshal([]byte(got), &gotV); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	_ = json.Unmarshal([]byte(doc), &wantV)
	if !reflect.DeepEqual(gotV, wantV) {
		t.Fatalf("the re-encoded document means something else:\n got %v\nwant %v", gotV, wantV)
	}
}

// TestMaterializeKeepsAnArgumentVectorAVector: Codex records argv as a list;
// the replay must send the list, element for element, not a command line.
func TestMaterializeKeepsAnArgumentVectorAVector(t *testing.T) {
	a := Canonicalize([]Step{{StepType: "exec", Consumed: true, Input: map[string]any{
		"command": []any{"bash", "-lc", "git commit -m \"don't\""},
	}}})[0]
	v, err := Materialize(a.Args, nil)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	want := []any{"bash", "-lc", "git commit -m \"don't\""}
	if got := v.(map[string]any)["command"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %#v, want %#v", got, want)
	}
}

func TestMaterializeFillsAHoleWithItsBinding(t *testing.T) {
	tmpl := echoTemplate(t)
	if got := materializedCommand(t, tmpl.Steps[0].Args, map[string]string{"s0.command.3": "c.txt"}); got != "echo hi > c.txt" {
		t.Fatalf("command = %q, want %q", got, "echo hi > c.txt")
	}
	// A value that needs quoting inside a command line gets it.
	if got := materializedCommand(t, tmpl.Steps[0].Args, map[string]string{"s0.command.3": "my file.txt"}); got != "echo hi > 'my file.txt'" {
		t.Fatalf("command = %q, want the binding quoted", got)
	}
}

// TestMaterializeRefusesAnUnboundHoleNamingIt is the pure half of the review
// focus "never run with a hole bound to """: a missing binding is an error,
// never an empty string, and the error names the hole so the refusal can say
// which parameter the goal could not supply.
func TestMaterializeRefusesAnUnboundHoleNamingIt(t *testing.T) {
	_, err := Materialize(echoTemplate(t).Steps[0].Args, map[string]string{})
	if err == nil {
		t.Fatal("an unbound hole must refuse")
	}
	if !strings.Contains(err.Error(), "s0.command.3") {
		t.Fatalf("the error must name the hole; got %q", err)
	}
}

// TestMaterializeTypesANumberLiteral: the replay sends a value to a tool whose
// schema was satisfied by the recording, so a number goes back as a number, a
// bool as a bool and a null as a null.
func TestMaterializeTypesANumberLiteral(t *testing.T) {
	a := Canonicalize([]Step{{StepType: "mcp", Consumed: true, Input: map[string]any{
		"retries": 3.0, "ratio": 0.25, "verbose": true, "name": "x", "cursor": nil,
	}}})[0]
	v, err := Materialize(a.Args, nil)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	want := map[string]any{"retries": float64(3), "ratio": 0.25, "verbose": true, "name": "x", "cursor": nil}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("materialized %#v\n          want %#v", v, want)
	}
}

// TestMaterializeTypesAHoleByWhatItObserved: a hole that only ever held
// numbers is filled with a number, and a binding that is not one refuses
// rather than sending a string where the recordings sent a number.
func TestMaterializeTypesAHoleByWhatItObserved(t *testing.T) {
	args := Obj(map[string]*Node{"limit": HoleNode("s0.limit", "number"), "all": HoleNode("s0.all", "bool")})
	v, err := Materialize(args, map[string]string{"s0.limit": "20", "s0.all": "false"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if want := map[string]any{"limit": float64(20), "all": false}; !reflect.DeepEqual(v, want) {
		t.Fatalf("materialized %#v, want %#v", v, want)
	}
	_, err = Materialize(args, map[string]string{"s0.limit": "twenty", "s0.all": "false"})
	if err == nil || !strings.Contains(err.Error(), "s0.limit") {
		t.Fatalf("a number hole bound to a non-number must refuse naming the hole; got %v", err)
	}
}

// learnInputMapFixture is two recordings of one command writing a file whose
// name the goal's input named.
func learnInputMapFixture() (Template, [][]Action) {
	instances := [][]Action{{execStep("echo hi > a.txt")}, {execStep("echo hi > b.txt")}}
	tmpl := Generalize(instances)
	tmpl.Holes = Classify(tmpl, instances)
	return tmpl, instances
}

// TestAFreeHoleEqualToAGoalInputInEveryInstanceMapsToThatKey: a trusted
// replay has no app to ask, so a free parameter can only come from the goal's
// own input -- and the evidence that input key IS the parameter is that it
// held the parameter's value in every recording.
func TestAFreeHoleEqualToAGoalInputInEveryInstanceMapsToThatKey(t *testing.T) {
	tmpl, instances := learnInputMapFixture()
	got := LearnInputMap(tmpl, instances, []map[string]any{
		{"file": "a.txt", "tone": "calm"},
		{"file": "b.txt", "tone": "calm"},
	})
	if want := map[string]string{"s0.command.3": "file"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("input map = %v, want %v", got, want)
	}
}

// TestAFreeHoleEqualToAnInputInOnlySomeInstancesStaysUnmapped is the negative
// control: a coincidence in one recording is not a mapping, and a wrong
// mapping would replay with a value the goal meant for something else.
func TestAFreeHoleEqualToAnInputInOnlySomeInstancesStaysUnmapped(t *testing.T) {
	tmpl, instances := learnInputMapFixture()
	got := LearnInputMap(tmpl, instances, []map[string]any{
		{"file": "a.txt"},
		{"file": "something-else.txt"},
	})
	if len(got) != 0 {
		t.Fatalf("a key matching one instance of two must not map; got %v", got)
	}
	if got := LearnInputMap(tmpl, instances, []map[string]any{{"file": "a.txt"}}); len(got) != 0 {
		t.Fatalf("fewer inputs than instances cannot show 'every instance'; got %v", got)
	}
}

// TestOnlyFreeHolesAreMapped: a data-flow hole's value comes from an earlier
// step's result at replay time. Mapping it to an input that happened to carry
// the same value would replace the dependency with a guess.
func TestOnlyFreeHolesAreMapped(t *testing.T) {
	mk := func(id string) []Action {
		return []Action{
			{Tool: "exec", Args: Obj(map[string]*Node{"cmd": Lit("mkid")}), ResultValue: map[string]any{"id": id}},
			{Tool: "fs_write", Args: Obj(map[string]*Node{"name": Lit(id)})},
		}
	}
	instances := [][]Action{mk("alpha"), mk("beta")}
	tmpl := Generalize(instances)
	tmpl.Holes = Classify(tmpl, instances)
	if h, _ := holeByPathKey(tmpl.Holes, 1, "name"); h.Class != HoleDataFlow {
		t.Fatalf("fixture: the hole must be data flow; got %+v", h)
	}
	got := LearnInputMap(tmpl, instances, []map[string]any{{"name": "alpha"}, {"name": "beta"}})
	if len(got) != 0 {
		t.Fatalf("a data-flow hole must not be mapped to an input; got %v", got)
	}
}

// TestInputLiteralSpellsAValueAsCanonicalizationDoes: LearnInputMap compares a
// goal input with a canonical literal, and the replay binds the same input
// through the same function. Two spellings of one number would map at
// learning time and miss at replay time.
func TestInputLiteralSpellsAValueAsCanonicalizationDoes(t *testing.T) {
	for _, c := range []struct {
		in   any
		want string
		ok   bool
	}{
		{"a.txt", "a.txt", true},
		{3.0, "3", true},
		{0.5, "0.5", true},
		{json.Number("12345678901234567890"), "12345678901234567890", true},
		{7, "7", true},
		{true, "true", true},
		{nil, "", false},
		{map[string]any{"k": "v"}, "", false},
		{[]any{"x"}, "", false},
	} {
		got, ok := InputLiteral(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("InputLiteral(%#v) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
		if !c.ok {
			continue
		}
		n := canonicalizeValue("x", c.in)
		if n.Kind == KindLit && n.Lit != got {
			t.Errorf("InputLiteral(%#v) = %q but canonicalization spells it %q", c.in, got, n.Lit)
		}
	}
}
