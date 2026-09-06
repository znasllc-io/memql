package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// runscript_test.go -- the ship / verify / run rules, with both surfaces
// faked. No engine, no database, no network: everything runScript decides is
// decided from the arguments and the surface's answers, which is what makes
// the rules below falsifiable rather than aspirational.

type recordedCall struct {
	surface string
	action  string
	args    map[string]any
}

// fakeSurface is a machine or a workbench that keeps a filesystem in a map.
type fakeSurface struct {
	name     string
	platform string
	files    map[string][]byte
	calls    *[]recordedCall
	// refuse pins one action to a named refusal, for the paths where the
	// dispatcher says no.
	refuse map[string]CallResult
	// corrupt rewrites what a read-back returns, which is the only way to
	// exercise a hash mismatch without a real disk.
	corrupt func([]byte) []byte
	// truncate makes the read-back report itself clipped.
	truncate bool
	execErr  error
}

func newSurface(name, platform string, calls *[]recordedCall) *fakeSurface {
	return &fakeSurface{name: name, platform: platform, files: map[string][]byte{}, calls: calls, refuse: map[string]CallResult{}}
}

func (f *fakeSurface) Name() string                             { return f.name }
func (f *fakeSurface) Platform(context.Context, Request) string { return f.platform }

func (f *fakeSurface) Call(_ context.Context, _ Request, action string, args map[string]any) (CallResult, error) {
	*f.calls = append(*f.calls, recordedCall{surface: f.name, action: action, args: args})
	if r, ok := f.refuse[action]; ok {
		return r, nil
	}
	switch action {
	case "fs_write":
		p, _ := args["path"].(string)
		content, _ := args["content"].(string)
		f.files[p] = []byte(content)
		return CallResult{OK: true, Payload: map[string]any{"path": p, "bytes": len(content)}}, nil
	case "fs_read":
		p, _ := args["path"].(string)
		body, ok := f.files[p]
		if !ok {
			return CallResult{OK: false, ErrorCode: "fs_read_failed", ErrorMsg: "no such file"}, nil
		}
		if f.corrupt != nil {
			body = f.corrupt(body)
		}
		return CallResult{OK: true, Payload: map[string]any{
			"path": p, "content": string(body), "bytes": len(body), "truncated": f.truncate,
		}}, nil
	case "exec":
		if f.execErr != nil {
			return CallResult{}, f.execErr
		}
		cmd, _ := args["cmd"].(string)
		return CallResult{OK: true, Payload: map[string]any{
			"exitCode": float64(0), "stdout": "ran " + cmd, "stderr": "", "durationMs": float64(7),
		}}, nil
	}
	return CallResult{OK: false, ErrorCode: "unknown_action"}, nil
}

type fakeSkills struct {
	rows map[string]SkillScripts
	err  error
	// written records SetScripts calls.
	written map[string][]Script
}

func (f *fakeSkills) SkillScripts(_ context.Context, id string) (SkillScripts, bool, error) {
	if f.err != nil {
		return SkillScripts{}, false, f.err
	}
	row, ok := f.rows[id]
	return row, ok, nil
}

func (f *fakeSkills) SetScripts(_ context.Context, id string, scripts []Script) error {
	if f.written == nil {
		f.written = map[string][]Script{}
	}
	f.written[id] = scripts
	row := f.rows[id]
	row.Scripts = scripts
	f.rows[id] = row
	return nil
}

type fakeArtifacts struct {
	blobs map[string][]byte
	names map[string]string
	err   error
	// wrote records WriteArtifact calls in order.
	wrote []string
	next  int
}

func (f *fakeArtifacts) ReadArtifact(_ context.Context, id string) (ScriptBytes, error) {
	if f.err != nil {
		return ScriptBytes{}, f.err
	}
	body, ok := f.blobs[id]
	if !ok {
		return ScriptBytes{}, errors.New("no such artifact")
	}
	return ScriptBytes{Data: body, Sha256: hashOf(body), Name: f.names[id]}, nil
}

func (f *fakeArtifacts) WriteArtifact(_ context.Context, name, _ string, data []byte) (string, error) {
	f.next++
	id := "artifact-new-" + string(rune('a'+f.next-1))
	if f.blobs == nil {
		f.blobs = map[string][]byte{}
	}
	if f.names == nil {
		f.names = map[string]string{}
	}
	f.blobs[id] = append([]byte(nil), data...)
	f.names[id] = name
	f.wrote = append(f.wrote, name)
	return id, nil
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const scriptBody = "#!/usr/bin/env bash\necho reconciled\n"

func fixture(t *testing.T) (*Runner, *fakeSurface, *fakeSurface, *fakeSkills, *fakeArtifacts, *[]recordedCall) {
	t.Helper()
	calls := &[]recordedCall{}
	wb := newSurface(SurfaceWorkbench, "linux", calls)
	fleet := newSurface(SurfaceMachine, "darwin", calls)
	arts := &fakeArtifacts{
		blobs: map[string][]byte{"artifact-1": []byte(scriptBody)},
		names: map[string]string{"artifact-1": "reconcile.sh"},
	}
	sk := &fakeSkills{rows: map[string]SkillScripts{
		"skill-1": {SkillID: "skill-1", Slug: "reconcile", Active: true, Scripts: []Script{
			{Platform: "any", ArtifactID: "artifact-1", Entry: "bash {script}"},
		}},
	}}
	return NewRunner(sk, arts, wb, fleet), wb, fleet, sk, arts, calls
}

func req() Request {
	return Request{SkillID: "skill-1", PlanID: "plan-1", OwnerID: "user-1"}
}

func TestAScriptIsShippedVerifiedAndRunOnTheWorkbenchByDefault(t *testing.T) {
	runner, wb, fleet, _, _, calls := fixture(t)

	receipt, err := runner.Run(context.Background(), req())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if receipt.Surface != SurfaceWorkbench {
		t.Fatalf("surface = %q, want the workbench by default", receipt.Surface)
	}
	if len(*fleet.calls) == 0 {
		t.Fatal("no calls recorded at all")
	}
	for _, c := range *calls {
		if c.surface == SurfaceMachine {
			t.Fatalf("a plain call reached the fleet: %+v", c)
		}
	}
	if !receipt.Verified || !receipt.Shipped {
		t.Fatalf("receipt = %+v, want shipped and verified", receipt)
	}
	if receipt.ContentHash != hashOf([]byte(scriptBody)) {
		t.Fatalf("hash = %q", receipt.ContentHash)
	}
	if got := string(wb.files[receipt.Path]); got != scriptBody {
		t.Fatalf("shipped bytes = %q", got)
	}
	if receipt.ExitCode != 0 || !strings.Contains(receipt.Stdout, "bash '") {
		t.Fatalf("receipt = %+v", receipt)
	}
}

// The order is the guarantee. exec must be the LAST call, after a write and a
// read-back: a design that ran first and verified afterwards would report a
// mismatch about a script that had already had its effect.
func TestVerificationHappensBeforeExecution(t *testing.T) {
	runner, _, _, _, _, calls := fixture(t)
	if _, err := runner.Run(context.Background(), req()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	seen := []string{}
	for _, c := range *calls {
		seen = append(seen, c.action)
	}
	last := seen[len(seen)-1]
	if last != "exec" {
		t.Fatalf("call order = %v, want exec last", seen)
	}
	writeAt, readBackAt := -1, -1
	for i, a := range seen {
		if a == "fs_write" {
			writeAt = i
		}
		if a == "fs_read" && writeAt >= 0 && readBackAt == -1 && i > writeAt {
			readBackAt = i
		}
	}
	if writeAt == -1 || readBackAt == -1 || readBackAt > len(seen)-2 {
		t.Fatalf("call order = %v, want write then read-back then exec", seen)
	}
}

func TestBytesThatCameBackDifferentRefuseAndRunNothing(t *testing.T) {
	runner, wb, _, _, _, calls := fixture(t)
	wb.corrupt = func(b []byte) []byte { return append(append([]byte(nil), b...), '!') }

	_, err := runner.Run(context.Background(), req())
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Code != ErrScriptHashMismatch {
		t.Fatalf("err = %v, want %s", err, ErrScriptHashMismatch)
	}
	for _, c := range *calls {
		if c.action == "exec" {
			t.Fatal("a script whose bytes did not verify was executed")
		}
	}
}

func TestATruncatedReadBackIsRefusedRatherThanReportedAsAMismatch(t *testing.T) {
	runner, wb, _, _, _, _ := fixture(t)
	wb.truncate = true

	_, err := runner.Run(context.Background(), req())
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Code != ErrScriptTooLargeToVerify {
		t.Fatalf("err = %v, want %s -- a clipped read is not a corruption", err, ErrScriptTooLargeToVerify)
	}
}

func TestAScriptTooLargeToVerifyIsRefusedBeforeAnythingIsSent(t *testing.T) {
	runner, _, _, _, arts, calls := fixture(t)
	arts.blobs["artifact-1"] = make([]byte, MaxVerifiableBytes+1)

	_, err := runner.Run(context.Background(), req())
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Code != ErrScriptTooLargeToVerify {
		t.Fatalf("err = %v, want %s", err, ErrScriptTooLargeToVerify)
	}
	if len(*calls) != 0 {
		t.Fatalf("calls = %+v, want nothing sent", *calls)
	}
}

// Content addressing's payoff: the second run of a script does not re-send
// it, because the path IS the hash and a file already there with the right
// bytes is by construction the right file.
func TestAScriptAlreadyOnTheFarSideIsNotShippedAgain(t *testing.T) {
	runner, _, _, _, _, calls := fixture(t)
	if _, err := runner.Run(context.Background(), req()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	*calls = (*calls)[:0]

	receipt, err := runner.Run(context.Background(), req())
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if receipt.Shipped {
		t.Fatal("the script was re-sent although the far side already held it")
	}
	if !receipt.Verified {
		t.Fatal("a run that skipped the ship must still report itself verified")
	}
	for _, c := range *calls {
		if c.action == "fs_write" {
			t.Fatal("fs_write ran on a second identical call")
		}
	}
}

func TestALabelRequirementRunsOnTheFleet(t *testing.T) {
	runner, _, _, _, _, calls := fixture(t)
	r := req()
	r.RequireLabels = map[string]string{"os": "darwin"}

	receipt, err := runner.Run(context.Background(), r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if receipt.Surface != SurfaceMachine {
		t.Fatalf("surface = %q, want the fleet", receipt.Surface)
	}
	for _, c := range *calls {
		if c.surface == SurfaceWorkbench {
			t.Fatalf("a labelled call reached the workbench: %+v", c)
		}
	}
}

func TestAnEnvironmentNeedTheWorkbenchLacksRunsOnTheFleet(t *testing.T) {
	runner, _, _, _, _, _ := fixture(t)
	r := req()
	r.Environment = map[string]any{"needs": []any{"user_files"}}

	receipt, err := runner.Run(context.Background(), r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if receipt.Surface != SurfaceMachine {
		t.Fatalf("surface = %q, want the fleet", receipt.Surface)
	}
}

// A typo must never be read as "send it to a machine". The workbench's own
// evaluator owns the closed set and refuses an unknown need by name; this
// layer passing it through unchanged is what lets it.
func TestAnUnrecognisedNeedDoesNotRouteToTheFleet(t *testing.T) {
	runner, _, _, _, _, _ := fixture(t)
	r := req()
	r.Environment = map[string]any{"needs": []any{"user_fies"}}

	receipt, err := runner.Run(context.Background(), r)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if receipt.Surface != SurfaceWorkbench {
		t.Fatalf("surface = %q -- a misspelled need must not put somebody's script on their laptop", receipt.Surface)
	}
}

// Falling back to the workbench here would run the step in the one place the
// caller just said it cannot run.
func TestAMachineRequirementWithNoFleetRefusesRatherThanFallingBack(t *testing.T) {
	sk := &fakeSkills{rows: map[string]SkillScripts{"skill-1": {SkillID: "skill-1", Active: true}}}
	calls := &[]recordedCall{}
	runner := NewRunner(sk, &fakeArtifacts{}, newSurface(SurfaceWorkbench, "linux", calls), nil)

	r := req()
	r.RequireLabels = map[string]string{"gpu": "true"}
	_, err := runner.Run(context.Background(), r)
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Code != ErrNoSurface {
		t.Fatalf("err = %v, want %s", err, ErrNoSurface)
	}
	if len(*calls) != 0 {
		t.Fatal("a refused call still reached a surface")
	}
}

// The refusal a dispatcher gives back has to survive this layer intact: the
// reroute and the consent card both key on the CODE, and translating it here
// makes a reroutable refusal unrecognisable.
func TestADispatcherRefusalIsReturnedVerbatim(t *testing.T) {
	runner, wb, _, _, _, _ := fixture(t)
	wb.refuse["exec"] = CallResult{OK: false, ErrorCode: "denied_by_scope", ErrorMsg: "observe does not cover exec"}

	_, err := runner.Run(context.Background(), req())
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Code != "denied_by_scope" {
		t.Fatalf("err = %v, want the dispatcher's own code", err)
	}
}

func TestAnUnreadableSkillAndAnAbsentOneGiveOneAnswer(t *testing.T) {
	runner, _, _, sk, _, _ := fixture(t)
	delete(sk.rows, "skill-1")

	_, err := runner.Run(context.Background(), req())
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Code != ErrSkillNotFound {
		t.Fatalf("err = %v, want %s", err, ErrSkillNotFound)
	}
	if !strings.Contains(refusal.Message, "not yours") {
		t.Fatalf("message = %q -- it must say both readings, because the engine cannot tell them apart", refusal.Message)
	}
}

func TestASkillWithNoScriptForThePlatformIsNamed(t *testing.T) {
	runner, wb, _, sk, _, _ := fixture(t)
	wb.platform = "windows"
	row := sk.rows["skill-1"]
	row.Scripts = []Script{{Platform: "linux", ArtifactID: "artifact-1", Entry: "bash {script}"}}
	sk.rows["skill-1"] = row

	_, err := runner.Run(context.Background(), req())
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Code != ErrNoScriptForPlatform {
		t.Fatalf("err = %v, want %s", err, ErrNoScriptForPlatform)
	}
	if !strings.Contains(refusal.Message, "windows") || !strings.Contains(refusal.Message, "reconcile") {
		t.Fatalf("message = %q, want it to name the skill and the platform", refusal.Message)
	}
}

func TestAnExactPlatformBeatsAny(t *testing.T) {
	scripts := []Script{
		{Platform: "any", ArtifactID: "generic"},
		{Platform: "linux", ArtifactID: "specific"},
	}
	got, ok := PickScript(scripts, "linux")
	if !ok || got.ArtifactID != "specific" {
		t.Fatalf("PickScript = %+v", got)
	}
}

// "We do not know what we are running on" must not select the linux script.
func TestAnUnknownPlatformMatchesOnlyAny(t *testing.T) {
	if _, ok := PickScript([]Script{{Platform: "linux", ArtifactID: "specific"}}, ""); ok {
		t.Fatal("an unknown platform selected a platform-specific script")
	}
	got, ok := PickScript([]Script{{Platform: "any", ArtifactID: "generic"}}, "")
	if !ok || got.ArtifactID != "generic" {
		t.Fatalf("PickScript = %+v", got)
	}
}

// Naming an artifact and getting a different one is worse than failing.
func TestAPinnedArtifactIsNeverSubstituted(t *testing.T) {
	runner, _, _, sk, _, _ := fixture(t)
	row := sk.rows["skill-1"]
	row.Scripts = append(row.Scripts, Script{Platform: "linux", ArtifactID: "artifact-2"})
	sk.rows["skill-1"] = row

	r := req()
	r.ScriptArtifactID = "artifact-absent"
	_, err := runner.Run(context.Background(), r)
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Code != ErrNoScriptForPlatform {
		t.Fatalf("err = %v, want a refusal rather than a substitution", err)
	}
}

func TestArgumentsAreQuotedIndividually(t *testing.T) {
	got := buildCommand("python3 {script}", ".memql-scripts/abc.py", []string{"a b", "it's"})
	want := `python3 '.memql-scripts/abc.py' 'a b' 'it'\''s'`
	if got != want {
		t.Fatalf("command = %q, want %q", got, want)
	}
}

func TestAnEntryThatNeverNamesTheScriptGetsItAppended(t *testing.T) {
	if got := buildCommand("bash", "s.sh", nil); got != "bash 's.sh'" {
		t.Fatalf("command = %q", got)
	}
	if got := buildCommand("", "s.sh", nil); got != "'s.sh'" {
		t.Fatalf("command = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Capture
// ---------------------------------------------------------------------------

func captureReq() CaptureRequest {
	return CaptureRequest{Request: req(), Path: "bin/reconcile.sh", Platform: "linux", Entry: "bash {script}"}
}

func TestCaptureFilesADiscoveredScriptUnderTheSkill(t *testing.T) {
	runner, wb, _, sk, arts, _ := fixture(t)
	runner = runner.WithLibrary(arts, sk)
	wb.files["bin/reconcile.sh"] = []byte("echo discovered\n")

	got, err := runner.Capture(context.Background(), captureReq())
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !got.Changed || got.ArtifactID == "" {
		t.Fatalf("captured = %+v", got)
	}
	if got.ContentHash != hashOf([]byte("echo discovered\n")) {
		t.Fatalf("hash = %q", got.ContentHash)
	}
	written := sk.written["skill-1"]
	found := false
	for _, s := range written {
		if s.ArtifactID == got.ArtifactID && s.Platform == "linux" && s.Entry == "bash {script}" {
			found = true
		}
	}
	if !found {
		t.Fatalf("skill scripts = %+v, want the captured entry", written)
	}
	// The machine path is gone from the record: that is the whole point.
	for _, s := range written {
		if strings.Contains(s.Entry, "bin/reconcile.sh") {
			t.Fatalf("a captured entry still names a path on a machine: %+v", s)
		}
	}
}

func TestCapturingTheSameBytesTwiceChangesNothing(t *testing.T) {
	runner, wb, _, sk, arts, _ := fixture(t)
	runner = runner.WithLibrary(arts, sk)
	wb.files["bin/reconcile.sh"] = []byte("echo discovered\n")

	first, err := runner.Capture(context.Background(), captureReq())
	if err != nil {
		t.Fatalf("first Capture: %v", err)
	}
	second, err := runner.Capture(context.Background(), captureReq())
	if err != nil {
		t.Fatalf("second Capture: %v", err)
	}
	if second.Changed {
		t.Fatal("capturing identical bytes filed a second artifact")
	}
	if second.ArtifactID != first.ArtifactID {
		t.Fatalf("artifact moved: %q then %q", first.ArtifactID, second.ArtifactID)
	}
	if len(arts.wrote) != 1 {
		t.Fatalf("Library writes = %v, want exactly one", arts.wrote)
	}
}

func TestARecapturedScriptReplacesTheEntryAndKeepsTheOldArtifact(t *testing.T) {
	runner, wb, _, sk, arts, _ := fixture(t)
	runner = runner.WithLibrary(arts, sk)
	wb.files["bin/reconcile.sh"] = []byte("echo v1\n")
	first, err := runner.Capture(context.Background(), captureReq())
	if err != nil {
		t.Fatalf("first Capture: %v", err)
	}

	wb.files["bin/reconcile.sh"] = []byte("echo v2\n")
	second, err := runner.Capture(context.Background(), captureReq())
	if err != nil {
		t.Fatalf("second Capture: %v", err)
	}
	if !second.Changed || second.Replaced != first.ArtifactID {
		t.Fatalf("captured = %+v, want it to replace %q", second, first.ArtifactID)
	}
	if _, stillThere := arts.blobs[first.ArtifactID]; !stillThere {
		t.Fatal("capture deleted the artifact a pinned template may still name")
	}
	linux := 0
	for _, s := range sk.written["skill-1"] {
		if s.Platform == "linux" {
			linux++
		}
	}
	if linux != 1 {
		t.Fatalf("linux entries = %d, want exactly one", linux)
	}
}

// A capture that quietly does nothing leaves a template pointing at a machine
// path forever, so an unwired node says so.
func TestCaptureWithNoLibraryWriterRefusesByName(t *testing.T) {
	runner, wb, _, _, _, _ := fixture(t)
	wb.files["bin/reconcile.sh"] = []byte("x")

	_, err := runner.Capture(context.Background(), captureReq())
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Code != ErrCaptureNotWired {
		t.Fatalf("err = %v, want %s", err, ErrCaptureNotWired)
	}
}

func TestCaptureOfAnAbsentPathIsNamed(t *testing.T) {
	runner, _, _, sk, arts, _ := fixture(t)
	runner = runner.WithLibrary(arts, sk)

	_, err := runner.Capture(context.Background(), captureReq())
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Code != ErrCaptureUnreadable {
		t.Fatalf("err = %v, want %s", err, ErrCaptureUnreadable)
	}
}

// "We did not ask" is not evidence about which platforms a script runs on.
func TestCaptureWithNoPlatformRecordsAny(t *testing.T) {
	runner, wb, _, sk, arts, _ := fixture(t)
	runner = runner.WithLibrary(arts, sk)
	wb.files["bin/x.sh"] = []byte("y")

	r := captureReq()
	r.Path = "bin/x.sh"
	r.Platform = ""
	got, err := runner.Capture(context.Background(), r)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got.Platform != "any" {
		t.Fatalf("platform = %q, want any", got.Platform)
	}
}

// ---------------------------------------------------------------------------
// The binding
// ---------------------------------------------------------------------------

type recordedBinding struct {
	owner   string
	stepID  string
	binding map[string]any
}

type fakeBindings struct{ calls []recordedBinding }

func (f *fakeBindings) StampBinding(_ context.Context, owner, stepID string, binding map[string]any) error {
	f.calls = append(f.calls, recordedBinding{owner: owner, stepID: stepID, binding: binding})
	return nil
}

func TestADispatchRecordsItsBindingOnTheStep(t *testing.T) {
	runner, _, _, _, _, _ := fixture(t)
	bindings := &fakeBindings{}
	runner = runner.WithBindings(bindings)

	r := req()
	r.StepID = "step-1"
	r.RunID = "run-1"
	if _, err := runner.Run(context.Background(), r); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(bindings.calls) != 1 {
		t.Fatalf("bindings = %+v, want exactly one", bindings.calls)
	}
	got := bindings.calls[0]
	if got.stepID != "step-1" || got.owner != "user-1" {
		t.Fatalf("stamped (%q, %q)", got.owner, got.stepID)
	}
	if got.binding["surface"] != SurfaceWorkbench {
		t.Fatalf("surface = %v", got.binding["surface"])
	}
	if got.binding["contentHash"] != hashOf([]byte(scriptBody)) {
		t.Fatalf("contentHash = %v -- the bytes the step will run are what make a receipt answerable", got.binding["contentHash"])
	}
	skillIDs, _ := got.binding["skillIds"].([]string)
	if len(skillIDs) != 1 || skillIDs[0] != "skill-1" {
		t.Fatalf("skillIds = %v", got.binding["skillIds"])
	}
}

// The provider half of a binding belongs to a REASONING step's dispatch. A
// script step calls no model, and writing those keys here would be recording
// a decision nobody made.
func TestAScriptStepsBindingClaimsNoProvider(t *testing.T) {
	runner, _, _, _, _, _ := fixture(t)
	bindings := &fakeBindings{}
	runner = runner.WithBindings(bindings)

	r := req()
	r.StepID = "step-1"
	if _, err := runner.Run(context.Background(), r); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, key := range []string{"provider", "model", "providerPolicy"} {
		if _, present := bindings.calls[0].binding[key]; present {
			t.Fatalf("the binding claims %q, which a script dispatch does not decide", key)
		}
	}
}

// An ad-hoc runScript is not a step of anything, so there is no row to stamp.
func TestACallThatNamesNoStepRecordsNothing(t *testing.T) {
	runner, _, _, _, _, _ := fixture(t)
	bindings := &fakeBindings{}
	runner = runner.WithBindings(bindings)

	if _, err := runner.Run(context.Background(), req()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(bindings.calls) != 0 {
		t.Fatalf("bindings = %+v, want none", bindings.calls)
	}
}

// BEFORE THE BODY, which is the whole point (spec section D: the `running`
// version carries the binding, written before anything executes). A binding
// stamped afterwards would be missing from exactly the case a person reads it
// for -- a step that started and never came back.
func TestTheBindingIsRecordedBeforeTheBodyRuns(t *testing.T) {
	runner, wb, _, _, _, calls := fixture(t)
	bindings := &fakeBindings{}
	runner = runner.WithBindings(bindings)
	// exec fails, so the only way a binding exists is if it was written first.
	wb.refuse["exec"] = CallResult{OK: false, ErrorCode: "denied_by_scope", ErrorMsg: "no"}

	r := req()
	r.StepID = "step-1"
	if _, err := runner.Run(context.Background(), r); err == nil {
		t.Fatal("the refused exec did not surface")
	}
	if len(bindings.calls) != 1 {
		t.Fatalf("bindings = %+v -- a step that started and was refused still has a binding", bindings.calls)
	}
	ranExec := false
	for _, c := range *calls {
		if c.action == "exec" {
			ranExec = true
		}
	}
	if !ranExec {
		t.Fatal("the test did not reach exec, so it proves nothing about the order")
	}
}

// A machine requirement is part of what the dispatch decided.
func TestALabelRequirementIsRecordedOnTheBinding(t *testing.T) {
	runner, _, _, _, _, _ := fixture(t)
	bindings := &fakeBindings{}
	runner = runner.WithBindings(bindings)

	r := req()
	r.StepID = "step-1"
	r.RequireLabels = map[string]string{"os": "darwin"}
	if _, err := runner.Run(context.Background(), r); err != nil {
		t.Fatalf("Run: %v", err)
	}
	labels, _ := bindings.calls[0].binding["machineLabels"].(map[string]string)
	if labels["os"] != "darwin" {
		t.Fatalf("machineLabels = %v", bindings.calls[0].binding["machineLabels"])
	}
	if bindings.calls[0].binding["surface"] != SurfaceMachine {
		t.Fatalf("surface = %v, want the fleet", bindings.calls[0].binding["surface"])
	}
}
