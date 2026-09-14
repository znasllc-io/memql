package memql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/envregistry"
	"github.com/znasllc-io/memql/component/memql/readiness"
)

// recordingExecutor captures every rendered mutation instead of executing it,
// and can stand in for a write that fails.
type recordingExecutor struct {
	calls []string
	fail  error
}

func (x *recordingExecutor) exec(_ context.Context, call string) error {
	if x.fail != nil {
		return x.fail
	}
	x.calls = append(x.calls, call)
	return nil
}

func (x *recordingExecutor) callsFor(module string) []string {
	var out []string
	for _, c := range x.calls {
		if strings.Contains(c, `module: "`+module+`"`) {
			out = append(out, c)
		}
	}
	return out
}

// flippingFleet answers the fleet read from a switch the test flips.
func flippingFleet(ok *bool) readinessResolvers {
	r := fakeResolvers(nil, nil, nil)
	r.Registrations = func(context.Context) ([]readiness.RegistrationFacts, error) {
		if !*ok {
			return nil, errors.New("read tcp 10.244.0.38:38472->10.0.185.57:5432: i/o timeout")
		}
		return []readiness.RegistrationFacts{{
			Labels:          map[string]string{"model:llama3.1:8b": "ctx=8192,structured=true"},
			ConnectedNodeId: "agent-1",
			LastSeenAt:      inferenceNow,
		}}, nil
	}
	return r
}

// standingFrom turns what a recording executor wrote into the standing rows
// the next pass would read back -- the round trip the database makes, without
// one.
func standingFrom(t *testing.T, x *recordingExecutor, nodeId string, at time.Time) readinessStanding {
	t.Helper()
	out := readinessStanding{rows: map[string]*readiness.NodeReport{}, readable: true}
	for _, c := range x.calls {
		for _, state := range []readiness.State{readiness.Configured, readiness.Partial, readiness.Unconfigured, readiness.NotApplicable, readiness.Unknown} {
			if !strings.Contains(c, `state: "`+string(state)+`"`) {
				continue
			}
			start := strings.Index(c, `module: "`) + len(`module: "`)
			module := c[start : start+strings.Index(c[start:], `"`)]
			r := &readiness.NodeReport{Module: module, NodeId: nodeId, State: state, ReportedAt: at}
			if strings.Contains(c, `reason: "`) {
				rs := strings.Index(c, `reason: "`) + len(`reason: "`)
				r.Reason = c[rs : rs+strings.Index(c[rs:], `"`)]
			}
			out.rows[module] = r
		}
	}
	return out
}

var noStanding = readinessStanding{rows: map[string]*readiness.NodeReport{}, readable: true}

// THE INCIDENT, IN ONE TEST. A node booted into a saturated database, the
// fleet read failed, and `unconfigured` was written over a correct row -- then
// the 30 s re-write failed the same way, and the row stood for a deploy. Now:
// a pass that could not evaluate never writes over a known row.
func TestAnUnknownReportIsNotPersistedOverAKnownRow(t *testing.T) {
	ok := true
	r := flippingFleet(&ok)
	mem := &readinessMemory{}
	x := &recordingExecutor{}
	mods := []envregistry.Module{aiModule()}

	if _, err := writeModuleReadiness(context.Background(), r, mods, "bff-1", "bff", mem, noStanding, x.exec, inferenceNow); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if got := x.callsFor("ai"); len(got) != 1 || !strings.Contains(got[0], `state: "configured"`) {
		t.Fatalf("first pass did not write configured: %v", got)
	}

	// A FRESH PROCESS, so the in-process memory cannot be what saves the row:
	// the standing row it reads back is.
	ok = false
	standing := standingFrom(t, x, "bff-1", inferenceNow)
	written, err := writeModuleReadiness(context.Background(), r, mods, "bff-1", "bff", &readinessMemory{}, standing, x.exec, inferenceNow.Add(time.Minute))
	var unknown *ReadinessUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("a pass with an unknown module returned %v, want *ReadinessUnknownError so the caller retries", err)
	}
	if written != 0 {
		t.Errorf("wrote %d rows over a known verdict; want 0", written)
	}
	if got := x.callsFor("ai"); len(got) != 1 {
		t.Fatalf("the unknown pass rendered a write for ai: %v", got)
	}
	if len(unknown.Modules) != 1 || unknown.Modules[0] != "ai" || unknown.Reasons["ai"] != readiness.ReasonFleetReadFailed {
		t.Errorf("the error does not name the module and reason: %+v", unknown)
	}
	// The error text is what the subscriber logs; it must be actionable and
	// must not carry the resolver's error string (that stayed in the
	// evaluator's own log line).
	if !strings.Contains(err.Error(), "ai=fleetReadFailed") || strings.Contains(err.Error(), "i/o timeout") {
		t.Errorf("error text: %q", err.Error())
	}
}

// THE SAME PROTECTION WHEN THE STANDING READ ITSELF FAILED -- which is exactly
// when unknown verdicts arrive. The read that would have shown the known row
// broke too, so a process with no memory of its own writes NOTHING rather than
// guessing there was no row: a failed standing read counts as "something known
// may stand".
func TestAnUnreadableStandingNeverLetsUnknownThrough(t *testing.T) {
	ok := false
	r := flippingFleet(&ok)
	unreadable := readinessStanding{rows: map[string]*readiness.NodeReport{}}
	x := &recordingExecutor{}

	written, err := writeModuleReadiness(context.Background(), r, []envregistry.Module{aiModule()}, "bff-1", "bff", &readinessMemory{}, unreadable, x.exec, inferenceNow)
	if !IsReadinessUnknown(err) {
		t.Fatalf("got %v, want a ReadinessUnknownError", err)
	}
	if written != 0 || len(x.callsFor("ai")) != 0 {
		t.Fatalf("an unknown verdict was written over a standing row nobody could read: %v", x.calls)
	}

	// And a KNOWN verdict still writes through an unreadable standing: the
	// pre-existing behaviour, because a needless version costs a row while a
	// missed one costs a stale verdict.
	ok = true
	if _, err := writeModuleReadiness(context.Background(), r, []envregistry.Module{aiModule()}, "bff-1", "bff", &readinessMemory{}, unreadable, x.exec, inferenceNow); err != nil {
		t.Fatalf("known pass: %v", err)
	}
	if got := x.callsFor("ai"); len(got) != 1 || !strings.Contains(got[0], `state: "configured"`) {
		t.Fatalf("a known verdict did not write through an unreadable standing: %v", got)
	}
}

// A FRESH NODE SAYS "UNKNOWN", WITH THE REASON, so the Modules page can show
// "could not check: the fleet read failed" instead of nothing -- and so the
// fold can name the node rather than count it.
func TestAFreshNodeWritesUnknownWithAReason(t *testing.T) {
	ok := false
	r := flippingFleet(&ok)
	mem := &readinessMemory{}
	x := &recordingExecutor{}

	written, err := writeModuleReadiness(context.Background(), r, []envregistry.Module{aiModule()}, "bff-1", "bff", mem, noStanding, x.exec, inferenceNow)
	var unknown *ReadinessUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("got %v, want *ReadinessUnknownError", err)
	}
	if written != 1 {
		t.Errorf("wrote %d, want 1 (the unknown row itself)", written)
	}
	got := x.callsFor("ai")
	if len(got) != 1 || !strings.Contains(got[0], `state: "unknown"`) || !strings.Contains(got[0], `reason: "fleetReadFailed"`) {
		t.Fatalf("the fresh node did not write unknown with its reason: %v", got)
	}
	if mem.hasKnown("ai") {
		t.Error("an unknown write was remembered as known; the next unknown pass would then be held back forever")
	}

	// AND THE FIRST SUCCESS AFTER IT IS WRITTEN AND REMEMBERED -- over the
	// unknown row, which is the one row unknown may sit in.
	ok = true
	standing := standingFrom(t, x, "bff-1", inferenceNow)
	if _, err := writeModuleReadiness(context.Background(), r, []envregistry.Module{aiModule()}, "bff-1", "bff", mem, standing, x.exec, inferenceNow.Add(time.Minute)); err != nil {
		t.Fatalf("the recovering pass errored: %v", err)
	}
	if got := x.callsFor("ai"); len(got) != 2 || !strings.Contains(got[1], `state: "configured"`) {
		t.Fatalf("recovery did not write configured: %v", got)
	}
	if !mem.hasKnown("ai") {
		t.Error("a known verdict was not remembered")
	}
}

// Known modules in the same pass are still written; the unknown one is the
// only one held back, and the error names only it.
func TestKnownModulesStillWriteBesideAnUnknownOne(t *testing.T) {
	ok := false
	r := flippingFleet(&ok)
	mem := &readinessMemory{}
	mem.remember("ai", readiness.Configured)
	x := &recordingExecutor{}
	mods := []envregistry.Module{aiModule(), twoSlotModule}

	written, err := writeModuleReadiness(context.Background(), r, mods, "bff-1", "bff", mem, noStanding, x.exec, inferenceNow)
	var unknown *ReadinessUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("got %v", err)
	}
	if written != 1 || len(x.callsFor("storage")) != 1 || len(x.callsFor("ai")) != 0 {
		t.Fatalf("written=%d calls=%v", written, x.calls)
	}
	if len(unknown.Modules) != 1 || unknown.Modules[0] != "ai" {
		t.Errorf("unknown modules %v, want [ai]", unknown.Modules)
	}
}

// A FAILED WRITE is a different failure from an unknown evaluation and keeps
// its shape: stop at the first module, wrap the error, no typed error.
func TestAFailedWriteIsNotAnUnknownError(t *testing.T) {
	ok := true
	r := flippingFleet(&ok)
	x := &recordingExecutor{fail: errors.New("FATAL: remaining connection slots are reserved (SQLSTATE 53300)")}
	_, err := writeModuleReadiness(context.Background(), r, []envregistry.Module{aiModule()}, "bff-1", "bff", &readinessMemory{}, noStanding, x.exec, inferenceNow)
	if err == nil || IsReadinessUnknown(err) {
		t.Fatalf("got %v, want a plain wrapped write error", err)
	}
	if !strings.Contains(err.Error(), "module readiness: write ai:") {
		t.Errorf("error text: %q", err.Error())
	}
}

// AN UNCHANGED VERDICT IS STILL KNOWN. Write-on-change skips restating it, and
// the skip must not leave this process believing it knows nothing -- or a
// failed read on the next pass, with the standing read also failing, would
// write unknown over the very row the skip left standing.
func TestAnUnchangedVerdictIsRememberedAsKnown(t *testing.T) {
	ok := true
	r := flippingFleet(&ok)
	x := &recordingExecutor{}
	first := &readinessMemory{}
	if _, err := writeModuleReadiness(context.Background(), r, []envregistry.Module{aiModule()}, "bff-1", "bff", first, noStanding, x.exec, inferenceNow); err != nil {
		t.Fatal(err)
	}
	standing := standingFrom(t, x, "bff-1", inferenceNow)
	// The standing row carries the lanes the pass will compute, so the second
	// pass finds nothing changed and writes nothing.
	for _, rep := range evaluateModules(context.Background(), r, []envregistry.Module{aiModule()}, "bff-1", "bff", inferenceNow) {
		row := rep
		standing.rows[rep.Module] = &row
	}

	mem := &readinessMemory{}
	written, err := writeModuleReadiness(context.Background(), r, []envregistry.Module{aiModule()}, "bff-1", "bff", mem, standing, x.exec, inferenceNow)
	if err != nil || written != 0 {
		t.Fatalf("an unchanged fresh verdict: written=%d err=%v, want 0 and nil", written, err)
	}
	if !mem.hasKnown("ai") {
		t.Fatal("an unchanged known verdict was not remembered as known")
	}
}

// THE RULE, PURE: every known state may be written, and unknown only where
// nothing known stands.
func TestPersistReadinessRule(t *testing.T) {
	cases := []struct {
		in   readinessPersistInput
		want bool
	}{
		{readinessPersistInput{State: readiness.Configured, KnownBefore: false}, true},
		{readinessPersistInput{State: readiness.Configured, KnownBefore: true}, true},
		{readinessPersistInput{State: readiness.Unconfigured, KnownBefore: true}, true},
		{readinessPersistInput{State: readiness.NotApplicable, KnownBefore: true}, true},
		{readinessPersistInput{State: readiness.Unknown, KnownBefore: false}, true},
		{readinessPersistInput{State: readiness.Unknown, KnownBefore: true}, false},
	}
	for _, c := range cases {
		if got := persistReadiness(c.in); got != c.want {
			t.Errorf("%+v: got %v want %v", c.in, got, c.want)
		}
	}
}

// An unknown standing row is NOT a known verdict to protect: it is the one
// row unknown may be restated over, and a known verdict replaces it.
func TestAnUnknownStandingRowIsNotKnown(t *testing.T) {
	standing := readinessStanding{rows: map[string]*readiness.NodeReport{
		"ai":      {Module: "ai", State: readiness.Unknown, Reason: readiness.ReasonFleetReadFailed},
		"storage": {Module: "storage", State: readiness.Configured},
	}, readable: true}
	if standing.knownRow("ai") {
		t.Error("an unknown standing row counted as known")
	}
	if !standing.knownRow("storage") {
		t.Error("a configured standing row did not count as known")
	}
	if standing.knownRow("email") {
		t.Error("an absent row counted as known on a readable standing")
	}
	if !(readinessStanding{}).knownRow("email") {
		t.Error("an UNREADABLE standing must be treated as possibly known")
	}
}

// THE REASON RIDES ON THE ROW, and only when there is one. A known verdict
// renders no reason argument at all, so its call is byte-for-byte what it was
// before the field existed.
func TestRenderCarriesTheReasonOnlyWhenUnknown(t *testing.T) {
	unknown := readiness.NodeReport{Module: "ai", NodeId: "bff-1", NodeType: "bff", State: readiness.Unknown,
		Reason: readiness.ReasonFleetReadFailed, Core: true, ReportedAt: inferenceNow}
	call, err := renderRecordModuleReadiness(unknown, readinessRowID("ai", "bff-1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`state: "unknown"`, `reason: "fleetReadFailed"`, `lanes: []`} {
		if !strings.Contains(call, want) {
			t.Errorf("the rendered call lacks %s: %s", want, call)
		}
	}

	known := readiness.NodeReport{Module: "ai", NodeId: "bff-1", NodeType: "bff", State: readiness.Configured,
		Core: true, ReportedAt: inferenceNow}
	call, err = renderRecordModuleReadiness(known, readinessRowID("ai", "bff-1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(call, "reason") {
		t.Errorf("a known verdict rendered a reason argument: %s", call)
	}
}

// The lane scope rides on the row, so the fold on every node and in every
// browser can tell a lagging row from a disagreeing one.
func TestRenderCarriesTheLaneScope(t *testing.T) {
	ok := true
	reports := evaluateModules(context.Background(), flippingFleet(&ok), []envregistry.Module{aiModule()}, "bff-1", "bff", inferenceNow)
	call, err := renderRecordModuleReadiness(reports[0], readinessRowID("ai", "bff-1"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(call, `"scope":"cluster"`) != 2 {
		t.Fatalf("the two fleet lanes did not render their scope: %s", call)
	}
}
