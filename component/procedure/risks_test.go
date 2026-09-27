package procedure

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// risks_test.go -- what a replay may put in a parameter (CheckBindings) and
// what a template must never be promoted with (ReplayRisks).

// holeShapeOf generalizes two recordings of one command and answers the
// shape Classify gave its one hole.
func holeShapeOf(t *testing.T, first, second string) HoleShape {
	t.Helper()
	instances := [][]Action{{execStep(first)}, {execStep(second)}}
	tmpl := Generalize(instances)
	holes := Classify(tmpl, instances)
	if len(holes) != 1 {
		t.Fatalf("%q / %q: holes = %+v, want one", first, second, holes)
	}
	if holes[0].Shape == nil {
		t.Fatalf("%q / %q: Classify recorded no shape", first, second)
	}
	return *holes[0].Shape
}

// TestClassifyRecordsWhatEveryRecordedValueLookedLike (B3): a parameter's
// shape is read off EVERY instance's value at the hole -- an option, an
// absolute path, a home directory, a parent directory -- and, for an argument
// of a command line, off how it was SPELLED: a value that reached the shell
// through an expansion is not the value the program received.
func TestClassifyRecordsWhatEveryRecordedValueLookedLike(t *testing.T) {
	for _, c := range []struct {
		first, second string
		want          HoleShape
	}{
		{"cp a.txt b.txt", "cp a.txt c.txt", HoleShape{}},
		{"ls -l", "ls -a", HoleShape{Dash: true}},
		{"ls x", "ls -a", HoleShape{Dash: true}},
		{"cat /etc/hosts", "cat /etc/passwd", HoleShape{Rooted: true}},
		{"cat ~/a", "cat '~/b'", HoleShape{Home: true, Expands: true}},
		{"cat '~/a'", "cat '~/b'", HoleShape{Home: true}},
		{"cat ../a", "cat b/../c", HoleShape{DotDot: true}},
		{`cat "$HOME/a"`, `cat "$HOME/b"`, HoleShape{Expands: true}},
		{"echo `id`", "echo `date`", HoleShape{Expands: true}},
		{"rm *.txt", "rm *.log", HoleShape{Expands: true}},
		{"cat '$HOME/a'", "cat '$HOME/b'", HoleShape{}},
		{`cat \$HOME/a`, `cat "\$HOME/b"`, HoleShape{}},
		{`grep 'a*' f`, `grep "b?" f`, HoleShape{}},
	} {
		if got := holeShapeOf(t, c.first, c.second); got != c.want {
			t.Errorf("%q / %q: shape = %+v, want %+v", c.first, c.second, got, c.want)
		}
	}
}

// cpTemplateShaped is the cp template with its one parameter's shape set.
func cpTemplateShaped(t *testing.T, shape *HoleShape) (Template, string) {
	t.Helper()
	tmpl, hole := cpTemplate(t)
	tmpl.Holes[0].Shape = shape
	return tmpl, hole
}

// TestCheckBindingsRefusesAValueWithAFeatureNoRecordingHad (B3): strict
// quoting makes a value one argument, and what that argument MEANS is judged
// against what the recordings put there. A destination recorded as plain file
// names does not take an option, an absolute path, a home directory or a
// parent directory -- `cp report.txt -rf` or `cp report.txt ../../x` is a call
// no recording made -- and it is refused in one sentence naming the parameter
// and the value.
func TestCheckBindingsRefusesAValueWithAFeatureNoRecordingHad(t *testing.T) {
	tmpl, hole := cpTemplateShaped(t, &HoleShape{})
	for value, why := range map[string]string{
		"-rf":         "was never recorded as an option",
		"/etc/passwd": "was never recorded as an absolute path",
		"~/x":         "was never recorded under a home directory",
		"..":          "was never recorded climbing to a parent directory",
		"a/../../b":   "was never recorded climbing to a parent directory",
	} {
		err := CheckBindings(tmpl, map[string]string{hole: value})
		if err == nil {
			t.Errorf("%q: accepted", value)
			continue
		}
		want := "parameter " + hole + " " + why + ", and this goal gave it " + strconvQuote(value)
		if err.Error() != want {
			t.Errorf("%q: error = %q\n          want %q", value, err, want)
		}
	}
	for _, value := range []string{"x;rm${IFS}-rf${IFS}$HOME", "$(id)", "`id`", "R&D.txt", "*.txt", "a|b", "dest3.txt", "a-b", "x/y"} {
		if err := CheckBindings(tmpl, map[string]string{hole: value}); err != nil {
			t.Errorf("%q: %v -- strict quoting already makes it one plain argument", value, err)
		}
	}
	if err := CheckBindings(tmpl, map[string]string{"s9.unknown": "-rf"}); err != nil {
		t.Errorf("a value for no hole of the template is not this function's to judge: %v", err)
	}
}

// strconvQuote is the value as the refusal quotes it.
func strconvQuote(s string) string { return strconv.Quote(s) }

// TestCheckBindingsAllowsWhatTheRecordingsShowed: a parameter recorded as an
// option takes one, and so on for each feature -- the refusal is about what
// no recording showed, not about the feature itself.
func TestCheckBindingsAllowsWhatTheRecordingsShowed(t *testing.T) {
	for value, shape := range map[string]HoleShape{
		"-rf":         {Dash: true},
		"/etc/passwd": {Rooted: true},
		"~/x":         {Home: true},
		"../x":        {DotDot: true},
	} {
		tmpl, hole := cpTemplateShaped(t, &shape)
		if err := CheckBindings(tmpl, map[string]string{hole: value}); err != nil {
			t.Errorf("%q with shape %+v: %v", value, shape, err)
		}
	}
}

// TestAParameterRecordedThroughAnExpansionTakesNoValue (B3): when a recorded
// value reached the command through a shell expansion, the value recorded is
// not the value the program received -- so there is no value a goal could give
// that would stand for it, and every one is refused.
func TestAParameterRecordedThroughAnExpansionTakesNoValue(t *testing.T) {
	tmpl, hole := cpTemplateShaped(t, &HoleShape{Expands: true})
	err := CheckBindings(tmpl, map[string]string{hole: "dest3.txt"})
	if err == nil || !strings.Contains(err.Error(), hole) || !strings.Contains(err.Error(), `"dest3.txt"`) ||
		!strings.Contains(err.Error(), "expansion") {
		t.Fatalf("CheckBindings = %v, want a refusal naming the parameter, the value and the expansion", err)
	}
}

// TestAParameterWithNoShapeIsJudgedAsHavingNone: a payload written before
// shapes carries none, and it is judged as if every recording had shown no
// feature at all -- the direction that refuses.
func TestAParameterWithNoShapeIsJudgedAsHavingNone(t *testing.T) {
	tmpl, hole := cpTemplateShaped(t, nil)
	if err := CheckBindings(tmpl, map[string]string{hole: "-rf"}); err == nil {
		t.Fatal("an unshaped parameter accepted an option")
	}
	if err := CheckBindings(tmpl, map[string]string{hole: "dest3.txt"}); err != nil {
		t.Fatalf("an unshaped parameter refused a plain name: %v", err)
	}
}

// TestAHoleShapeIsStoredAndReadBackStrictly: the shape travels in the stored
// template, omitted when absent, and a feature this replica does not know is
// refused rather than dropped: a newer writer records a feature because it
// CHECKS values against it, and a replica that dropped the feature would bind
// values that check refuses.
func TestAHoleShapeIsStoredAndReadBackStrictly(t *testing.T) {
	in := Template{Holes: []Hole{
		{Id: "a", Class: HoleFree, Shape: &HoleShape{Dash: true, Rooted: true, Home: true, DotDot: true, Expands: true}},
		{Id: "b", Class: HoleFree, Shape: &HoleShape{}},
		{Id: "c", Class: HoleFree},
	}}
	b, err := MarshalTemplate(in)
	if err != nil {
		t.Fatalf("MarshalTemplate: %v", err)
	}
	want := `{"steps":null,"holes":[` +
		`{"id":"a","stepIndex":0,"path":null,"class":"free","shape":{"dash":true,"rooted":true,"home":true,"dotDot":true,"expands":true}},` +
		`{"id":"b","stepIndex":0,"path":null,"class":"free","shape":{}},` +
		`{"id":"c","stepIndex":0,"path":null,"class":"free"}]}`
	if string(b) != want {
		t.Fatalf("wire shape:\n got %s\nwant %s", b, want)
	}
	out, err := UnmarshalTemplate(b)
	if err != nil {
		t.Fatalf("UnmarshalTemplate: %v", err)
	}
	if !reflect.DeepEqual(out.Holes, in.Holes) {
		t.Fatalf("holes changed on a round trip:\n got %+v\nwant %+v", out.Holes, in.Holes)
	}
	if _, err := UnmarshalTemplate([]byte(`{"steps":[],"holes":[{"id":"a","stepIndex":0,"path":[],"class":"free","shape":{"glob":true}}]}`)); err == nil {
		t.Fatal("a shape feature this replica does not know was dropped rather than refused")
	}
}

// risksOf generalizes recordings -- each one step, an exec of the command --
// classifies the template, and answers ReplayRisks over the instances it was
// generalized from.
func risksOf(t *testing.T, commands ...any) []string {
	t.Helper()
	var instances [][]Action
	for _, c := range commands {
		instances = append(instances, Canonicalize([]Step{{StepType: "exec", Consumed: true, Input: map[string]any{"command": c}}}))
	}
	tmpl := Generalize(instances)
	tmpl.Holes = Classify(tmpl, instances)
	return ReplayRisks(tmpl, instances)
}

// oneRiskSaying asserts exactly one risk, carrying every fragment.
func oneRiskSaying(t *testing.T, risks []string, fragments ...string) {
	t.Helper()
	if len(risks) != 1 || !containsEvery(risks[0], fragments...) {
		t.Fatalf("risks = %q, want one saying %q", risks, fragments)
	}
}

func containsEvery(s string, fragments ...string) bool {
	for _, f := range fragments {
		if !strings.Contains(s, f) {
			return false
		}
	}
	return true
}

// TestATemplateWhoseParametersAreDataHasNoReplayRisk is the control every
// risk below is measured against: a parameter that is an ordinary argument --
// strictly quoted, one word, judged by its shape -- is what a procedure is
// FOR.
func TestATemplateWhoseParametersAreDataHasNoReplayRisk(t *testing.T) {
	for _, pair := range [][2]any{
		{"cp report.txt dest1.txt", "cp report.txt dest2.txt"},
		{"mkdir -p out && echo hello > a.txt", "mkdir -p out && echo hello > b.txt"},
		{"bash script.sh alpha", "bash script.sh beta"},
		{"cat <<'EOF' > notes.txt\nhello\nEOF\ncp notes.txt a.txt", "cat <<'EOF' > notes.txt\nhello\nEOF\ncp notes.txt b.txt"},
		{[]any{"git", "commit", "-m", "first"}, []any{"git", "commit", "-m", "second"}},
		{[]any{"bash", "-lc", "echo a > x"}, []any{"bash", "-lc", "echo b > x"}},
		{[]any{"sudo", "bash", "-c", "cp report.txt out/a.txt"}, []any{"sudo", "bash", "-c", "cp report.txt out/b.txt"}},
	} {
		if risks := risksOf(t, pair[0], pair[1]); len(risks) != 0 {
			t.Errorf("%q / %q: risks = %q, want none", pair[0], pair[1], risks)
		}
	}
}

// TestATemplateThatCannotBindItsOwnRecordingIsARisk (B4 a): the template is
// the generalization of its instances, so it must bind every one of them. One
// it cannot -- recordings whose argument lists differ in length leave a gap
// hole that only some of them fill -- cannot reproduce what that recording
// did, and a replay compared against the others would be compared against a
// procedure the recordings never showed.
func TestATemplateThatCannotBindItsOwnRecordingIsARisk(t *testing.T) {
	oneRiskSaying(t, risksOf(t, "cp a.txt b.txt", "cp -r a.txt b.txt"), "instance 0", "step 0")
}

// TestAParameterRecordedThroughAnExpansionIsARisk (B4 b).
func TestAParameterRecordedThroughAnExpansionIsARisk(t *testing.T) {
	oneRiskSaying(t, risksOf(t, `cat "$HOME/a.txt"`, `cat "$HOME/b.txt"`), "s0.command.1", "expansion")
}

// TestAParameterThatIsCodeIsARisk (B4 c): strict quoting makes a value one
// argument, and there are arguments a program RUNS -- a shell's -c script, an
// interpreter's inline code, eval's arguments. A goal's input there chooses
// the code, quoted or not. The same holds for a command vector, which is how
// Codex records every command, and through a wrapper like sudo.
func TestAParameterThatIsCodeIsARisk(t *testing.T) {
	for _, c := range []struct {
		first, second any
		says          []string
	}{
		{`bash -c "echo a > x"`, `bash -c "echo b > x"`, []string{"script", "bash"}},
		{`sh -ec 'make a'`, `sh -ec 'make b'`, []string{"script", "sh"}},
		{`/bin/zsh -lc "go test ./a"`, `/bin/zsh -lc "go test ./b"`, []string{"script", "zsh"}},
		{`sudo sh -c "echo a"`, `sudo sh -c "echo b"`, []string{"script", "sh"}},
		{`python3 -c "print(1)"`, `python3 -c "print(2)"`, []string{"code", "python3"}},
		{`node --eval "f(1)"`, `node --eval "f(2)"`, []string{"code", "node"}},
		{`node -p "a"`, `node -p "b"`, []string{"code", "node"}},
		{`perl -e 'print 1'`, `perl -e 'print 2'`, []string{"code", "perl"}},
		{`ruby -e 'puts 1'`, `ruby -e 'puts 2'`, []string{"code", "ruby"}},
		{`eval "echo a"`, `eval "echo b"`, []string{"eval"}},
	} {
		risks := risksOf(t, c.first, c.second)
		if len(risks) == 0 || !containsEvery(risks[0], append([]string{"code that runs"}, c.says...)...) {
			t.Errorf("%q / %q: risks = %q, want one naming %q", c.first, c.second, risks, c.says)
		}
	}
}

// TestAParameterThatIsTheCommandWordIsARisk: a parameter where a shell expects
// a command word chooses the PROGRAM, however it is quoted -- the principle
// replayable.go holds argv[0] to, applied to every simple command of a line.
func TestAParameterThatIsTheCommandWordIsARisk(t *testing.T) {
	oneRiskSaying(t, risksOf(t, "mkdir -p out && touch out/x", "mkdir -p out && rm out/x"), "s0.command.4", "program")
}

// TestAParameterInsideAHereDocumentIsARisk (B4 d): a here-document's body is
// text the shell hands the command -- expanding $ and backticks in it when the
// delimiter is unquoted -- and a value's quotes are just more text there, so a
// parameter in one is not one argument. A parameter after a here-document
// operator on its line is flagged too; one on a line after the document
// ended is an ordinary argument (the control above).
func TestAParameterInsideAHereDocumentIsARisk(t *testing.T) {
	oneRiskSaying(t, risksOf(t, "cat <<EOF > f\nhello alpha\nEOF", "cat <<EOF > f\nhello beta\nEOF"), "s0.command.5", "here-document")
	oneRiskSaying(t, risksOf(t, "cat <<< alpha", "cat <<< beta"), "s0.command.2", "here-document")
}

// TestReplayRisksAreInStepOrder: the first sentence is the ladder's reason, so
// the order is the procedure's own.
func TestReplayRisksAreInStepOrder(t *testing.T) {
	mk := func(a, b string) []Action {
		return Canonicalize([]Step{
			{StepType: "exec", Consumed: true, Input: map[string]any{"command": `bash -c "echo ` + a + `"`}},
			{StepType: "exec", Consumed: true, Input: map[string]any{"command": `cat "$HOME/` + b + `"`}},
		})
	}
	instances := [][]Action{mk("a", "x"), mk("b", "y")}
	tmpl := Generalize(instances)
	tmpl.Holes = Classify(tmpl, instances)
	risks := ReplayRisks(tmpl, instances)
	if len(risks) != 2 || !strings.Contains(risks[0], "step 0") || !strings.Contains(risks[1], "step 1") {
		t.Fatalf("risks = %q, want step 0's then step 1's", risks)
	}
}

// codexStep is one Codex exec step: its command the vector
// ["bash", "-lc", script].
func codexStep(script string) []Action {
	return Canonicalize([]Step{{StepType: "exec", Consumed: true, Input: map[string]any{"command": []any{"bash", "-lc", script}}}})
}

// TestACodexProcedureIsAParameterInsideItsScript: two Codex recordings that
// wrote out/a.txt and out/b.txt generalize to ONE template whose one hole is
// the file inside the script -- not the script. It has no replay risk, a new
// value goes back into the script as one word, a hostile one as one QUOTED
// word, and the vector still materializes as [shell, flags, "<script>"], so no
// dispatcher changes.
func TestACodexProcedureIsAParameterInsideItsScript(t *testing.T) {
	instances := [][]Action{codexStep("cp report.txt out/a.txt"), codexStep("cp report.txt out/b.txt")}
	tmpl := Generalize(instances)
	tmpl.Holes = Classify(tmpl, instances)
	if len(tmpl.Holes) != 1 || tmpl.Holes[0].Id != "s0.command.2.2" || tmpl.Holes[0].Class != HoleFree {
		t.Fatalf("holes = %+v, want the one free hole s0.command.2.2 -- the file inside the script", tmpl.Holes)
	}
	if risks := ReplayRisks(tmpl, instances); len(risks) != 0 {
		t.Fatalf("risks = %q, want none: the parameter is a word of the script, not the script", risks)
	}
	materialized := func(value string) []any {
		t.Helper()
		v, err := Materialize(tmpl.Steps[0].Args, map[string]string{"s0.command.2.2": value})
		if err != nil {
			t.Fatalf("Materialize(%q): %v", value, err)
		}
		vec, ok := v.(map[string]any)["command"].([]any)
		if !ok {
			t.Fatalf("command materialized as %T, want the vector", v.(map[string]any)["command"])
		}
		return vec
	}
	if got, want := materialized("out/c.txt"), []any{"bash", "-lc", "cp report.txt out/c.txt"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %q, want %q", got, want)
	}
	hostile := "x;rm${IFS}-rf${IFS}$HOME"
	got := materialized(hostile)
	if want := []any{"bash", "-lc", `cp report.txt 'x;rm${IFS}-rf${IFS}$HOME'`}; !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %q, want %q", got, want)
	}
	if words := lits(splitArgv(got[2].(string))); !reflect.DeepEqual(words, []string{"cp", "report.txt", hostile}) {
		t.Fatalf("the script reads back as %q, want the hostile value as ONE argument", words)
	}
	if b, ok := BindInstance(tmpl, codexStep("cp report.txt out/d.txt")); !ok || b["s0.command.2.2"] != "out/d.txt" {
		t.Fatalf("a third recording does not bind: %v, %v", b, ok)
	}
}

// TestACodexScriptWithNoParameterRoundTripsByteForByte: the script is read as
// a command line WITH its spelling, so a vector the template holds whole goes
// back exactly as recorded -- heredoc, expansions and all.
func TestACodexScriptWithNoParameterRoundTripsByteForByte(t *testing.T) {
	script := "git commit -m \"$(cat <<'EOF'\nAdd the replay runner\n\nIt serves a goal with no model.\nEOF\n)\" && echo \"done $HOME\""
	a := codexStep(script)[0]
	v, err := Materialize(a.Args, nil)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if got, want := v.(map[string]any)["command"], []any{"bash", "-lc", script}; !reflect.DeepEqual(got, want) {
		t.Fatalf("command = %q\n     want %q", got, want)
	}
}

// TestACodexScriptJudgesItsParametersLikeAnyCommandLine: inside the script a
// parameter is judged exactly as in a command line -- a program, inline code,
// an expansion are still risks -- and scripts that differ in SHAPE, not in a
// word, still leave a template no replay can be trusted with.
func TestACodexScriptJudgesItsParametersLikeAnyCommandLine(t *testing.T) {
	for _, c := range []struct {
		first, second string
		says          []string
	}{
		{"touch out/x", "rm out/x", []string{"s0.command.2.0", "program"}},
		{`python3 -c 'print(1)'`, `python3 -c 'print(2)'`, []string{"s0.command.2.2", "code python3 runs"}},
		{`cat "$HOME/a"`, `cat "$HOME/b"`, []string{"s0.command.2.1", "expansion"}},
		{"echo a > x", "make build", []string{"does not bind"}},
	} {
		instances := [][]Action{codexStep(c.first), codexStep(c.second)}
		tmpl := Generalize(instances)
		tmpl.Holes = Classify(tmpl, instances)
		risks := ReplayRisks(tmpl, instances)
		if len(risks) == 0 || !containsEvery(strings.Join(risks, " | "), c.says...) {
			t.Errorf("%q / %q: risks = %q, want one saying %q", c.first, c.second, risks, c.says)
		}
	}
}

// TestAStoredVectorWhoseWholeScriptIsAParameterIsStillAScriptRisk: a payload
// written before scripts were read -- or a template whose scripts were of
// different kinds -- holds the whole script as one hole, and that is still
// code a goal would choose.
func TestAStoredVectorWhoseWholeScriptIsAParameterIsStillAScriptRisk(t *testing.T) {
	tmpl := Template{Steps: []TemplateStep{{Tool: "exec", Args: Obj(map[string]*Node{
		"command": Arr(Lit("bash"), Lit("-lc"), HoleNode("s0.command.2", "string")),
	})}}}
	oneRiskSaying(t, ReplayRisks(tmpl, nil), "s0.command.2", "script bash runs")
}
