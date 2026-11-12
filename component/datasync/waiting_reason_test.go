package datasync

import (
	"context"
	"strings"
	"testing"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// waiting_reason_test.go -- "Backfill now" on a domain that waits says why.
//
// A busy answer leaves the backfill queued or running with no error, and the
// operator who pressed the button is told it is waiting. It used to be told
// one reason for every such answer -- "behind another export of this origin"
// -- which is not why a store that does not ingest waits. The reply's detail
// is now the reason the connector's answer carries, and the old sentence only
// when it carries none.

// THE RESULT CARRIES THE REASON, so whoever asked can say it.
func TestAWaitingBackfillCarriesTheConnectorsReason(t *testing.T) {
	const why = `store "acme" does not ingest now, so its mirror waits for it`
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"the connector said why":   {memqlsync.Busy("shopify", why), why},
		"the connector gave none":  {memqlsync.Busy("shopify", ""), ""},
		"the origin's own message": {memqlsync.Busy("shopify", "A bulk query operation for this app and shop is already in progress."), "A bulk query operation for this app and shop is already in progress."},
	} {
		t.Run(name, func(t *testing.T) {
			c := mirrorConnector()
			c.backfillErr = tc.err
			r := stateRunner(newStateEngine(), c)
			res, err := r.StartBackfill(context.Background(), "shopify", testMirrorConcept, "")
			if err != nil || !res.Waiting {
				t.Fatalf("StartBackfill = %+v, %v; want waiting and no error", res, err)
			}
			if res.WaitingFor != tc.want {
				t.Errorf("WaitingFor = %q, want %q", res.WaitingFor, tc.want)
			}
		})
	}
}

// A connector with partitions asked for none: each partition that waits may
// say something else, and the result says each reason once.
func TestAFanOutThatWaitsSaysEachReasonOnce(t *testing.T) {
	c := newFakePartitioned("a", "b", "c")
	c.partErrs = map[string]error{
		"a": memqlsync.Busy("shopify", `store "a" does not ingest now, so its mirror waits for it`),
		"b": memqlsync.Busy("shopify", "another export is running"),
		"c": memqlsync.Busy("shopify", "another export is running"),
	}
	r := stateRunner(newStateEngine(), c)
	res, err := r.StartBackfill(context.Background(), c.Name(), testMirrorConcept, "")
	if err != nil || !res.Waiting {
		t.Fatalf("StartBackfill = %+v, %v; want waiting and no error", res, err)
	}
	if want := `store "a" does not ingest now, so its mirror waits for it; another export is running`; res.WaitingFor != want {
		t.Errorf("WaitingFor = %q, want each reason once: %q", res.WaitingFor, want)
	}
}

// THE OPERATOR'S REPLY SAYS THE CONNECTOR'S REASON, and the sentence it always
// said only when the connector gave none.
func TestTheWaitingReplySaysTheConnectorsOwnReason(t *testing.T) {
	const paused = `store "acme" does not ingest now, so its mirror waits for it`
	for name, tc := range map[string]struct {
		err     error
		want    string
		wantNot string
	}{
		"a store that does not ingest": {memqlsync.Busy("shopify", paused), paused, "another export"},
		"no reason given":              {memqlsync.Busy("shopify", ""), "the backfill is waiting behind another export of this origin", "does not ingest"},
	} {
		t.Run(name, func(t *testing.T) {
			engine := newStateEngine()
			c := mirrorConnector()
			c.backfillErr = tc.err
			i := &Integration{runner: stateRunner(engine, c), logger: discardLogger()}
			nodes, err := i.handleStartBackfill(clusterOwnerCtx(), map[string]any{"connector": "shopify", "conceptId": testMirrorConcept}, 0)
			if err != nil || len(nodes) != 1 {
				t.Fatalf("handleStartBackfill = %v, %v", nodes, err)
			}
			reply := string(nodes[0].Payload)
			if !strings.Contains(reply, `"outcome":"waiting"`) {
				t.Fatalf("reply %s, want the outcome waiting", reply)
			}
			if !strings.Contains(reply, strings.ReplaceAll(tc.want, `"`, `\"`)) || strings.Contains(reply, tc.wantNot) {
				t.Errorf("reply %s; want it to say %q and not %q", reply, tc.want, tc.wantNot)
			}
			if st := keptState(t, engine, testMirrorConcept, ""); st.BackfillStatus != "queued" || st.LastError != "" {
				t.Errorf("row = %s, lastError %q; want queued with no error, as before", st.BackfillStatus, st.LastError)
			}
		})
	}
}
