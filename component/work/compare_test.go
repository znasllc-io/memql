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

func wrote(path, digest string) ContentDigest {
	return ContentDigest{Op: "write", Path: path, Digest: digest}
}

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

// The stored keys are a contract: a replay, the OS and the proving driver read
// them, and construct.procedureHash is computed over them. Every key a later
// change added is omitempty, so an expectation that does not use it is stored
// -- and hashed -- exactly as before, and a row written before the key existed
// reads back as it did.
func TestTheStoredExpectationKeysArePinned(t *testing.T) {
	for _, tc := range []struct {
		name string
		exp  StepExpectation
		want string
	}{
		{
			"the original keys, unchanged by every later one",
			StepExpectation{NoError: true, ExitCode: intp(0), ResultType: "string", Contents: []ContentDigest{wrote("a.txt", contentA)}, Exact: true},
			`{"noError":true,"exitCode":0,"resultType":"string","contents":[{"op":"write","path":"a.txt","digest":"` + contentA + `"}],"exact":true}`,
		},
		{
			"a failure every recording agreed on",
			StepExpectation{Error: true, ResultType: "string", Exact: true},
			`{"error":true,"resultType":"string","exact":true}`,
		},
		{
			"a write whose path varied",
			StepExpectation{NoError: true, MinWrites: 1},
			`{"noError":true,"minWrites":1}`,
		},
	} {
		raw, err := json.Marshal(tc.exp)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, raw, tc.want)
		}
		var back StepExpectation
		if err := json.Unmarshal([]byte(tc.want), &back); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back, tc.exp) {
			t.Errorf("%s: %s read back as %+v, want %+v", tc.name, tc.want, back, tc.exp)
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

// notFound is a lookup that failed in the recording -- the "not found" a
// procedure goes on to act on by creating the thing.
func notFound() StepObservation { return StepObservation{IsError: boolp(true), ResultType: "string"} }

// The other direction. A step every recording saw FAIL is a step whose failure
// is part of the procedure, and for an mcp or fetch step the error flag and
// the result type are all a replay can report. A replay in which the lookup
// SUCCEEDED has not done what the recordings did: the steps after it would
// run on a state no recording saw.
func TestCompareRefusesACleanReplayOfAStepEveryRecordingSawFail(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{notFound(), notFound()})
	ok, why := Compare(exp, StepObservation{IsError: boolp(false), ResultType: "string"})
	if ok {
		t.Fatal("a clean replay matched a step every recording saw fail")
	}
	if !strings.Contains(strings.Join(why, "; "), "no error where every recording had one") {
		t.Fatalf("the reason must say the replay did not fail where every recording did; got %v", why)
	}
	if ok, why := Compare(exp, notFound()); !ok {
		t.Fatalf("a replay that failed where every recording failed was refused: %v", why)
	}
	if ok, _ := Compare(exp, StepObservation{ResultType: "string"}); ok {
		t.Fatal("a replay that never reported its error flag matched a step every recording saw fail")
	}
}

// The flag is held only where every recording reported it, one way. A flag the
// recordings split on, or one a recording never reported, is held in neither
// direction -- and is not a sign of determinism either.
func TestAnExpectationHoldsTheErrorFlagOnlyWhereEveryRecordingAgreed(t *testing.T) {
	clean := StepObservation{IsError: boolp(false), ResultType: "string"}
	silent := StepObservation{ResultType: "string"}
	for _, tc := range []struct {
		name                   string
		recordings             []StepObservation
		noError, failed, exact bool
	}{
		{"every recording failed", []StepObservation{notFound(), notFound()}, false, true, true},
		{"every recording was clean", []StepObservation{clean, clean}, true, false, true},
		{"the recordings split", []StepObservation{notFound(), clean}, false, false, false},
		{"one recording never said", []StepObservation{notFound(), silent}, false, false, false},
	} {
		exp := ExpectationFrom(tc.recordings)
		if exp.NoError != tc.noError || exp.Error != tc.failed || exp.Exact != tc.exact {
			t.Errorf("%s: NoError=%v Error=%v Exact=%v, want NoError=%v Error=%v Exact=%v",
				tc.name, exp.NoError, exp.Error, exp.Exact, tc.noError, tc.failed, tc.exact)
		}
	}
}

// ExpectationFrom never writes both flags, but a stored row is a value anyone
// could have written. One claiming both is a contradiction no replay can meet,
// and it must refuse every replay rather than be read whichever way suits it.
func TestAnExpectationClaimingBothFlagsRefusesEveryReplay(t *testing.T) {
	both := StepExpectation{NoError: true, Error: true, ResultType: "string"}
	for _, flag := range []bool{false, true} {
		if ok, _ := Compare(both, StepObservation{IsError: boolp(flag), ResultType: "string"}); ok {
			t.Fatalf("isError=%v met an expectation claiming both a clean and a failed step", flag)
		}
	}
}

// An agreed failure is something the recordings agreed on, so a step whose
// only agreed observable is its failure is verifiable -- by that failure --
// rather than refused as a step nobody can check.
func TestAnAgreedFailureAloneMakesAStepVerifiable(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{{IsError: boolp(true)}, {IsError: boolp(true)}})
	if ok, why := Compare(exp, StepObservation{IsError: boolp(true)}); !ok {
		t.Fatalf("a failure every recording agreed on was not enough to verify the step: %v", why)
	}
	if ok, _ := Compare(exp, StepObservation{IsError: boolp(false)}); ok {
		t.Fatal("a clean replay matched a step whose one agreed observable is its failure")
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

// reportFor is a recording of a step that wrote the report its own goal named:
// the path is a parameter, so no two recordings wrote the same file.
func reportFor(month, digest string) StepObservation {
	return StepObservation{IsError: boolp(false), Contents: []ContentDigest{wrote("reports/"+month+".txt", digest)}}
}

// A write whose path is a parameter is in no two recordings' intersection, so
// Contents cannot hold it. Without a count the step would expect no write at
// all, and a canary or trusted replay that reported NONE would match -- an
// absent measurement read as agreement, the one reading this file refuses.
func TestAWriteWhosePathVariedIsStillExpected(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{reportFor("2026-07", contentA), reportFor("2026-08", contentB)})
	ok, why := Compare(exp, StepObservation{IsError: boolp(false)})
	if ok {
		t.Fatal("a replay that reported no write matched a step in which every recording wrote a file")
	}
	if !reflect.DeepEqual(why, []string{"reported no write where every recording wrote at least 1 file"}) {
		t.Fatalf("why = %v, want the missing write named against what every recording wrote", why)
	}
	if ok, why := Compare(exp, reportFor("2026-09", "")); !ok {
		t.Fatalf("a replay that wrote this goal's own report was refused: %v", why)
	}

	// The floor alone verifies a step: recordings that reported no error
	// flag still agreed that the step writes a file.
	unflagged := ExpectationFrom([]StepObservation{
		{Contents: []ContentDigest{wrote("x.txt", contentA)}},
		{Contents: []ContentDigest{wrote("y.txt", contentA)}},
	})
	if ok, why := Compare(unflagged, StepObservation{Contents: []ContentDigest{wrote("z.txt", "")}}); !ok {
		t.Fatalf("a step whose one agreed observable is that it writes was not verifiable by a write: %v", why)
	}
	if ok, _ := Compare(unflagged, StepObservation{}); ok {
		t.Fatal("a replay that reported nothing matched a step every recording wrote a file in")
	}
}

// The floor counts EVERY write, the files the recordings agreed on included:
// each recording wrote the shared log and a report of its own, so a replay
// that wrote the log alone did less than any of them. And a file reported
// twice is one write, the way contentIndex reads it.
func TestAReplayWithFewerWritesThanEveryRecordingIsNotAMatch(t *testing.T) {
	logged := func(month, digest string) StepObservation {
		r := reportFor(month, digest)
		r.Contents = append(r.Contents, wrote("run.log", contentA))
		return r
	}
	exp := ExpectationFrom([]StepObservation{logged("2026-07", contentA), logged("2026-08", contentB)})
	onlyLog := StepObservation{IsError: boolp(false), Contents: []ContentDigest{wrote("run.log", contentA)}}
	ok, why := Compare(exp, onlyLog)
	if ok {
		t.Fatal("a replay that wrote the log alone matched recordings that each wrote the log and a report")
	}
	if !reflect.DeepEqual(why, []string{"reported 1 write where every recording wrote at least 2 files"}) {
		t.Fatalf("why = %v, want the one write named against the two every recording made", why)
	}
	twice := onlyLog
	twice.Contents = append(twice.Contents, wrote("./run.log", contentA))
	if ok, _ := Compare(exp, twice); ok {
		t.Fatal("one file reported twice counted as two writes")
	}
	if ok, why := Compare(exp, logged("2026-09", contentB)); !ok {
		t.Fatalf("a replay that wrote the log and its own report was refused: %v", why)
	}
}

// The floor is the FEWEST writes any one recording made, counted as distinct
// files, and it is stated only where the recordings did not all write the same
// files -- where they did, Contents already holds every one of them.
func TestMinWritesIsTheFewestWritesWhereTheWrittenFilesVaried(t *testing.T) {
	read := func(p string) ContentDigest { return ContentDigest{Op: "read", Path: p, Digest: contentA} }
	did := func(cs ...ContentDigest) StepObservation { return StepObservation{IsError: boolp(false), Contents: cs} }
	for _, tc := range []struct {
		name       string
		recordings []StepObservation
		want       int
	}{
		{"the same file every time", []StepObservation{did(wrote("a.txt", contentA)), did(wrote("a.txt", contentB))}, 0},
		{"a different file every time", []StepObservation{did(wrote("x.txt", contentA)), did(wrote("y.txt", contentA))}, 1},
		{"two files, then three", []StepObservation{
			did(wrote("x.txt", contentA), wrote("y.txt", contentA)),
			did(wrote("x.txt", contentA), wrote("z.txt", contentA), wrote("w.txt", contentA)),
		}, 2},
		{"one recording wrote a file the other did not", []StepObservation{
			did(wrote("a.txt", contentA)),
			did(wrote("a.txt", contentA), wrote("b.txt", contentA)),
		}, 1},
		{"a recording that wrote nothing sets no floor", []StepObservation{did(), did(wrote("a.txt", contentA))}, 0},
		{"one file reported twice is one write", []StepObservation{
			did(wrote("x.txt", contentA), wrote("./x.txt", contentA)),
			did(wrote("y.txt", contentA), wrote("z.txt", contentA)),
		}, 1},
		{"reads that varied are not writes", []StepObservation{did(read("x.txt")), did(read("y.txt"))}, 0},
	} {
		if got := ExpectationFrom(tc.recordings).MinWrites; got != tc.want {
			t.Errorf("%s: MinWrites = %d, want %d", tc.name, got, tc.want)
		}
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
	ok, why := CompareShadow(exp, silent, bad)
	if ok {
		t.Fatal("exit 1 matched because the app did not report an exit code, although every recording exited 0")
	}
	// The reason names what the replay was held to: the app reported no exit
	// code, so "where the app exited 0" would be a claim about nothing.
	if !reflect.DeepEqual(why, []string{"exit code 1 where every recording exited 0"}) {
		t.Fatalf("why = %v, want the exit code held to every recording's", why)
	}
	if ok, why := CompareShadow(exp, silent, cleanRun(contentA)); !ok {
		t.Fatalf("a replay agreeing with the recordings where the app was silent was refused: %v", why)
	}
}

// Shadow holds the error flag to the APP's, exactly, in both directions and
// whatever the recordings agreed on: the app is the reference for this goal,
// and a replay that failed where it succeeded, or succeeded where it failed,
// did not do what the app did.
func TestCompareShadowHoldsTheAppsErrorFlagExactly(t *testing.T) {
	clean := StepObservation{IsError: boolp(false), ResultType: "string"}
	for _, tc := range []struct {
		name string
		exp  StepExpectation
	}{
		{"every recording failed", ExpectationFrom([]StepObservation{notFound(), notFound()})},
		{"every recording was clean", ExpectationFrom([]StepObservation{clean, clean})},
		{"the recordings split", ExpectationFrom([]StepObservation{notFound(), clean})},
	} {
		if ok, why := CompareShadow(tc.exp, notFound(), notFound()); !ok {
			t.Errorf("%s: a replay that failed where the app failed was refused: %v", tc.name, why)
		}
		if ok, why := CompareShadow(tc.exp, clean, clean); !ok {
			t.Errorf("%s: a clean replay beside a clean app was refused: %v", tc.name, why)
		}
		if ok, _ := CompareShadow(tc.exp, notFound(), clean); ok {
			t.Errorf("%s: a clean replay matched an app that failed", tc.name)
		}
		if ok, _ := CompareShadow(tc.exp, clean, notFound()); ok {
			t.Errorf("%s: a failed replay matched an app that succeeded", tc.name)
		}
	}
}

// Shadow holds the same floor. Beside an app that did not report the file it
// wrote, the recordings' count is the only thing saying the step writes at
// all, and a replay that reported no write is held to it rather than excused
// by the app's silence. The floor is the procedure's own, so it holds beside
// an app that did less, too: a canary replay of the same goal would be refused
// by it, and shadow must not count as evidence what a canary refuses.
func TestCompareShadowHoldsTheWriteFloor(t *testing.T) {
	exp := ExpectationFrom([]StepObservation{reportFor("2026-07", contentA), reportFor("2026-08", contentB)})
	silent := StepObservation{IsError: boolp(false)}
	ok, why := CompareShadow(exp, silent, StepObservation{IsError: boolp(false)})
	if ok {
		t.Fatal("a replay that reported no write matched because the app did not report the write every recording made")
	}
	if !strings.Contains(strings.Join(why, "; "), "every recording wrote at least 1 file") {
		t.Fatalf("the reason must name the recordings' floor; got %v", why)
	}
	if ok, why := CompareShadow(exp, silent, reportFor("2026-09", contentA)); !ok {
		t.Fatalf("a replay that wrote this goal's report beside a silent app was refused: %v", why)
	}
	if ok, why := CompareShadow(exp, reportFor("2026-09", contentA), reportFor("2026-09", contentB)); !ok {
		t.Fatalf("a replay that wrote the app's own file was refused: %v", why)
	}

	logged := func(month string) StepObservation {
		r := reportFor(month, contentA)
		r.Contents = append(r.Contents, wrote("run.log", contentA))
		return r
	}
	both := ExpectationFrom([]StepObservation{logged("2026-07"), logged("2026-08")})
	onlyLog := StepObservation{IsError: boolp(false), Contents: []ContentDigest{wrote("run.log", contentA)}}
	if ok, _ := CompareShadow(both, onlyLog, onlyLog); ok {
		t.Fatal("a replay below the recordings' floor matched because the app did less too")
	}
}

// Where the app did not report its flag, the recordings' agreed flag stands in
// -- in either direction -- and the reason names the recordings, since the
// app said nothing to differ from.
func TestCompareShadowHoldsTheRecordingsErrorFlagWhereTheAppIsSilent(t *testing.T) {
	clean := StepObservation{IsError: boolp(false), ResultType: "string"}
	silent := StepObservation{ResultType: "string"}
	failedEverywhere := ExpectationFrom([]StepObservation{notFound(), notFound()})
	ok, why := CompareShadow(failedEverywhere, silent, clean)
	if ok {
		t.Fatal("a clean replay matched a step every recording saw fail, because the app did not report its flag")
	}
	if joined := strings.Join(why, "; "); !strings.Contains(joined, "every recording") || strings.Contains(joined, "the app") {
		t.Fatalf("the reason must name the recordings that stood in for the silent app; got %v", why)
	}
	if ok, why := CompareShadow(failedEverywhere, silent, notFound()); !ok {
		t.Fatalf("a failed replay of a step every recording saw fail was refused: %v", why)
	}
	if ok, _ := CompareShadow(ExpectationFrom([]StepObservation{clean, clean}), silent, notFound()); ok {
		t.Fatal("a failed replay matched a step no recording saw fail, because the app did not report its flag")
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
