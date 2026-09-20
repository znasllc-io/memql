package router

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/airoute"
)

// TestDecisionFieldsAreWrittenOnASuccessfulResolution pins that a hit carries
// the whole decision, not only the model it chose.
func TestDecisionFieldsAreWrittenOnASuccessfulResolution(t *testing.T) {
	args := buildRouterCallArgs(CallRecord{
		Level:          string(airoute.LevelStrong),
		RequestedLevel: string(airoute.LevelFast),
		ServedLevel:    string(airoute.LevelStrong),
		Degraded:       false,
		Rule:           "operatorReasoning",
		Policy:         "localFirst",
		Door:           airoute.DoorLocal,
		Considered: []airoute.ConsideredEntry{
			{Entry: "fleet:strongest", Door: airoute.DoorLocal, Reason: "served the call"},
		},
		Touches:          []string{"v1:library:file"},
		MinContextTokens: 4200,
		Outcome:          airoute.OutcomeOK,
	}, "abc123")

	for _, key := range []string{
		"level", "requestedLevel", "servedLevel", "degraded", "rule", "policy",
		"door", "considered", "touches", "minContextTokens", "machineOwnerUserId",
	} {
		if _, ok := args[key]; !ok {
			t.Fatalf("the ledger write omits %q; a decision nobody can read back is an assertion", key)
		}
	}
	if args["requestedLevel"] != string(airoute.LevelFast) || args["level"] != string(airoute.LevelStrong) {
		t.Fatalf("the declared and effective levels must BOTH survive: requested=%v level=%v",
			args["requestedLevel"], args["level"])
	}
	considered, _ := args["considered"].([]map[string]any)
	if len(considered) != 1 || considered[0]["entry"] != "fleet:strongest" {
		t.Fatalf("considered = %v; the door report is KEPT ON SUCCESS, not only on a refusal", args["considered"])
	}
}

// TestConsideredIsAnEmptySliceNotNil pins the distinction the ledger has to
// carry: nil would read as "no report was produced", and a resolution always
// considered at least the entry it took.
func TestConsideredIsAnEmptySliceNotNil(t *testing.T) {
	got := consideredArgs(nil)
	if got == nil {
		t.Fatal("consideredArgs returned nil; an empty report and an absent one are different claims")
	}
	if len(got) != 0 {
		t.Fatalf("consideredArgs(nil) = %v, want an empty slice", got)
	}
}

// TestDecisionFieldsAreWrittenOnARefusal pins that a park is as legible as a
// hit. The refusal path is where a person most needs the door report, and it
// is the path most easily left carrying nothing.
func TestDecisionFieldsAreWrittenOnARefusal(t *testing.T) {
	args := buildRouterCallArgs(CallRecord{
		Level:       string(airoute.LevelReasoning),
		ServedLevel: "",
		Rule:        "reasoningParks",
		Policy:      "federationStrongest",
		Considered: []airoute.ConsideredEntry{
			{Entry: "fleet:strongest", Door: airoute.DoorLocal, Reason: "no machine offering it is online"},
			{Entry: "app:*", Door: airoute.DoorApp, Reason: "no signed-in app on any of this user's machines"},
		},
		Outcome: "every_door_shut",
	}, "def456")

	if args["rule"] != "reasoningParks" || args["policy"] != "federationStrongest" {
		t.Fatalf("a refusal must still name the rule and policy that produced it: %v / %v", args["rule"], args["policy"])
	}
	considered, _ := args["considered"].([]map[string]any)
	if len(considered) != 2 {
		t.Fatalf("considered = %v, want both doors and their reasons", args["considered"])
	}
	if considered[1]["reason"] == "" {
		t.Error("a door with no reason reads as a bug in the report")
	}
}

// TestRouterCallHasNoBroadcastRoutingRuleAndHereIsWhy is a NOTE with a
// failure mode.
//
// v1:router:call is deliberately absent from component/node/routing.go, on the
// same volume grounds that keep v1:worker:invocation out: one row per model
// call, on every node, forwarded to every replica, is a firehose whose only
// consumer is a list somebody opens occasionally. routerDecisionsRecent reads
// them on demand instead.
//
// The test exists because "for consistency with the other work rows" is a
// convincing sentence, and a later session adding the rule would find nothing
// arguing back. Adding it fails here and reads the reason.
func TestRouterCallHasNoBroadcastRoutingRuleAndHereIsWhy(t *testing.T) {
	path := filepath.Join("..", "node", "routing.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("component/node/routing.go is not readable from this module: %v", err)
	}
	if strings.Contains(string(raw), "v1:router:call") {
		t.Fatal("component/node/routing.go now carries a rule for v1:router:call. " +
			"That row is one per MODEL CALL on every node -- the same volume argument that " +
			"excludes v1:worker:invocation -- and broadcasting it forwards a firehose to every " +
			"replica for a list somebody opens occasionally. Read it on demand through " +
			"routerDecisionsRecent instead. If a live decisions feed is genuinely wanted, it " +
			"needs a sampling or filtering decision first, not a broadcast rule.")
	}
}

// servedModelStub is a provider that REPORTS what ran, the way an app session
// does. A vendor provider implements nothing of the sort, which is the other
// half of the test below.
type servedModelStub struct{ model, effort string }

func (s *servedModelStub) ServedModel() (string, string) { return s.model, s.effort }

// servedModel is the SURFACE'S REPORT, never the request's pin (epic
// memql#5391, design D9).
//
// The two differ exactly when it matters -- an app that rerouted, or one that
// ignored the model it was given -- and a servedModel copied from the
// resolution would record as measured something nobody measured, in the one
// case anybody would want to look at.
func TestServedModelIsTheReportAndNotThePin(t *testing.T) {
	rec := buildRecord(
		ResolveRequest{RequestId: "req-1"},
		// The chain resolved the DOOR; `model` is the app id, which is what an
		// app entry's registry record carries.
		Resolved{ProviderName: "app:claude-code:claude-sonnet-4-6", Model: "claude-code"},
		&servedModelStub{model: "claude-opus-5", effort: "high"},
		0, 0, 0,
		time.Now(), time.Time{}, time.Now(),
		false, nil, nil,
	)
	if rec.ServedModel != "claude-opus-5" {
		t.Fatalf("ServedModel = %q, want the app's report", rec.ServedModel)
	}
	if rec.ServedEffort != "high" {
		t.Fatalf("ServedEffort = %q", rec.ServedEffort)
	}
	if rec.Model != "claude-code" {
		t.Fatalf("Model must stay what the chain resolved, got %q", rec.Model)
	}

	args := buildRouterCallArgs(rec, "abc123")
	if args["servedModel"] != "claude-opus-5" || args["servedEffort"] != "high" {
		t.Fatalf("the ledger write must carry the report: %v / %v", args["servedModel"], args["servedEffort"])
	}
}

// A provider that reports nothing leaves both EMPTY. Empty is "the surface did
// not say", which the row records as unknown -- never the requested value, and
// never a guess. It is also the ordinary reading for every vendor provider,
// where what was asked for is what ran and `model` already says it.
func TestServedModelIsEmptyWhenNobodyReported(t *testing.T) {
	rec := buildRecord(
		ResolveRequest{RequestId: "req-2"},
		Resolved{ProviderName: "streamClaudeSonnet", Model: "claude-sonnet-4-6"},
		struct{}{},
		0, 0, 0,
		time.Now(), time.Time{}, time.Now(),
		false, nil, nil,
	)
	if rec.ServedModel != "" || rec.ServedEffort != "" {
		t.Fatalf("a provider that reports nothing leaves both empty, got (%q, %q)", rec.ServedModel, rec.ServedEffort)
	}
}

// An app that reported a MODEL but no EFFORT leaves the effort empty rather
// than borrowing the level or the request's pin. Claude Code's headless output
// states no effort, so this is the common case rather than the edge one, and a
// token count spent at an unknown effort cannot be compared with one spent at
// a known one.
func TestServedEffortIsEmptyWhenTheAppStatedNone(t *testing.T) {
	rec := buildRecord(
		ResolveRequest{RequestId: "req-3", Level: airoute.LevelReasoning},
		Resolved{ProviderName: "app:claude-code", Model: "claude-code"},
		&servedModelStub{model: "claude-opus-5"},
		0, 0, 0,
		time.Now(), time.Time{}, time.Now(),
		false, nil, nil,
	)
	if rec.ServedModel != "claude-opus-5" {
		t.Fatalf("ServedModel = %q", rec.ServedModel)
	}
	if rec.ServedEffort != "" {
		t.Fatalf("ServedEffort = %q, want empty -- never the level, never a guess", rec.ServedEffort)
	}
}
