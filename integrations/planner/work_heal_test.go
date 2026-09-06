package planner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/healing"
	workintegration "github.com/znasllc-io/memql/integrations/work"
)

type fakeProposer struct {
	patches []healing.Patch
	err     error
	calls   int
}

func (f *fakeProposer) Propose(_ context.Context, _ healing.PreconditionMiss, _ map[string]any) ([]healing.Patch, error) {
	f.calls++
	return f.patches, f.err
}

// recordingRaiser stands in for the work integration. Asserting on the SEED
// rather than on a rendered call string is the point of the delegation: the
// rendering, and the named-args form it has to take, are now integrations/work's
// concern and are pinned there against the REAL parser.
type recordingRaiser struct {
	seeds  []workintegration.ApprovalSeed
	owners []string
	err    error
}

func (r *recordingRaiser) RaiseApproval(_ context.Context, ownerUserId string, a workintegration.ApprovalSeed) (string, error) {
	r.seeds = append(r.seeds, a)
	r.owners = append(r.owners, ownerUserId)
	if r.err != nil {
		return "", r.err
	}
	return "v1:work:approval:a1", nil
}

func missEvent() events.Event {
	return events.Event{
		Topic: events.TopicPreconditionMissed,
		Payload: map[string]any{
			"automationName":          "nightlyExport",
			"executionId":             "run-1",
			"preconditionId":          "toolPresent",
			"check":                   "fileExists",
			"literal":                 "/opt/exporter",
			"preconditionDescription": "the exporter binary is installed",
		},
	}
}

func TestWorkHealer_CarriesPatchesToAPlanReviewApproval(t *testing.T) {
	prop := &fakeProposer{patches: []healing.Patch{
		{Kind: healing.PatchRelativizeLiteral, Target: "steps.run.input.path", Replacement: "$config.exporterPath", Reason: "the path differs per machine"},
		{Kind: healing.PatchAddPrecondition, Precondition: &healing.PatchPrecondition{ID: "toolPresent", Check: "fileExists"}},
	}}
	r := &recordingRaiser{}
	h := NewWorkHealer(prop, r, nil, time.Hour)

	h.HandlePreconditionMissed(missEvent())

	if prop.calls != 1 {
		t.Fatalf("Propose called %d times, want 1", prop.calls)
	}
	if len(r.seeds) != 1 {
		t.Fatalf("raised %d approvals, want exactly one", len(r.seeds))
	}
	a := r.seeds[0]
	if a.Kind != "planReview" {
		t.Errorf("kind = %q, want planReview (D5: never a silent edit)", a.Kind)
	}
	if a.RunId != "run-1" {
		t.Errorf("runId = %q", a.RunId)
	}
	if a.ArtifactHash == "" {
		t.Error("an approval must hash what is being approved, or resume cannot tell the patches changed")
	}
	patches, _ := a.Subject["patches"].([]map[string]any)
	if len(patches) != 2 {
		t.Fatalf("the person must see the PATCHES, not a count: %+v", a.Subject)
	}
	if patches[0]["kind"] != "relativize-literal" || patches[0]["replacement"] != "$config.exporterPath" {
		t.Errorf("patch detail lost: %+v", patches[0])
	}
	if a.Evidence["tier"] != "environment" || a.Evidence["ruleId"] != "environment.literal" {
		t.Errorf("evidence = %+v", a.Evidence)
	}
	if !a.ExpiresAt.After(a.RequestedAt) {
		t.Error("a proposed repair should lapse rather than sit forever")
	}
}

// The healer PROPOSES and raises one approval. It never writes the construct.
func TestWorkHealer_NeverEditsTheConstruct(t *testing.T) {
	r := &recordingRaiser{}
	h := NewWorkHealer(&fakeProposer{patches: []healing.Patch{{Kind: healing.PatchInsertGuard, Target: "steps.run", Guard: "x == 1"}}}, r, nil, time.Hour)
	h.HandlePreconditionMissed(missEvent())
	for _, a := range r.seeds {
		if a.Kind != "planReview" {
			t.Fatalf("the heal arm raised a %q -- D5 forbids anything but carrying the patches to a person", a.Kind)
		}
	}
}

func TestWorkHealer_NoPatchesRaisesNothingAndIsNotAnError(t *testing.T) {
	r := &recordingRaiser{}
	h := NewWorkHealer(&fakeProposer{}, r, nil, time.Hour)
	h.HandlePreconditionMissed(missEvent())
	if len(r.seeds) != 0 {
		t.Fatalf("an empty proposal must not raise an empty approval: %+v", r.seeds)
	}
}

func TestWorkHealer_RefusesAMissWithNoRunToPark(t *testing.T) {
	r := &recordingRaiser{}
	h := NewWorkHealer(&fakeProposer{patches: []healing.Patch{{Kind: healing.PatchInsertGuard, Target: "s", Guard: "g"}}}, r, nil, time.Hour)
	ev := missEvent()
	delete(ev.Payload, "executionId")
	h.HandlePreconditionMissed(ev)
	if len(r.seeds) != 0 {
		t.Fatal("an approval with no run is a question nobody can answer; it must be refused rather than orphaned")
	}
}

func TestWorkHealer_ProposalFailureRaisesNothing(t *testing.T) {
	r := &recordingRaiser{}
	h := NewWorkHealer(&fakeProposer{err: errors.New("provider down")}, r, nil, time.Hour)
	h.HandlePreconditionMissed(missEvent())
	if len(r.seeds) != 0 {
		t.Fatalf("a failed proposal must not raise an approval carrying no patches: %+v", r.seeds)
	}
}

func TestNewWorkHealer_NilSeamsYieldANoOpHealer(t *testing.T) {
	if NewWorkHealer(nil, &recordingRaiser{}, nil, 0) != nil {
		t.Error("no proposer means no healer")
	}
	if NewWorkHealer(&fakeProposer{}, nil, nil, 0) != nil {
		t.Error("no raiser means no healer")
	}
	var h *WorkHealer
	h.HandlePreconditionMissed(missEvent()) // must not panic
}

// The hash covers the whole subject, so an approval for one precondition
// cannot be replayed against another that wants the same edits.
func TestWorkHealer_HashIsScopedToTheMiss(t *testing.T) {
	patches := []healing.Patch{{Kind: healing.PatchInsertGuard, Target: "steps.run", Guard: "g"}}
	hashFor := func(preconditionId string) string {
		r := &recordingRaiser{}
		h := NewWorkHealer(&fakeProposer{patches: patches}, r, nil, time.Hour)
		ev := missEvent()
		ev.Payload["preconditionId"] = preconditionId
		h.HandlePreconditionMissed(ev)
		if len(r.seeds) != 1 {
			t.Fatalf("expected one approval, got %d", len(r.seeds))
		}
		return r.seeds[0].ArtifactHash
	}
	if hashFor("toolPresent") == hashFor("networkUp") {
		t.Fatal("the same patch set against a DIFFERENT precondition must hash differently, or one approval covers both")
	}
}

// THE REGRESSION THIS FILE EXISTS FOR. The planner must not write the work
// rows itself: createWorkApproval is @serverOnly, an unstamped write is
// refused with one WARN nothing hears, and stamping is allowlisted per
// package -- integrations/planner is not on that list and must not be added.
func TestPlannerNeverStampsInternalOrigin(t *testing.T) {
	for _, f := range []string{"work_heal.go", "work_compile_adapter.go", "work_compile.go"} {
		src := readSource(t, f)
		if strings.Contains(src, "ContextWithInternalOrigin") {
			t.Fatalf("%s stamps internal origin. integrations/planner is large and request-derived and is deliberately NOT in the call-origin allowlist; the write belongs in integrations/work, which has the entry and exactly one stamping site.", f)
		}
	}
}
