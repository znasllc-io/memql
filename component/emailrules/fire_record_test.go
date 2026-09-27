package emailrules

// fire_record_test.go -- the record step's contract, without a database
// (memql#5431). fired_count_db_test.go proves the count against real replicas;
// these pin what that proof depends on, where a regression would otherwise
// only show as a slightly low number on a busy cluster.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// recordCall is one engine call and the context it arrived with.
type recordCall struct {
	q                       string
	fresh, inGate, internal bool
}

// recordEngine answers a rule whose count has MOVED since it was first read:
// an ordinary read -- the one this node's cache may answer -- sees 3, and a
// fresh read sees 7, stamped by a replica whose clock runs ahead.
type recordEngine struct {
	mu     sync.Mutex
	inGate bool
	calls  []recordCall
	ahead  time.Time
}

func (e *recordEngine) Execute(ctx context.Context, q string) (any, error) {
	e.mu.Lock()
	e.calls = append(e.calls, recordCall{q: q, fresh: memql.FreshReadFromContext(ctx), inGate: e.inGate, internal: auth.OriginFromContext(ctx).IsInternal()})
	e.mu.Unlock()
	switch {
	case strings.HasPrefix(q, "query emailRuleById"):
		row := activeRule("updated")
		row["firedCount"] = 3
		row["createdAt"] = timestamppb.New(time.Now().Add(-time.Hour))
		if memql.FreshReadFromContext(ctx) {
			row["firedCount"] = 7
			row["createdAt"] = timestamppb.New(e.ahead)
		}
		return rowsEnvelope([]map[string]any{row}), nil
	case strings.HasPrefix(q, "query templateById"):
		return rowsEnvelope([]map[string]any{{"id": "v1:campaigns:template:t1", "subject": "s", "textBody": "b"}}), nil
	case strings.HasPrefix(q, "query activeUsers"):
		return rowsEnvelope([]map[string]any{{"id": "v1:identity:user:owner1", "role": "owner", "primaryEmail": "owner@acme.com"}}), nil
	}
	return rowsEnvelope(nil), nil
}

func (e *recordEngine) gate() FiringGate {
	return func(ctx context.Context, _ string, record func(context.Context) error) error {
		e.mu.Lock()
		e.inGate = true
		e.mu.Unlock()
		defer func() {
			e.mu.Lock()
			e.inGate = false
			e.mu.Unlock()
		}()
		return record(ctx)
	}
}

// TestRecordFiringAdvancesTheCountReadFreshInsideTheGate: the record is
// derived from a count read INSIDE the gate and FRESH, never from the count
// the firing read before it sent; it is written inside the gate, stamped after
// the version it read. The early reads stay ordinary: freshness belongs to the
// one call that needs it.
func TestRecordFiringAdvancesTheCountReadFreshInsideTheGate(t *testing.T) {
	e := &recordEngine{ahead: time.Now().Add(30 * time.Second).UTC().Truncate(time.Microsecond)}
	if _, err := NewFirer(e).WithFiringGate(e.gate()).Fire(context.Background(),
		"v1:campaigns:emailRule:ab12cd34", "v1:identity:user:eve", map[string]any{"timestamp": "2026-09-27T10:00:00Z"}); err != nil {
		t.Fatal(err)
	}

	var reads, records []recordCall
	for _, c := range e.calls {
		switch {
		case strings.HasPrefix(c.q, "query emailRuleById"):
			reads = append(reads, c)
		case strings.HasPrefix(c.q, "mutation recordEmailRuleFiring"):
			records = append(records, c)
		}
	}
	if len(records) != 1 {
		t.Fatalf("wrote %d firing records, want 1", len(records))
	}
	record := records[0]
	if !record.inGate || !record.internal {
		t.Errorf("the record was written inGate=%v internal=%v, want both: outside the gate it can interleave with another firing, and unstamped the @serverOnly write is refused", record.inGate, record.internal)
	}
	if !strings.Contains(record.q, "firedCount: 8") {
		t.Errorf("the record %q does not advance the fresh count 7 to 8: it was derived from the count read before the sends", record.q)
	}
	if want := `lastFiredAt: "` + e.ahead.Add(time.Microsecond).Format(firedAtLayout) + `"`; !strings.Contains(record.q, want) {
		t.Errorf("the record %q is not stamped one microsecond after the version it read (%s)", record.q, want)
	}

	var fresh int
	for _, r := range reads {
		switch {
		case r.fresh && r.inGate:
			fresh++
		case r.fresh:
			t.Errorf("a fresh read outside the gate: %q", r.q)
		case r.inGate:
			t.Errorf("a read inside the gate that this node's cache may answer: %q", r.q)
		}
	}
	if fresh != 1 || len(reads) < 2 {
		t.Errorf("reads of the rule = %+v, want the ordinary reads before the sends and exactly one fresh read inside the gate", reads)
	}
}

// TestFiringGateWithNoDatabaseRecordsUnlocked: a gate that cannot lock records
// anyway -- an overlapping firing may lose its increment, which is the
// behaviour before the gate existed, but the record and its error still land.
func TestFiringGateWithNoDatabaseRecordsUnlocked(t *testing.T) {
	if newFiringGate(nil, quietLogger()) != nil {
		t.Fatal("a node with no database getter built a gate; it has nothing to lock with")
	}
	gate := newFiringGate(func() *bun.DB { return nil }, quietLogger())
	ran := false
	if err := gate(context.Background(), "v1:campaigns:emailRule:ab12cd34", func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("gate with no database handle: ran=%v err=%v, want the record run unlocked", ran, err)
	}
	boom := context.DeadlineExceeded
	if err := gate(context.Background(), "v1:campaigns:emailRule:ab12cd34", func(context.Context) error { return boom }); err != boom {
		t.Fatalf("the record's own error came back as %v, want %v", err, boom)
	}
}

// TestFiringGateKeyIsTheRulesNotItsSpelling: the canonical and bare spellings
// of one rule take one lock.
func TestFiringGateKeyIsTheRulesNotItsSpelling(t *testing.T) {
	if firingGateKey("v1:campaigns:emailRule:ab12cd34") != firingGateKey("ab12cd34") {
		t.Fatal("two spellings of one rule take two locks, so their firings are not serialised against each other")
	}
	if firingGateKey("ab12cd34") == firingGateKey("ab12cd35") {
		t.Error("two rules share a lock key in the fixture; pick ids that do not collide")
	}
}

// TestTheRegisteredIntegrationWiresTheFiringGate: the plug-in the engine
// registers hands the fire builtin a gate whenever the node has a database --
// over the DIRECT endpoint when there is one -- and none when it has not. A
// gate that exists in this package and is never wired is the count loss with
// extra steps.
func TestTheRegisteredIntegrationWiresTheFiringGate(t *testing.T) {
	var factory memql.PluginFactory
	for _, r := range memql.RegisteredPlugins() {
		if r.Name == IntegrationName {
			factory = r.Factory
		}
	}
	if factory == nil {
		t.Fatalf("no %q plug-in is registered", IntegrationName)
	}
	direct, main := &bun.DB{}, &bun.DB{}
	for _, c := range []struct {
		name     string
		pctx     memql.PluginContext
		wantGate bool
	}{
		{"direct and main", memql.PluginContext{DirectBunDB: func() *bun.DB { return direct }, BunDB: func() *bun.DB { return main }}, true},
		{"main only", memql.PluginContext{BunDB: func() *bun.DB { return main }}, true},
		{"no database", memql.PluginContext{}, false},
	} {
		provider, err := factory(c.pctx)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		integration, ok := provider.(*Integration)
		if !ok {
			t.Fatalf("%s: the factory built a %T", c.name, provider)
		}
		if got := integration.gate != nil; got != c.wantGate {
			t.Errorf("%s: gate wired = %v, want %v", c.name, got, c.wantGate)
		}
		if got := integration.firer().gate != nil; got != c.wantGate {
			t.Errorf("%s: the fire builtin's Firer carries a gate = %v, want %v", c.name, got, c.wantGate)
		}
	}
	if got := firingGateDB(memql.PluginContext{DirectBunDB: func() *bun.DB { return direct }, BunDB: func() *bun.DB { return main }})(); got != direct {
		t.Error("the gate locks on the main pool although the node has a direct endpoint")
	}
}
