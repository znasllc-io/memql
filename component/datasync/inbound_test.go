package datasync

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

const testMirrorConcept = "v1:shopify:product"

func testSpecs(versionField string) map[string]memqlsync.DomainSpec {
	return map[string]memqlsync.DomainSpec{
		testMirrorConcept: {Concept: testMirrorConcept, VersionField: versionField, Direction: memqlsync.DirectionInbound},
	}
}

// THE VERSION GUARD (D6). An out-of-order webhook leaves the newer row
// alone and is recorded as stale.
func TestAnOutOfOrderWriteLeavesTheNewerRowAndIsRecordedStale(t *testing.T) {
	writer := newFakeWriter()
	writer.versions[testMirrorConcept+"|p1"] = "2026-08-23T12:00:00Z"
	a := NewApplier(NewStore(newFakeEngine()), writer)

	res, err := a.Apply(context.Background(), "shopify", testSpecs("updatedAt"), []memqlsync.MirrorWrite{
		{Concept: testMirrorConcept, RowId: "p1", Version: "2026-08-23T11:00:00Z", Payload: map[string]any{"handle": "old"}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied != 0 || res.Stale != 1 {
		t.Fatalf("applied=%d stale=%d, want 0/1 -- an older delivery must not overwrite newer data", res.Applied, res.Stale)
	}
	if len(writer.written) != 0 {
		t.Errorf("a stale write was applied anyway: %+v", writer.written)
	}
	// Recorded, not silently dropped: a mirror that keeps rejecting stale
	// writes is a webhook stream that is reordering, and an operator who
	// cannot see that has no way to know.
	if res.Stale != 1 {
		t.Error("the stale write was not counted")
	}
}

func TestANewerWriteIsApplied(t *testing.T) {
	writer := newFakeWriter()
	writer.versions[testMirrorConcept+"|p1"] = "2026-08-23T11:00:00Z"
	a := NewApplier(NewStore(newFakeEngine()), writer)

	res, err := a.Apply(context.Background(), "shopify", testSpecs("updatedAt"), []memqlsync.MirrorWrite{
		{Concept: testMirrorConcept, RowId: "p1", Version: "2026-08-23T12:00:00Z"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied != 1 || res.Stale != 0 {
		t.Fatalf("applied=%d stale=%d, want 1/0", res.Applied, res.Stale)
	}
}

// A row MemQL does not have is never stale: a first delivery has nothing
// to be older than, and refusing it would leave the mirror empty forever.
func TestAFirstDeliveryIsNeverStale(t *testing.T) {
	a := NewApplier(NewStore(newFakeEngine()), newFakeWriter())
	res, err := a.Apply(context.Background(), "shopify", testSpecs("updatedAt"), []memqlsync.MirrorWrite{
		{Concept: testMirrorConcept, RowId: "brand-new", Version: "1999-01-01T00:00:00Z"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied != 1 {
		t.Errorf("applied=%d, want 1 -- a row we do not have cannot be older than one we do", res.Applied)
	}
}

// A connector that cannot say when a change happened gets last-write-
// wins, which is what it had before the guard existed.
func TestAWriteCarryingNoVersionIsAlwaysApplied(t *testing.T) {
	writer := newFakeWriter()
	writer.versions[testMirrorConcept+"|p1"] = "2026-08-23T12:00:00Z"
	a := NewApplier(NewStore(newFakeEngine()), writer)

	res, err := a.Apply(context.Background(), "shopify", testSpecs("updatedAt"), []memqlsync.MirrorWrite{
		{Concept: testMirrorConcept, RowId: "p1"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Applied != 1 {
		t.Errorf("applied=%d, want 1", res.Applied)
	}
}

// Timestamps are compared as INSTANTS. Two spellings of one moment must
// order equal, or every row with a differently-formatted origin version
// compares wrong.
func TestVersionsAreComparedAsInstantsNotStrings(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"2026-08-23T12:00:00Z", "2026-08-23T12:00:00+00:00", 0},
		{"2026-08-23T11:00:00Z", "2026-08-23T12:00:00Z", -1},
		{"2026-08-23T13:00:00Z", "2026-08-23T12:00:00Z", 1},
		// A zero-padded sequence has no timestamp reading, so it falls
		// back to a lexicographic compare -- which is correct for it.
		{"000000123", "000000124", -1},
	}
	for _, tc := range cases {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// testDispatcher wires a dispatcher over one fake connector.
func testDispatcher(engine *fakeEngine, writer MirrorWriter, c *fakeConnector) *Dispatcher {
	store := NewStore(engine)
	d := NewDispatcher(store, NewApplier(store, writer))
	d.lookup = func(_ context.Context, name string) (memqlsync.Connector, bool) {
		if name == c.name {
			return c, true
		}
		return nil, false
	}
	d.now = func() time.Time { return testNow }
	return d
}

// The dispatcher stamps the staged request either way, so the queue does
// not grow without bound and lose its meaning as a to-do list.
func TestTheDispatcherStampsTheRequestRow(t *testing.T) {
	cases := []struct {
		name       string
		connector  *fakeConnector
		wantStatus string
		wantErr    bool
	}{
		{
			name: "a delivery it applies",
			connector: &fakeConnector{
				name:        "shopify",
				domains:     []memqlsync.DomainSpec{{Concept: testMirrorConcept, Direction: memqlsync.DirectionInbound}},
				applyWrites: []memqlsync.MirrorWrite{{Concept: testMirrorConcept, RowId: "p1", Version: "2026-08-23T12:00:00Z"}},
			},
			wantStatus: `status: "processed"`,
		},
		{
			name: "a delivery it does not recognise",
			connector: &fakeConnector{
				name:    "shopify",
				domains: []memqlsync.DomainSpec{{Concept: testMirrorConcept}},
			},
			wantStatus: `status: "processed"`,
		},
		{
			name: "a delivery whose apply fails",
			connector: &fakeConnector{
				name:     "shopify",
				domains:  []memqlsync.DomainSpec{{Concept: testMirrorConcept}},
				applyErr: errors.New("origin unreachable"),
			},
			wantStatus: `status: "failed"`,
			wantErr:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := newFakeEngine()
			d := testDispatcher(engine, newFakeWriter(), tc.connector)

			_, err := d.Dispatch(context.Background(), memqlsync.InboundRequest{
				RequestId: "req-1", Source: "shopify", ReceivedAt: testNow,
			})
			if tc.wantErr && err == nil {
				t.Fatal("Dispatch reported success for a failed apply")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			stamps := engine.callsContaining("updateInboundRequestStatus")
			if len(stamps) != 1 {
				t.Fatalf("the request was stamped %d times, want 1 -- an unstamped row stays `received` forever", len(stamps))
			}
			if !strings.Contains(stamps[0], tc.wantStatus) {
				t.Errorf("stamped %q, want it to contain %q", stamps[0], tc.wantStatus)
			}
		})
	}
}

// updateInboundRequestStatus is @serverOnly (memql#5707 follow-up), so the
// dispatcher's stamp is refused unless its context carries internal origin
// -- and d.stamp discards the error, so a refused stamp is SILENT: the row
// stays `received` while the connector's work is done. The stamp must carry
// the origin itself, through OperatorContext, whatever context Dispatch was
// handed; here that context is explicitly CLIENT, so inheritance cannot
// satisfy the assertion. Every exit that stamps is covered: processed after
// an apply, processed for a delivery the connector does not recognise, and
// failed for an apply error.
func TestTheDispatchersStampCarriesInternalOrigin(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *fakeConnector
	}{
		{"applied", &fakeConnector{name: "shopify", domains: []memqlsync.DomainSpec{{Concept: testMirrorConcept}},
			applyWrites: []memqlsync.MirrorWrite{{Concept: testMirrorConcept, RowId: "p1", Version: testNow.Format(time.RFC3339)}}}},
		{"unrecognised", &fakeConnector{name: "shopify"}},
		{"apply failed", &fakeConnector{name: "shopify", applyErr: errors.New("vendor said no")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := newFakeEngine()
			d := testDispatcher(engine, newFakeWriter(), tc.c)
			_, _ = d.Dispatch(auth.ContextWithClientOrigin(context.Background()),
				memqlsync.InboundRequest{RequestId: "r", Source: "shopify", ReceivedAt: testNow})
			origins := engine.internalContaining("updateInboundRequestStatus")
			if len(origins) != 1 {
				t.Fatalf("stamped %d times, want 1", len(origins))
			}
			if !origins[0] {
				t.Error("the request-row stamp ran without internal origin; updateInboundRequestStatus is " +
					"@serverOnly, so the engine refuses it and d.stamp drops the error")
			}
		})
	}
}

// A staged row whose metadata cannot be parsed is stamped `failed` once a
// connector claims it and before Apply is called, not left `received` to
// re-fire identically on every re-stage.
func TestUnparseableStagedMetadataStampsTheRowFailed(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"headers that are not a JSON object", map[string]any{"headersJson": "[]"}},
		{"a receipt time that is not RFC3339", map[string]any{"receivedAt": "2026-09-27T23:59:60Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := newFakeEngine()
			c := &fakeConnector{name: "shopify", domains: []memqlsync.DomainSpec{{Concept: testMirrorConcept}}}
			i := &Integration{dispatcher: testDispatcher(engine, newFakeWriter(), c)}
			args := map[string]any{"inboundRequestId": "req-bad", "source": "shopify", "body": "{}"}
			for k, v := range tc.args {
				args[k] = v
			}
			if _, err := i.handleDispatchInbound(auth.ContextWithInternalOrigin(context.Background()), args, 0); err == nil {
				t.Fatal("unparseable metadata was dispatched as if it were valid")
			}
			stamps := engine.callsContaining("updateInboundRequestStatus")
			if len(stamps) != 1 || !strings.Contains(stamps[0], `status: "failed"`) {
				t.Fatalf("stamped %q, want exactly one `failed` stamp", stamps)
			}
			if c.applyCalls != 0 {
				t.Fatal("the connector was asked to apply a delivery whose metadata never parsed")
			}
		})
	}
}

// The same unreadable metadata on a source NO connector serves is not the
// dispatcher's to judge. Dispatch's contract is that such a row is a no-op,
// left `received` for the product automation that does serve it -- which may
// read none of the metadata and have no quarrel with it. So the metadata is
// parsed only after a connector claims the row: here nothing is stamped, no
// error is raised, and the builtin reports the row skipped.
func TestUnparseableMetadataOnAnUnservedSourceIsLeftAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"headers that are not a JSON object", map[string]any{"headersJson": "[]"}},
		{"a receipt time that is not RFC3339", map[string]any{"receivedAt": "2026-09-27T23:59:60Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := newFakeEngine()
			c := &fakeConnector{name: "shopify", domains: []memqlsync.DomainSpec{{Concept: testMirrorConcept}}}
			i := &Integration{dispatcher: testDispatcher(engine, newFakeWriter(), c)}
			args := map[string]any{"inboundRequestId": "req-other", "source": "stripe", "body": "{}"}
			for k, v := range tc.args {
				args[k] = v
			}
			nodes, err := i.handleDispatchInbound(auth.ContextWithInternalOrigin(context.Background()), args, 0)
			if err != nil {
				t.Fatalf("a row no connector serves raised %v -- it is the product automation's, not a dispatch failure", err)
			}
			if n := engine.countContaining("updateInboundRequestStatus"); n != 0 {
				t.Fatalf("a row no connector serves was stamped %d times; it belongs to whatever does serve it", n)
			}
			if len(nodes) != 1 || !strings.Contains(string(nodes[0].Payload), `"outcome":"skipped"`) {
				t.Fatalf("result %v, want one skipped node", nodes)
			}
		})
	}
}

// A source no connector serves is skipped, not failed: /inbound/{source}
// is a shared door and most of what comes through belongs to something
// else.
func TestADeliveryForAnUnservedSourceIsSkipped(t *testing.T) {
	engine := newFakeEngine()
	d := testDispatcher(engine, newFakeWriter(), &fakeConnector{name: "shopify"})

	res, err := d.Dispatch(context.Background(), memqlsync.InboundRequest{RequestId: "r", Source: "stripe"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Handled {
		t.Error("a source nothing serves reported itself handled")
	}
	if engine.countContaining("updateInboundRequestStatus") != 0 {
		t.Error("a request nothing served was stamped -- it belongs to whatever does serve it")
	}
}

// Applying a delivery records the domain's health, so an operator can
// see how far behind the mirror is.
func TestApplyingADeliveryRecordsInboundHealth(t *testing.T) {
	engine := newFakeEngine()
	c := &fakeConnector{
		name:    "shopify",
		domains: []memqlsync.DomainSpec{{Concept: testMirrorConcept, Direction: memqlsync.DirectionInbound}},
		applyWrites: []memqlsync.MirrorWrite{
			{Concept: testMirrorConcept, RowId: "p1", Version: testNow.Add(-90 * time.Second).Format(time.RFC3339)},
		},
	}
	d := testDispatcher(engine, newFakeWriter(), c)

	if _, err := d.Dispatch(context.Background(), memqlsync.InboundRequest{RequestId: "r", Source: "shopify", ReceivedAt: testNow}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	writes := engine.callsContaining("upsertSyncState")
	if len(writes) != 1 {
		t.Fatalf("syncState written %d times, want 1", len(writes))
	}
	// Lag is measured from the ORIGIN's version, not from staging: the
	// number an operator needs is how far behind the system of record is.
	if !strings.Contains(writes[0], "lagSeconds: 90") {
		t.Errorf("health write %q does not record the 90s lag from the origin's version", writes[0])
	}
}

// dispatchInbound is a builtin, so it carries its gate in the handler: a
// call whose context does not carry internal origin -- a client, or a
// product bundle's logic, choosing the source, body and headers -- is refused
// before any argument is read, the connector is never asked and the row is
// never stamped. The automation executor's step context carries the origin,
// which is the one legitimate caller (proved end to end by
// test/inboundhop against Postgres).
func TestDispatchInboundRefusesACallWithoutInternalOrigin(t *testing.T) {
	args := map[string]any{
		"inboundRequestId": "req-forged", "source": "shopify", "body": `{"shop_domain":"acme.myshopify.com"}`,
		"headersJson": `{"x-shopify-topic":"shop/redact"}`,
	}
	t.Run("client origin", func(t *testing.T) {
		engine := newFakeEngine()
		c := &fakeConnector{name: "shopify", domains: []memqlsync.DomainSpec{{Concept: testMirrorConcept}}}
		i := &Integration{dispatcher: testDispatcher(engine, newFakeWriter(), c)}
		if _, err := i.handleDispatchInbound(context.Background(), args, 0); err == nil {
			t.Fatal("a client-origin call reached the dispatcher")
		}
		if c.applyCalls != 0 {
			t.Fatal("the connector was asked to apply a client-authored delivery")
		}
		if engine.countContaining("updateInboundRequestStatus") != 0 {
			t.Fatal("a refused call stamped the row")
		}
	})
	t.Run("internal origin", func(t *testing.T) {
		engine := newFakeEngine()
		c := &fakeConnector{name: "shopify", domains: []memqlsync.DomainSpec{{Concept: testMirrorConcept}}}
		i := &Integration{dispatcher: testDispatcher(engine, newFakeWriter(), c)}
		if _, err := i.handleDispatchInbound(auth.ContextWithInternalOrigin(context.Background()), args, 0); err != nil {
			t.Fatalf("the automation path was refused: %v", err)
		}
		if c.applyCalls != 1 {
			t.Fatalf("Apply called %d times, want 1", c.applyCalls)
		}
		if stamps := engine.callsContaining("updateInboundRequestStatus"); len(stamps) != 1 || !strings.Contains(stamps[0], `status: "processed"`) {
			t.Fatalf("stamped %q, want one processed stamp", stamps)
		}
	})
}

// reconcileDomains is the other automation-driven builtin and carries the
// same gate, ahead of anything else: a client-origin call is refused before
// the runner is touched.
func TestReconcileDomainsRefusesACallWithoutInternalOrigin(t *testing.T) {
	t.Run("client origin", func(t *testing.T) {
		i := &Integration{} // no runner: the gate must answer before anything is dereferenced
		if _, err := i.handleReconcileDomains(context.Background(), nil, 0); err == nil {
			t.Fatal("a client-origin call reached the reconcile runner")
		}
	})
	t.Run("internal origin", func(t *testing.T) {
		store := NewStore(newFakeEngine())
		i := &Integration{runner: NewRunner(store, NewApplier(store, newFakeWriter()), discardLogger()), logger: discardLogger()}
		if _, err := i.handleReconcileDomains(auth.ContextWithInternalOrigin(context.Background()), nil, 0); err != nil {
			t.Fatalf("the cron's own call was refused: %v", err)
		}
	})
}
