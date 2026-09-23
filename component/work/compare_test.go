package work

// The step comparison a replay is judged by (epic memql#5408, task memql#5410;
// design record D15 "exactly for deterministic actions and by inferred type
// where the successful recordings themselves varied", D16).

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func boolp(b bool) *bool { return &b }
func intp(i int) *int    { return &i }

// Content digests as a file row carries them: lowercase hex, no prefix.
const (
	contentA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	contentB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func wrote(path, digest string) ContentDigest { return ContentDigest{Op: "write", Path: path, Digest: digest} }

// cleanRun is a recording of a command that exited 0, reported no error,
// printed text and wrote one file.
func cleanRun(digest string) StepObservation {
	return StepObservation{IsError: boolp(false), ExitCode: intp(0), ResultType: "string", Contents: []ContentDigest{wrote("out/report.txt", digest)}}
}

// A step every recording agreed on is DETERMINISTIC, and the expectation holds
// every observable exactly -- which is what lets a replay be held to the same
// bytes rather than to the same shape.
func TestExpectationIsExactWhenEveryRecordingAgreed(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{cleanRun(contentA), cleanRun(contentA)})
	want := StepExpectation{
		NoError:    true,
		ExitCode:   intp(0),
		ResultType: "string",
		Contents:   []ContentDigest{wrote("out/report.txt", contentA)},
		Exact:      true,
	}
	if !reflect.DeepEqual(exp, want) {
		t.Fatalf("ExpectationFrom =\n %+v\nwant\n %+v", exp, want)
	}
}

// Where the successful recordings varied -- a report stamped with the time it
// ran -- the content is not the step's contract, its presence and type are.
// Demanding the bytes would fail every replay of a step that was never
// deterministic to begin with.
func TestExpectationFallsBackToTypeWhereRecordingsVaried(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{cleanRun(contentA), cleanRun(contentB)})
	if exp.Exact {
		t.Fatal("recordings that wrote different bytes are not a deterministic step")
	}
	if exp.ResultType != "string" || exp.ExitCode == nil || *exp.ExitCode != 0 || !exp.NoError {
		t.Fatalf("what the recordings DID agree on must still be held: %+v", exp)
	}
	if want := []ContentDigest{wrote("out/report.txt", "")}; !reflect.DeepEqual(exp.Contents, want) {
		t.Fatalf("Contents = %+v, want the write with its digest left open %+v", exp.Contents, want)
	}
}

// An observable some recordings carried and others did not was not agreed on:
// it is neither demanded of a replay nor a sign of determinism. And a write only
// one recording made is not the step's contract.
func TestExpectationHoldsOnlyWhatEveryRecordingCarried(t *testing.T) {
	a := StepObservation{IsError: boolp(false), ExitCode: intp(0), ResultType: "object", Contents: []ContentDigest{wrote("a.txt", contentA)}}
	b := StepObservation{IsError: nil, ExitCode: intp(0), ResultType: "object"}
	exp := ExpectationFrom([]StepObservation{a, b})
	if exp.NoError {
		t.Fatal("one recording never reported its error flag; \"every recording reported no error\" is false")
	}
	if len(exp.Contents) != 0 {
		t.Fatalf("a write only one recording made was kept: %+v", exp.Contents)
	}
	if exp.Exact {
		t.Fatal("recordings that disagree on what they report are not exact")
	}
	if exp.ExitCode == nil || exp.ResultType != "object" {
		t.Fatalf("the exit code and type both recordings agreed on must be held: %+v", exp)
	}
	if got := ExpectationFrom(nil); !reflect.DeepEqual(got, StepExpectation{}) {
		t.Fatalf("no recordings is no expectation, and never an exact one: %+v", got)
	}
}

// The expectation is stored inside the construct's procedure payload, whose
// hash is the version a promotion pins. Contents listed in whatever order the
// recorder happened to produce them would re-hash an unchanged procedure on
// every re-lift, and every re-lift would restart the ladder.
func TestAnExpectationIsTheSameWhateverOrderItsContentsArrivedIn(t *testing.T) {
	x := wrote("x.txt", contentA)
	y := ContentDigest{Op: "read", Path: "y.txt", Digest: contentB}
	one := StepObservation{ExitCode: intp(0), Contents: []ContentDigest{x, y}}
	two := StepObservation{ExitCode: intp(0), Contents: []ContentDigest{y, x}}
	a, _ := json.Marshal(ExpectationFrom([]StepObservation{one, two}))
	b, _ := json.Marshal(ExpectationFrom([]StepObservation{two, one}))
	if string(a) != string(b) {
		t.Fatalf("the expectation depends on arrival order:\n %s\n %s", a, b)
	}
}

// Stored and read back, an ABSENT exit code must stay absent and a ZERO one
// must stay zero: they are different answers (a file write has no exit code; a
// clean command exits 0), and a round trip that merged them would hold every
// write to "exit 0".
func TestAnExpectationKeepsAbsentAndZeroApartThroughItsStoredForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		exp  StepExpectation
	}{
		{"absent exit code", StepExpectation{ResultType: "null"}},
		{"zero exit code", StepExpectation{ExitCode: intp(0), NoError: true, Exact: true}},
	} {
		raw, err := json.Marshal(tc.exp)
		if err != nil {
			t.Fatal(err)
		}
		var back StepExpectation
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, tc.exp) {
			t.Fatalf("%s: round trip through %s gave %+v, want %+v", tc.name, raw, back, tc.exp)
		}
	}
}

// A replay whose command exited differently from every recording did not do
// what the recordings did, whatever its output looked like.
func TestCompareRefusesADifferentExitCode(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{cleanRun(contentA), cleanRun(contentA)})
	got := cleanRun(contentA)
	got.ExitCode = intp(1)
	ok, why := Compare(exp, got)
	if ok {
		t.Fatal("exit 1 against recordings that all exited 0 matched")
	}
	if len(why) == 0 || !strings.Contains(strings.Join(why, "; "), "exit code") {
		t.Fatalf("the reason must name the exit code; got %v", why)
	}
}

func TestCompareRefusesAnErrorWhereTheRecordingsHadNone(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{cleanRun(contentA), cleanRun(contentA)})
	got := cleanRun(contentA)
	got.IsError = boolp(true)
	if ok, why := Compare(exp, got); ok || len(why) == 0 {
		t.Fatalf("an error where every recording had none matched (why=%v)", why)
	}
}

// The side effect is the step. A replay that did not write the file every
// recording wrote has not done the step, and the steps after it would read a
// file that is not there.
func TestCompareRefusesAMissingWrite(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{cleanRun(contentA), cleanRun(contentA)})
	got := cleanRun(contentA)
	got.Contents = nil
	ok, why := Compare(exp, got)
	if ok {
		t.Fatal("a replay that never wrote out/report.txt matched")
	}
	if !strings.Contains(strings.Join(why, "; "), "out/report.txt") {
		t.Fatalf("the reason must name the missing file; got %v", why)
	}
}

// And a deterministic step that wrote the wrong bytes is not the same step.
func TestCompareRefusesADifferentDigestOfADeterministicStep(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{cleanRun(contentA), cleanRun(contentA)})
	if ok, _ := Compare(exp, cleanRun(contentB)); ok {
		t.Fatal("different bytes from a deterministic step matched")
	}
}

func TestCompareAcceptsAnyDigestWhereRecordingsVaried(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{cleanRun(contentA), cleanRun(contentB)})
	const other = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	if ok, why := Compare(exp, cleanRun(other)); !ok {
		t.Fatalf("where the recordings' bytes varied, any bytes will do; refused: %v", why)
	}
}

// A deterministic step's replay that touched a file no recording touched did
// something extra, and an extra side effect is the one kind of divergence that
// cannot be taken back.
func TestAnExtraWriteOfADeterministicStepIsNotAMatch(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{cleanRun(contentA), cleanRun(contentA)})
	got := cleanRun(contentA)
	got.Contents = append(got.Contents, wrote("elsewhere.txt", contentB))
	ok, why := Compare(exp, got)
	if ok {
		t.Fatal("a deterministic step that wrote an extra file matched")
	}
	if !strings.Contains(strings.Join(why, "; "), "elsewhere.txt") {
		t.Fatalf("the reason must name the extra file; got %v", why)
	}
}

// AN ABSENT MEASUREMENT IS NEVER A MATCH (the step concept, D16). A replay
// that did not report what the recordings all reported has not been shown to
// agree with them, and reading silence as agreement is how a broken executor
// certifies itself.
func TestCompareTreatsAnAbsentObservationAsNotAMatch(t *testing.T) {
	exp := StepExpectation{ExitCode: intp(0)}
	ok, why := Compare(exp, StepObservation{ResultType: "string"})
	if ok {
		t.Fatal("an unreported exit code matched an expected one")
	}
	if !reflect.DeepEqual(why, []string{"exit code not reported"}) {
		t.Fatalf("why = %v, want [exit code not reported]", why)
	}
	withDigest := StepExpectation{Contents: []ContentDigest{wrote("a.txt", contentA)}, Exact: true}
	if ok, _ := Compare(withDigest, StepObservation{Contents: []ContentDigest{wrote("a.txt", "")}}); ok {
		t.Fatal("a write reported with no digest matched a digest every recording agreed on")
	}
}

// A step the recordings agreed on NOTHING about cannot be checked, and a step
// that cannot be checked cannot be called deterministic (postcondition.go's
// rule, applied to a replay). An empty expectation must refuse rather than
// wave every replay through.
func TestCompareRefusesAStepTheRecordingsCannotVerify(t *testing.T) {
	varied := ExpectationFrom([]StepObservation{
		{ExitCode: intp(0), ResultType: "string"},
		{ExitCode: intp(1), ResultType: "object"},
	})
	for _, exp := range []StepExpectation{{}, varied} {
		if ok, why := Compare(exp, cleanRun(contentA)); ok || len(why) == 0 {
			t.Fatalf("Compare(%+v) verified a step with nothing to verify it by (why=%v)", exp, why)
		}
		if ok, _ := CompareShadow(exp, cleanRun(contentA), cleanRun(contentA)); ok {
			t.Fatalf("CompareShadow(%+v) promoted evidence a canary could never verify", exp)
		}
	}
}

// The cockpit spells a digest "sha256:<hex>" and a Library file row spells it
// "<hex>". They are one digest, and a comparison that read them as two would
// fail every replay whose two sides were measured by different writers.
func TestADigestReadsTheSameInEitherSpelling(t *testing.T) {
	exp := StepExpectation{Contents: []ContentDigest{wrote("a.txt", contentA)}, Exact: true}
	got := StepObservation{Contents: []ContentDigest{wrote("./a.txt", "sha256:"+strings.ToUpper(contentA))}}
	if ok, why := Compare(exp, got); !ok {
		t.Fatalf("one digest and one path in two spellings were read as different: %v", why)
	}
}

// Shadow on a deterministic step holds the replay to the APP's own result for
// this goal, byte for byte: the same bindings, the same step, the same bytes.
func TestCompareShadowIsExactForADeterministicStep(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{cleanRun(contentA), cleanRun(contentA)})
	if ok, why := CompareShadow(exp, cleanRun(contentA), cleanRun(contentA)); !ok {
		t.Fatalf("a replay that wrote what the app wrote was refused: %v", why)
	}
	ok, why := CompareShadow(exp, cleanRun(contentA), cleanRun(contentB))
	if ok {
		t.Fatal("a deterministic step whose replay wrote different bytes from the app matched")
	}
	if !strings.Contains(strings.Join(why, "; "), "out/report.txt") {
		t.Fatalf("the reason must name the file; got %v", why)
	}
}

// Where the recordings themselves varied, shadow compares by type: the app
// and the replay each wrote a report stamped with a different second, and that
// is the same step.
func TestCompareShadowComparesByTypeWhereTheRecordingsVaried(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{cleanRun(contentA), cleanRun(contentB)})
	if ok, why := CompareShadow(exp, cleanRun(contentA), cleanRun(contentB)); !ok {
		t.Fatalf("different bytes of the same type were refused where the recordings varied: %v", why)
	}
	objectResult := cleanRun(contentB)
	objectResult.ResultType = "object"
	if ok, _ := CompareShadow(exp, cleanRun(contentA), objectResult); ok {
		t.Fatal("a replay that returned an object where the app returned a string matched")
	}
}

// "By type" relaxes the bytes, never the outcome. A replay whose command
// failed where the app's succeeded is a divergence in any step, deterministic
// or not -- counting it as a match would promote a procedure that breaks.
func TestCompareShadowNeverAcceptsAFailedCommandWhereTheAppSucceeded(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{cleanRun(contentA), cleanRun(contentB)})
	failed := cleanRun(contentB)
	failed.ExitCode = intp(2)
	failed.IsError = boolp(true)
	ok, why := CompareShadow(exp, cleanRun(contentA), failed)
	if ok {
		t.Fatal("a failed replay matched a clean app step because the output type agreed")
	}
	joined := strings.Join(why, "; ")
	if !strings.Contains(joined, "exit code") {
		t.Fatalf("the reason must name the exit code; got %v", why)
	}
}

// Shadow holds the replay to what the app did. A file the app wrote that the
// replay did not is missing in either mode; a file only the replay wrote is an
// extra effect of a deterministic step.
func TestCompareShadowHoldsTheReplayToTheAppsEffects(t *testing.T) {
	varied := ExpectationFrom([]StepObservation{cleanRun(contentA), cleanRun(contentB)})
	missing := cleanRun(contentB)
	missing.Contents = nil
	if ok, _ := CompareShadow(varied, cleanRun(contentA), missing); ok {
		t.Fatal("a replay that never wrote the app's file matched")
	}

	exact := ExpectationFrom([]StepObservation{cleanRun(contentA), cleanRun(contentA)})
	extra := cleanRun(contentA)
	extra.Contents = append(extra.Contents, wrote("elsewhere.txt", contentB))
	if ok, _ := CompareShadow(exact, cleanRun(contentA), extra); ok {
		t.Fatal("a deterministic step whose replay wrote an extra file matched")
	}
	if ok, why := CompareShadow(varied, cleanRun(contentA), extra); !ok {
		t.Fatalf("where the recordings' effects varied an extra file is not a divergence: %v", why)
	}
}

// When the app's own observation of the step did not report something, the
// recordings' agreed value stands in for it: the replay is still held to
// something rather than excused by the app's silence.
func TestCompareShadowFallsBackToTheRecordingsWhereTheAppIsSilent(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{cleanRun(contentA), cleanRun(contentA)})
	silent := StepObservation{Contents: []ContentDigest{wrote("out/report.txt", contentA)}}
	bad := cleanRun(contentA)
	bad.ExitCode = intp(1)
	if ok, _ := CompareShadow(exp, silent, bad); ok {
		t.Fatal("exit 1 matched because the app did not report an exit code, although every recording exited 0")
	}
	if ok, why := CompareShadow(exp, silent, cleanRun(contentA)); !ok {
		t.Fatalf("a replay agreeing with the recordings where the app was silent was refused: %v", why)
	}
}

// The cockpit's rule (memql-cockpit internal/worker/harness/result.go
// inferTextType): text that parses as exactly ONE JSON value is typed by what
// it parses as, and anything else -- trailing text, two values, nothing at
// all, a vertical tab that JSON does not call whitespace -- is a string. A
// replay typed by a different rule would disagree with every recording of the
// same output.
func TestInferTextTypeMatchesTheCockpit(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{`{"a":1}`, "object"},
		{`[1]`, "array"},
		{`1`, "number"},
		{`12345678901234567890`, "number"},
		{`1e400`, "number"},
		{`true`, "boolean"},
		{`null`, "null"},
		{`"quoted"`, "string"},
		{`hello`, "string"},
		{`1 2`, "string"},
		{`{"a":1} trailing`, "string"},
		{` {"a":1}` + "\n", "object"},
		{"0\v", "string"},
		{"", "string"},
		{"   ", "string"},
		{`{"a":`, "string"},
	} {
		if got := InferTextType(tc.in); got != tc.want {
			t.Errorf("InferTextType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
