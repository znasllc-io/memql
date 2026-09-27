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
