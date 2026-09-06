package capability

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot resolves the checkout from this file's own path, the way every
// other repo-reading gate here does.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// TestTheGoEnvelopeMatchesTheShellLibrarysFormatStrings is the drift gate this
// package exists for.
//
// `cmd/memql-bench` claims to be under the capability-script contract, and
// `scripts/lib/capability_contract_test.go` cannot check that claim: its walk
// is rooted at `scripts/` and filters `*.sh`, so no Go binary is ever visited.
// Hand-transcribing the shell library's printf format into Go would therefore
// be a copy nothing keeps in step -- the envelope would diverge on the day
// somebody adds a field to the shell one, and the only symptom would be a
// caller parsing one shape and getting the other.
//
// So this reads capability.sh AT TEST TIME and compares.
func TestTheGoEnvelopeMatchesTheShellLibrarysFormatStrings(t *testing.T) {
	lib := filepath.Join(repoRoot(t), "scripts", "lib", "capability.sh")
	raw, err := os.ReadFile(lib)
	if err != nil {
		t.Fatalf("reading the shell library this package is checked against: %v", err)
	}
	shell := string(raw)

	// The shell writes the format as a single-quoted printf argument. Compare
	// the exact string, trailing newline included -- the newline is what makes
	// "exactly one envelope, on one line" checkable by a caller reading a line.
	wantEnvelope := strings.TrimSuffix(envelopeFormat, "\n")
	if !strings.Contains(shell, "'"+wantEnvelope+`\n'`) {
		t.Errorf("the Go envelope format has drifted from scripts/lib/capability.sh.\n"+
			"  Go:    %s\n"+
			"  The shell library is the contract; update envelopeFormat to match the printf in _cap_emit, or update both deliberately.",
			envelopeFormat)
	}

	wantSpec := strings.TrimSuffix(specFormat, "\n")
	if !strings.Contains(shell, "'"+wantSpec+`\n'`) {
		t.Errorf("the Go --print-spec format has drifted from scripts/lib/capability.sh.\n"+
			"  Go:    %s\n"+
			"  Update specFormat to match the printf in _cap_print_spec.",
			specFormat)
	}
}

// TestTheExitCodeTableMatchesTheContract pins the five reserved codes against
// the contract document, so renumbering one has to be a deliberate act in two
// files rather than a plausible-looking edit in this one.
func TestTheExitCodeTableMatchesTheContract(t *testing.T) {
	doc := filepath.Join(repoRoot(t), "docs", "internal", "design", "capability-script-contract.md")
	raw, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("reading the contract: %v", err)
	}
	text := string(raw)
	for _, tc := range []struct {
		code int
		want string
	}{
		{ExitBadParam, "2"},
		{ExitRefused, "3"},
		{ExitPrerequisite, "4"},
		{ExitOpFailed, "5"},
	} {
		// The contract's table pads its cells: `| 2    | bad invocation: ...`.
		if !strings.Contains(text, "\n| "+tc.want+" ") {
			t.Errorf("the contract no longer documents exit code %d; the Go constants and the document disagree", tc.code)
		}
	}
	if ExitOK != 0 || ExitGeneric != 1 {
		t.Fatalf("ExitOK=%d ExitGeneric=%d; 0 and 1 are fixed by every shell caller", ExitOK, ExitGeneric)
	}
}

func spec() Spec {
	return Spec{
		Id:      "bench.run",
		Summary: "Run the proving corpus and emit its figures",
		Params: []Param{
			{Name: "tier", Description: "ci or live", Required: true},
			{Name: "dry-run", Description: "plan without running"},
		},
	}
}

func run(t *testing.T, argv []string) (stdout, stderr string, c *Capability, handled bool, err error) {
	t.Helper()
	var o, e bytes.Buffer
	c, handled, err = Parse(spec(), argv, &o, &e)
	return o.String(), e.String(), c, handled, err
}

func TestExactlyOneEnvelopeOnStdoutAndEveryHumanLineOnStderr(t *testing.T) {
	var o, e bytes.Buffer
	c, handled, err := Parse(spec(), []string{"--tier=ci"}, &o, &e)
	if err != nil || handled {
		t.Fatalf("Parse: err=%v handled=%v", err, handled)
	}
	c.Info("reading the corpus")
	c.Step("running durability.kill-and-resume")
	c.Warn("the live tier is disarmed")
	c.Set("scenarios", 14)
	c.Changed()
	if code := c.OK(); code != ExitOK {
		t.Fatalf("OK() = %d", code)
	}

	lines := strings.Split(strings.TrimRight(o.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout carried %d lines, want exactly one envelope:\n%s", len(lines), o.String())
	}
	env, err := ParseEnvelope([]byte(lines[0]))
	if err != nil {
		t.Fatalf("the envelope does not parse: %v\n%s", err, lines[0])
	}
	if !env.OK || env.Capability != "bench.run" || !env.Changed || env.Error != nil {
		t.Errorf("envelope = %+v", env)
	}
	if got, _ := env.Result["scenarios"].(float64); got != 14 {
		t.Errorf("result.scenarios = %v, want 14", env.Result["scenarios"])
	}
	for _, want := range []string{"reading the corpus", "running durability", "live tier is disarmed"} {
		if !strings.Contains(e.String(), want) {
			t.Errorf("stderr is missing %q; human lines must not be on stdout", want)
		}
	}
}

func TestAFailureEnvelopeCarriesTheExitCode(t *testing.T) {
	var o, e bytes.Buffer
	c, _, err := Parse(spec(), []string{"--tier=live"}, &o, &e)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if code := c.Fail(ExitRefused, "the run would cost $%.2f, above the ceiling of $%.2f", 4.10, 1.00); code != ExitRefused {
		t.Fatalf("Fail returned %d, want %d", code, ExitRefused)
	}
	env, err := ParseEnvelope(o.Bytes())
	if err != nil {
		t.Fatalf("ParseEnvelope: %v", err)
	}
	if env.OK {
		t.Error("ok is true on a failure envelope")
	}
	if env.Error == nil || env.Error.Code != ExitRefused {
		t.Fatalf("error block = %+v, want code %d", env.Error, ExitRefused)
	}
	if !strings.Contains(env.Error.Message, "above the ceiling") {
		t.Errorf("message = %q", env.Error.Message)
	}
}

func TestAnUndeclaredFlagIsRefusedWithExitTwo(t *testing.T) {
	// The declared set IS the accepted surface. This is what stops
	// `--teir=live` silently doing nothing.
	_, _, _, _, err := run(t, []string{"--teir=live"})
	if err == nil {
		t.Fatal("Parse accepted an undeclared flag")
	}
	pe, ok := err.(*ParseError)
	if !ok || pe.Code != ExitBadParam {
		t.Fatalf("err = %#v, want a ParseError with code %d", err, ExitBadParam)
	}
	if !strings.Contains(pe.Msg, "--teir") || !strings.Contains(pe.Msg, "--tier") {
		t.Errorf("message = %q; it must name both the typo and the declared set", pe.Msg)
	}
}

func TestAPositionalArgumentIsRefused(t *testing.T) {
	_, _, _, _, err := run(t, []string{"run", "--tier=ci"})
	pe, ok := err.(*ParseError)
	if !ok || pe.Code != ExitBadParam {
		t.Fatalf("err = %#v, want ExitBadParam", err)
	}
}

func TestAMissingRequiredParameterIsRefusedWithExitTwo(t *testing.T) {
	_, _, _, _, err := run(t, []string{"--dry-run"})
	pe, ok := err.(*ParseError)
	if !ok || pe.Code != ExitBadParam {
		t.Fatalf("err = %#v, want ExitBadParam", err)
	}
	if !strings.Contains(pe.Msg, "tier") {
		t.Errorf("message = %q, want it to name the missing parameter", pe.Msg)
	}
}

func TestABareFlagIsTheStringTrueAndNotOne(t *testing.T) {
	// memql#4629 settled this on the shell side. A Go emitter answering "1"
	// would make cap_bool-shaped consumers disagree across the two.
	_, _, c, _, err := run(t, []string{"--tier=ci", "--dry-run"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := c.Param("dry-run", ""); got != "true" {
		t.Fatalf("a bare flag resolved to %q, want \"true\"", got)
	}
}

func TestBoolAcceptsTheShellLibrarysSetsAndRefusesEverythingElse(t *testing.T) {
	for _, truthy := range []string{"true", "1", "yes", "y", "on", "TRUE", "Yes"} {
		_, _, c, _, err := run(t, []string{"--tier=ci", "--dry-run=" + truthy})
		if err != nil {
			t.Fatalf("Parse(%q): %v", truthy, err)
		}
		got, err := c.Bool("dry-run", false)
		if err != nil || !got {
			t.Errorf("Bool(%q) = %v, %v; want true", truthy, got, err)
		}
	}
	for _, falsey := range []string{"false", "0", "no", "n", "off", "OFF"} {
		_, _, c, _, err := run(t, []string{"--tier=ci", "--dry-run=" + falsey})
		if err != nil {
			t.Fatalf("Parse(%q): %v", falsey, err)
		}
		got, err := c.Bool("dry-run", true)
		if err != nil || got {
			t.Errorf("Bool(%q) = %v, %v; want false", falsey, got, err)
		}
	}
	// The one that matters: a typo must not run for real.
	_, _, c, _, err := run(t, []string{"--tier=ci", "--dry-run=ture"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := c.Bool("dry-run", false); err == nil {
		t.Fatal("Bool accepted \"ture\" as false; a dry-run typo would then run for real")
	}
}

func TestPrintSpecEmitsTheDescriptorAndNothingRuns(t *testing.T) {
	out, _, _, handled, err := run(t, []string{"--print-spec"})
	if err != nil || !handled {
		t.Fatalf("err=%v handled=%v", err, handled)
	}
	var got struct {
		Capability string `json:"capability"`
		Summary    string `json:"summary"`
		Params     []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Required    bool   `json:"required"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("the spec does not parse: %v\n%s", err, out)
	}
	if got.Capability != "bench.run" || len(got.Params) != 2 {
		t.Fatalf("spec = %+v", got)
	}
	if !got.Params[0].Required || got.Params[1].Required {
		t.Errorf("required flags: %+v", got.Params)
	}
}

func TestPrintSpecIsHandledBeforeARequiredParameterIsChecked(t *testing.T) {
	// A descriptor a caller cannot read without already knowing the arguments
	// is a descriptor for nothing.
	_, _, _, handled, err := run(t, []string{"--print-spec"})
	if err != nil {
		t.Fatalf("--print-spec was refused for a missing required parameter: %v", err)
	}
	if !handled {
		t.Fatal("--print-spec was not handled")
	}
}

func TestASecondEnvelopeIsARefusalRatherThanCorruption(t *testing.T) {
	var o, e bytes.Buffer
	c, _, _ := Parse(spec(), []string{"--tier=ci"}, &o, &e)
	c.Fail(ExitOpFailed, "first")
	if code := c.Fail(ExitOpFailed, "second"); code != ExitOpFailed {
		t.Fatalf("the second Fail returned %d", code)
	}
	if n := strings.Count(strings.TrimRight(o.String(), "\n"), "\n"); n != 0 {
		t.Fatalf("stdout carries %d newlines, so more than one envelope was written:\n%s", n+1, o.String())
	}
	if !strings.Contains(e.String(), "already emitted") {
		t.Error("the second failure was silently dropped; it must at least reach stderr")
	}
}

func TestAControlCharacterInAMessageStaysValidJSON(t *testing.T) {
	// The shell library hand-rolls five escapes because bash has no
	// alternative. Go has one, and using it is what keeps a scenario title
	// carrying a tab from making the envelope unparseable.
	var o, e bytes.Buffer
	c, _, _ := Parse(spec(), []string{"--tier=ci"}, &o, &e)
	c.Set("title", "a\ttitle\nwith \"quotes\" and \\backslashes\\")
	c.Fail(ExitOpFailed, "failed on %q", "a\tb")
	if _, err := ParseEnvelope(o.Bytes()); err != nil {
		t.Fatalf("the envelope does not parse: %v\n%s", err, o.String())
	}
}
