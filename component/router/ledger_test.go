package router

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// countingLedger is a fake engine that records every ledger write, the peak
// number of writes in flight at once, and can be made to block.
type countingLedger struct {
	writes     chan string
	inFlight   atomic.Int64
	peak       atomic.Int64
	gate       chan struct{} // nil unless the writer should block
	executions atomic.Int64
}

func (e *countingLedger) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	now := e.inFlight.Add(1)
	for {
		peak := e.peak.Load()
		if now <= peak || e.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	if e.gate != nil {
		<-e.gate
	}
	e.executions.Add(1)
	e.inFlight.Add(-1)
	select {
	case e.writes <- query:
	default:
	}
	return &memql.ExecuteResult{}, nil
}

func (e *countingLedger) args(t *testing.T) map[string]any {
	t.Helper()
	select {
	case query := <-e.writes:
		expr, err := langparser.ParseExpression(query)
		if err != nil {
			t.Fatal(err)
		}
		call, ok := expr.(*langparser.FunctionCallExpr)
		if !ok || call.Name != "recordRouterCall" {
			t.Fatalf("unexpected ledger invocation: %s", query)
		}
		return call.Args
	case <-time.After(3 * time.Second):
		t.Fatal("the ledger never wrote its record")
		return nil
	}
}

// ---------------------------------------------------------------------------
// Attribution: the caller kind is on every row, and never blank.
// ---------------------------------------------------------------------------

// A ROW THAT NAMES NO PERSON SAYS WHICH KIND OF CALLER IT WAS (memql#5581).
// Before this, a blank userId answered both "a person made this call and
// nobody wrote down which one" and "no person made this call at all".
func TestLedgerRowNamesTheKindOfCallerEvenWithNoPerson(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		user string
		want string
	}{
		{"a signed-in person", auth.ContextWithAccess(context.Background(),
			&auth.AccessContext{UserId: "alice", Role: auth.RoleWriter}), "alice", auth.CallerKindUser},
		{"the cluster's own sweep", auth.ContextWithAccess(context.Background(),
			auth.MaintenanceActor("auditEventRetentionSweep")), "", auth.CallerKindSystem},
		{"nobody stamped anything", context.Background(), "", auth.CallerKindUnattributed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := levelRouter(t, airoute.LevelStrong, nil)
			ledger := &countingLedger{writes: make(chan string, 4)}
			r.engine = ledger

			resolved, err := r.ResolveFor(tc.ctx, ResolveRequest{Level: airoute.LevelStrong, Modality: airoute.ModalityChat})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resolved.Client.(common.ChatAIProvider).CallChat(tc.ctx, nil); err != nil {
				t.Fatal(err)
			}
			args := ledger.args(t)
			if args["callerKind"] != tc.want {
				t.Errorf("callerKind=%v, want %q", args["callerKind"], tc.want)
			}
			if args["callerKind"] == "" {
				t.Error("a blank caller kind is the one answer the field must never carry")
			}
			// The sweep and the unstamped call carry no user, which is now an
			// answer rather than a gap.
			if tc.user == "" && args["userId"] != "" && args["userId"] != auth.AnonymousUserId {
				if !strings.HasPrefix(fmt.Sprint(args["userId"]), "system:") {
					t.Errorf("userId=%v, want none for %s", args["userId"], tc.name)
				}
			}
		})
	}
}

// An unrecognised caller kind is normalized rather than stored: the concept
// declares a CLOSED five-value enum, and a sixth string would be refused at
// write time and lose the whole row.
func TestUnknownCallerKindIsNormalizedNotStored(t *testing.T) {
	for _, in := range []string{"", "operator", "USER", " user "} {
		if got := callerKindOrUnattributed(in); got != auth.CallerKindUnattributed {
			t.Errorf("callerKindOrUnattributed(%q) = %q, want %q", in, got, auth.CallerKindUnattributed)
		}
	}
	for _, in := range auth.CallerKinds() {
		if got := callerKindOrUnattributed(in); got != in {
			t.Errorf("callerKindOrUnattributed(%q) = %q; a legal kind must survive", in, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Cache hits: distinguishable in the record, and spending nothing.
// ---------------------------------------------------------------------------

func cacheResolution() airoute.Resolution {
	return airoute.Resolution{
		ProviderName: "chat54Mini",
		Vendor:       "openai",
		Model:        "gpt-5.4-mini",
		Decision: airoute.Decision{
			Level: airoute.LevelStrong, RequestedLevel: airoute.LevelStrong, ServedLevel: airoute.LevelStrong,
			Rule: "defaultChat", Policy: "localFirst", Door: airoute.DoorFederation,
			Considered: []airoute.ConsideredEntry{
				{Entry: "fleet:strongest", Door: airoute.DoorLocal, Reason: "no local model is online for this user"},
				{Entry: "chat54Mini", Door: airoute.DoorFederation, Reason: "selected"},
			},
			MinContextTokens: 8192,
		},
	}
}

// A CACHE HIT IS TELLABLE FROM A PROVIDER CALL IN THE RECORD, and its spend
// figures agree with what happened: nothing was sent, so every token and every
// cost is a real zero (memql#5581).
func TestCacheServedRowNamesTheCacheAndSpendsNothing(t *testing.T) {
	r, _ := levelRouter(t, airoute.LevelStrong, nil)
	ledger := &countingLedger{writes: make(chan string, 4)}
	r.engine = ledger

	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "alice", Role: auth.RoleWriter})
	r.RecordCacheServed(ctx, ResolveRequest{
		Level: airoute.LevelStrong, Modality: airoute.ModalityChat,
		PromptName: "agentReply", RequestId: "run:v1:work:run:r1#summarise",
	}, cacheResolution(), airoute.CacheKindExact, 2)

	args := ledger.args(t)
	if args["cacheKind"] != airoute.CacheKindExact {
		t.Fatalf("cacheKind=%v, want %q -- a reader cannot tell this from a provider call",
			args["cacheKind"], airoute.CacheKindExact)
	}
	for key, want := range map[string]any{
		"inputTokens": 0, "outputTokens": 0, "cachedInputTokens": 0,
	} {
		if fmt.Sprint(args[key]) != fmt.Sprint(want) {
			t.Errorf("%s=%v on a cache row, want %v: no tokens were spent", key, args[key], want)
		}
	}
	for _, key := range []string{"inputCost", "outputCost", "cachedInputCost", "totalCost"} {
		if fmt.Sprint(args[key]) != "0" {
			t.Errorf("%s=%v on a cache row; MemQL was not billed for an answer it did not ask for", key, args[key])
		}
	}
	for _, key := range []string{"tokensEstimated", "pricingConfigured", "streaming", "degraded"} {
		if args[key] != false {
			t.Errorf("%s=%v on a cache row; nothing was estimated and no cost was computed", key, args[key])
		}
	}
	// The attribution and the whole decision travel with it.
	if args["userId"] != "alice" || args["callerKind"] != auth.CallerKindUser {
		t.Errorf("cache row lost its attribution: userId=%v callerKind=%v", args["userId"], args["callerKind"])
	}
	if args["rule"] != "defaultChat" || args["policy"] != "localFirst" || args["door"] != airoute.DoorFederation {
		t.Errorf("cache row lost its decision: %v / %v / %v", args["rule"], args["policy"], args["door"])
	}
	// CONSIDERED IS NOT WEAKENED. The calls a cache serves are the ones that
	// repeat, so dropping their door report would make them the decisions the
	// rule corpus can never be checked against.
	considered, ok := args["considered"].([]any)
	if !ok || len(considered) != 2 {
		t.Fatalf("considered=%#v, want the two entries the walk reported", args["considered"])
	}
	if args["outcome"] != "ok" {
		t.Errorf("outcome=%v, want ok: the call was answered", args["outcome"])
	}
}

// AN ORDINARY PROVIDER ROW CARRIES NO cacheKind AT ALL. Absent is what says "a
// provider answered", and sending "" would be refused by the two-value enum
// and lose the whole row.
func TestProviderRowOmitsCacheKind(t *testing.T) {
	args := buildRouterCallArgs(CallRecord{Outcome: "ok"}, "call-1")
	if _, present := args["cacheKind"]; present {
		t.Fatalf("a provider row sent cacheKind=%#v; absent is what means 'a provider answered'", args["cacheKind"])
	}
	rendered, err := langparser.RenderCall("recordRouterCall", args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, "cacheKind") {
		t.Fatalf("cacheKind reached the mutation on a provider row: %s", rendered)
	}
}

// An unknown cache kind records nothing rather than writing a row the enum
// would refuse.
func TestUnknownCacheKindRecordsNothing(t *testing.T) {
	r, _ := levelRouter(t, airoute.LevelStrong, nil)
	ledger := &countingLedger{writes: make(chan string, 4)}
	r.engine = ledger
	r.RecordCacheServed(context.Background(), ResolveRequest{Level: airoute.LevelStrong}, cacheResolution(), "guess", 1)
	select {
	case query := <-ledger.writes:
		t.Fatalf("an unknown cache kind wrote a row: %s", query)
	case <-time.After(200 * time.Millisecond):
	}
}

// ---------------------------------------------------------------------------
// Write cost: one bounded queue, one writer, counted drops.
// ---------------------------------------------------------------------------

// THE LEDGER COSTS ONE GOROUTINE AND ONE CONCURRENT WRITE, not one of each per
// model call (memql#5581). What this replaced spawned a goroutine per record,
// each rendering a ~1.1 KB mutation, parsing it and taking a database
// connection at exactly the moment the node was busiest.
func TestLedgerWritesAreSerialisedThroughOneWriter(t *testing.T) {
	const records = 64
	r, _ := levelRouter(t, airoute.LevelStrong, nil)
	ledger := &countingLedger{writes: make(chan string, records)}
	r.engine = ledger

	for i := 0; i < records; i++ {
		r.recordCall(CallRecord{RequestId: fmt.Sprint("req-", i), Outcome: "ok"})
	}
	deadline := time.Now().Add(5 * time.Second)
	for ledger.executions.Load() < records && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := ledger.executions.Load(); got != records {
		t.Fatalf("%d ledger writes for %d records, want %d", got, records, records)
	}
	if peak := ledger.peak.Load(); peak != 1 {
		t.Fatalf("peak concurrent ledger writes = %d, want 1: the records are competing with the work they describe", peak)
	}
	if dropped := r.RecordsDropped(); dropped != 0 {
		t.Fatalf("dropped %d records with a queue of %d and nothing blocking", dropped, LedgerQueueDepth)
	}
}

// THE QUEUE IS THE BOUNDARY, and past it a record is DROPPED AND COUNTED
// rather than queued forever. A node whose database cannot keep up with its
// own model calls must not grow its heap until it dies.
func TestLedgerQueueOverflowIsDroppedAndCounted(t *testing.T) {
	r, _ := levelRouter(t, airoute.LevelStrong, nil)
	gate := make(chan struct{})
	ledger := &countingLedger{writes: make(chan string, 1), gate: gate}
	r.engine = ledger

	// Fill the queue, plus the one record the writer has taken off it, plus
	// the overflow.
	const overflow = 8
	for i := 0; i < LedgerQueueDepth+overflow+1; i++ {
		r.recordCall(CallRecord{RequestId: fmt.Sprint("req-", i), Outcome: "ok"})
	}
	if dropped := r.RecordsDropped(); dropped == 0 {
		t.Fatalf("no drops after %d records against a queue of %d", LedgerQueueDepth+overflow+1, LedgerQueueDepth)
	} else if dropped > overflow+1 {
		t.Fatalf("dropped %d records; the queue should have absorbed all but about %d", dropped, overflow)
	}
	close(gate)
}

// A router with no engine drops and counts rather than starting a writer that
// would dereference nil -- a panic on a goroutine nobody recovers is fatal.
func TestEngineLessRouterDropsWithoutAWriter(t *testing.T) {
	r := &Router{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r.recordCall(CallRecord{Outcome: "ok"})
	if r.RecordsDropped() != 1 {
		t.Fatalf("dropped %d, want 1", r.RecordsDropped())
	}
	if r.ledger != nil {
		t.Fatal("a router with no engine started a ledger writer")
	}
}

// ---------------------------------------------------------------------------
// The mutation and the shape still take what the router sends.
// ---------------------------------------------------------------------------

// EVERY ARG THE ROUTER SENDS IS DECLARED AND ACCEPTED. A mutation that does
// not declare an argument refuses the call, which loses the WHOLE row -- the
// failure shape memql#1244 records, and the one a new field reintroduces most
// easily.
func TestRecordRouterCallDeclaresEveryArgTheRouterSends(t *testing.T) {
	source := readDSL(t, "../../dsl/router/mutations.memql")
	args := buildRouterCallArgs(CallRecord{Outcome: "ok", CacheKind: airoute.CacheKindSemantic}, "call-1")
	accept := acceptList(t, source)
	stamp := source[strings.Index(source, "stamp {"):]
	for name := range args {
		if !strings.Contains(source, "\n    "+name+" ") && !strings.Contains(source, "\n    "+name+"  ") {
			t.Errorf("recordRouterCall does not declare the arg %q the router sends", name)
		}
		// An arg reaches the row through accept{} or through a stamp{} line
		// that reads it -- `callId` becomes the row id and `billing` is
		// stamped so its documented `?? \"metered\"` default is real.
		if accept[name] || strings.Contains(stamp, "args."+name) || strings.Contains(stamp, name+":") {
			continue
		}
		t.Errorf("recordRouterCall declares %q and neither accepts nor stamps it, so it would never reach the row", name)
	}
}

// THE EXISTING READ STILL RETURNS WHAT IT DID. routerDecisionsRecent projects
// the routerDecision shape, so every field it carried before memql#5581 must
// still be in the body -- the two new ones are additions beside them.
func TestRouterDecisionShapeKeepsItsProjectionAndGainsTheTwoNewFields(t *testing.T) {
	body := shapeBody(t, readDSL(t, "../../dsl/router/shapes.memql"), "routerDecision")
	before := []string{
		"row.id", "row.createdAt", "requestId", "promptName", "level", "requestedLevel",
		"servedLevel", "degraded", "rule", "policy", "door", "providerName", "vendor",
		"model", "servedModel", "servedEffort", "considered", "touches", "minContextTokens",
		"machineOwnerUserId", "totalCost", "totalDurationMs", "outcome", "billing",
		"executionSurface",
	}
	for _, field := range before {
		if !body[field] {
			t.Errorf("routerDecision no longer projects %q; the existing read changed", field)
		}
	}
	for _, field := range []string{"callerKind", "cacheKind"} {
		if !body[field] {
			t.Errorf("routerDecision does not project %q, so the reader cannot see it", field)
		}
	}
	if len(body) != len(before)+2 {
		t.Errorf("routerDecision projects %d fields, want the %d it had plus the two new ones", len(body), len(before))
	}
}

func readDSL(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func acceptList(t *testing.T, source string) map[string]bool {
	t.Helper()
	start := strings.Index(source, "accept {")
	if start < 0 {
		t.Fatal("recordRouterCall has no accept block")
	}
	end := strings.Index(source[start:], "}")
	out := map[string]bool{}
	for _, name := range strings.Split(source[start+len("accept {"):start+end], ",") {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			out[trimmed] = true
		}
	}
	return out
}

func shapeBody(t *testing.T, source, name string) map[string]bool {
	t.Helper()
	start := strings.Index(source, "shape call "+name+" {")
	if start < 0 {
		t.Fatalf("no shape %q in the source", name)
	}
	rest := source[start:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		t.Fatalf("shape %q is unterminated", name)
	}
	out := map[string]bool{}
	for _, line := range strings.Split(rest[:end], "\n")[1:] {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "//") {
			continue
		}
		out[line] = true
	}
	return out
}
