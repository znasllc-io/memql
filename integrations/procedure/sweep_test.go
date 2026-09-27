package procedure

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
)

// sweep_test.go -- the certification ladder's demotion and retirement sweeps
// (epic memql#5408, D14, D16; plan Task 5 step 7).

// sweepWorld holds each owner's learned procedures, paged the way
// learnedProceduresForOwner pages, and applies the ladder writes back.
type sweepWorld struct {
	eng *fakeEngine
	i   *Integration
	mu  sync.Mutex
	// rows are each owner's procedure rows, in page order; pageSize splits
	// them into pages.
	rows     map[string][]map[string]any
	pageSize int
	// readAs records, per owner, the actor each page read ran as.
	readAs map[string][]string
}

func newSweepWorld(t *testing.T, pageSize int) *sweepWorld {
	t.Helper()
	w := &sweepWorld{eng: newFakeEngine(), rows: map[string][]map[string]any{}, pageSize: pageSize, readAs: map[string][]string{}}
	w.eng.answer("usersForSeedSweep", func(recordedCall, string) ([]map[string]any, string) {
		w.mu.Lock()
		defer w.mu.Unlock()
		var out []map[string]any
		for owner := range w.rows {
			out = append(out, map[string]any{"id": owner})
		}
		return out, ""
	})
	w.eng.answer("learnedProceduresForOwner", func(c recordedCall, cursor string) ([]map[string]any, string) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.readAs[c.Actor] = append(w.readAs[c.Actor], cursor)
		rows := w.rows[c.Actor]
		start := 0
		if cursor != "" {
			for n := range rows {
				if "after-"+str(rows[n], "id") == cursor {
					start = n + 1
				}
			}
		}
		end := min(start+w.pageSize, len(rows))
		page := make([]map[string]any, 0, end-start)
		for _, r := range rows[start:end] {
			page = append(page, copyRow(t, r))
		}
		next := ""
		if end < len(rows) && end > start {
			next = "after-" + str(rows[end-1], "id")
		}
		return page, next
	})
	// The construct read FRESH inside the ladder lock, by id, as its owner.
	w.eng.answer("authoringConstructById", func(c recordedCall, _ string) ([]map[string]any, string) {
		id, _ := parseCallArgs(t, c.Query)["constructId"].(string)
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, r := range w.rows[c.Actor] {
			if str(r, "id") == id {
				return []map[string]any{copyRow(t, r)}, ""
			}
		}
		return nil, ""
	})
	w.eng.onWrite("recordConstructLadder", func(c recordedCall) {
		args := parseCallArgs(t, c.Query)
		w.mu.Lock()
		defer w.mu.Unlock()
		for _, r := range w.rows[c.Actor] {
			if str(r, "id") == args["constructId"] {
				for k, v := range args {
					if k != "constructId" {
						r[k] = v
					}
				}
			}
		}
	})
	w.i = newTestIntegration(w.eng)
	return w
}

// procedureOn is a learned procedure row on a rung.
func procedureOn(id, rung string, fields map[string]any) map[string]any {
	row := map[string]any{
		"id": id, "name": "learnedProcedure_" + id, "ladder": rung, "createdAt": testNow.Add(-90 * 24 * time.Hour).Format(timeLayout),
		"lastReplayAt": testNow.Add(-24 * time.Hour).Format(timeLayout), "shadowMatches": float64(0), "canaryMatches": float64(0),
		"failures": float64(0), "insufficient": float64(0), "distinctBindings": map[string]any{}, "promotionApprovalId": "",
	}
	for k, v := range fields {
		row[k] = v
	}
	return row
}

func (w *sweepWorld) row(owner, id string) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range w.rows[owner] {
		if str(r, "id") == id {
			return r
		}
	}
	return nil
}

func sweepAsCluster(t *testing.T, w *sweepWorld, sweep string) SweepResult {
	t.Helper()
	res, err := w.i.Sweep(maintenanceCtx("demoteProcedures"), sweep, testNow)
	if err != nil {
		t.Fatalf("Sweep(%s): %v", sweep, err)
	}
	return res
}

// TestTheDemotionSweepDemotesStoredEvidence (D16): a trusted procedure whose
// STORED failures already reach the current policy's threshold -- evidence
// written before the threshold tightened -- stops serving without waiting for
// its next replay. A procedure within the policy is not written at all, and a
// candidate is not the demotion sweep's.
func TestTheDemotionSweepDemotesStoredEvidence(t *testing.T) {
	w := newSweepWorld(t, 100)
	w.rows[testOwner] = []map[string]any{
		procedureOn("over", "trusted", map[string]any{"failures": float64(3)}),
		procedureOn("clean", "trusted", nil),
		procedureOn("fresh", "candidate", nil),
		procedureOn("unused", "trusted", map[string]any{"lastReplayAt": testNow.Add(-400 * 24 * time.Hour).Format(timeLayout)}),
	}
	res := sweepAsCluster(t, w, SweepDemotion)
	if res.Demoted != 1 || res.Retired != 0 || len(res.Changed) != 1 || res.Changed[0] != "over" {
		t.Fatalf("result = %+v, want exactly the one over its threshold demoted", res)
	}
	if got := w.row(testOwner, "over")["ladder"]; got != "shadow" {
		t.Fatalf("over is %v, want shadow", got)
	}
	if got := w.row(testOwner, "unused")["ladder"]; got != "trusted" {
		t.Fatalf("the DEMOTION sweep retired %v: disuse is the retirement sweep's", got)
	}
	if n := len(w.eng.callsTo("recordConstructLadder")); n != 1 {
		t.Fatalf("%d ladder writes, want only the changed row", n)
	}
}

// TestTheRetirementSweepRetiresAnUnusedProcedure (D14): a procedure unused
// for longer than the window retires, on any rung; one used within it, or
// already retired, is not written.
func TestTheRetirementSweepRetiresAnUnusedProcedure(t *testing.T) {
	w := newSweepWorld(t, 100)
	old := testNow.Add(-40 * 24 * time.Hour).Format(timeLayout)
	w.rows[testOwner] = []map[string]any{
		procedureOn("stale", "trusted", map[string]any{"lastReplayAt": old}),
		procedureOn("stale-shadow", "shadow", map[string]any{"lastReplayAt": old, "promotionApprovalId": "v1:work:approval:open"}),
		procedureOn("recent", "trusted", nil),
		procedureOn("gone", "retired", map[string]any{"lastReplayAt": old}),
		// Never replayed: its creation is its last use, and it was lifted
		// recently.
		procedureOn("new", "shadow", map[string]any{"lastReplayAt": "", "createdAt": testNow.Add(-2 * 24 * time.Hour).Format(timeLayout)}),
	}
	res := sweepAsCluster(t, w, SweepRetirement)
	if res.Retired != 2 {
		t.Fatalf("result = %+v, want the two stale procedures retired", res)
	}
	for id, want := range map[string]string{"stale": "retired", "stale-shadow": "retired", "recent": "trusted", "gone": "retired", "new": "shadow"} {
		if got := w.row(testOwner, id)["ladder"]; got != want {
			t.Errorf("%s is %v, want %s", id, got, want)
		}
	}
	if got := w.row(testOwner, "stale-shadow")["promotionApprovalId"]; got != "" {
		t.Errorf("a retired procedure still points at an open approval %v", got)
	}
}

// TestASweepReadsEveryOwnerUnderTheirOwnActor: a construct has NO
// cluster-owner arm, so every owner's procedures are read -- every page of
// them -- and written as that owner. The owner list is the one read made as
// the cluster.
func TestASweepReadsEveryOwnerUnderTheirOwnActor(t *testing.T) {
	w := newSweepWorld(t, 2)
	const bob = "v1:identity:user:bob"
	w.rows[testOwner] = []map[string]any{
		procedureOn("a1", "trusted", nil), procedureOn("a2", "trusted", nil),
		procedureOn("a3", "canary", map[string]any{"failures": float64(2)}),
	}
	w.rows[bob] = []map[string]any{procedureOn("b1", "trusted", map[string]any{"failures": float64(5)})}

	res := sweepAsCluster(t, w, SweepDemotion)
	if res.Owners != 2 || res.Procedures != 4 || res.Demoted != 2 {
		t.Fatalf("result = %+v, want both owners, all four procedures, two demoted", res)
	}
	if pages := w.readAs[testOwner]; len(pages) != 2 || pages[0] != "" || pages[1] != "after-a2" {
		t.Fatalf("alice's procedures were read in pages %q, want both pages", pages)
	}
	if len(w.readAs[bob]) != 1 {
		t.Fatalf("bob's procedures were read %d times as bob", len(w.readAs[bob]))
	}
	for actor := range w.readAs {
		if actor != testOwner && actor != bob {
			t.Fatalf("procedures were read as %q -- a construct is readable by its owner alone", actor)
		}
	}
	list := w.eng.callTo(t, "usersForSeedSweep")
	if !list.Internal || !list.Synthetic {
		t.Fatalf("the owner list was read internal=%v synthetic=%v", list.Internal, list.Synthetic)
	}
	for _, c := range w.eng.callsTo("recordConstructLadder") {
		if !c.Internal || (c.Actor != testOwner && c.Actor != bob) {
			t.Fatalf("a sweep write ran internal=%v as %q", c.Internal, c.Actor)
		}
	}
	if w.row(testOwner, "a3")["ladder"] != "shadow" || w.row(bob, "b1")["ladder"] != "shadow" {
		t.Fatal("a procedure over its threshold on the SECOND page, or another owner's, was not demoted")
	}
}

// TestAPersonCannotRunTheSweep: the sweeps span every owner, so only the
// cluster's maintenance principal -- or trusted server-side Go -- runs them.
// A person, a cluster owner included, and an ordinary automation's reader are
// refused before anything is read.
func TestAPersonCannotRunTheSweep(t *testing.T) {
	w := newSweepWorld(t, 100)
	w.rows[testOwner] = []map[string]any{procedureOn("over", "trusted", map[string]any{"failures": float64(3)})}
	owner := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "v1:identity:user:root", Role: auth.RoleOwner})
	reader := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:automation:x", Role: auth.RoleReader, Synthetic: true})
	for name, ctx := range map[string]context.Context{"a person": personCtx(testOwner), "a cluster owner": owner, "an automation's reader": reader} {
		if _, err := w.i.Sweep(ctx, SweepDemotion, testNow); err == nil || !strings.Contains(err.Error(), "maintenance principal") {
			t.Fatalf("%s ran the sweep: %v", name, err)
		}
	}
	if len(w.eng.recorded()) != 0 {
		t.Fatalf("a refused sweep reached the engine: %s", w.eng.summary())
	}
	if _, err := w.i.Sweep(maintenanceCtx("retireProcedures"), "everything", testNow); err == nil {
		t.Fatal("an unknown sweep ran")
	}
	// Trusted server-side Go under internal origin may run it.
	if _, err := w.i.Sweep(auth.ContextWithInternalOrigin(context.Background()), SweepDemotion, testNow); err != nil {
		t.Fatalf("internal origin was refused: %v", err)
	}
	if w.row(testOwner, "over")["ladder"] != "shadow" {
		t.Fatal("the internal sweep did not demote")
	}
}

// TestTheSweepDecidesOnTheConstructAsItIsNow (review finding C1): the page
// row says a trusted procedure is over its failure threshold, and by the time
// the sweep reaches it a clean replay has cleared the failures. The page only
// NOMINATES; the demotion is decided again from the construct read inside the
// ladder lock, so a procedure that is fine now is not demoted on what it was.
func TestTheSweepDecidesOnTheConstructAsItIsNow(t *testing.T) {
	w := newSweepWorld(t, 100)
	w.rows[testOwner] = []map[string]any{procedureOn("healed", "trusted", map[string]any{"failures": float64(3)})}
	page := w.eng.dynamic["learnedProceduresForOwner"]
	w.eng.answerSome("learnedProceduresForOwner", func(c recordedCall, cursor string) ([]map[string]any, string, bool) {
		rows, next, handled := page(c, cursor)
		// A clean replay lands after the page was read.
		w.mu.Lock()
		w.rows[testOwner][0]["failures"] = float64(0)
		w.mu.Unlock()
		return rows, next, handled
	})
	res := sweepAsCluster(t, w, SweepDemotion)
	if res.Demoted != 0 || len(res.Changed) != 0 {
		t.Fatalf("result = %+v: the sweep demoted a procedure on the failures it no longer has", res)
	}
	if got := w.row(testOwner, "healed")["ladder"]; got != "trusted" {
		t.Fatalf("healed is %v, want still trusted", got)
	}
	if n := len(w.eng.callsTo("authoringConstructById")); n != 1 {
		t.Fatalf("the nominated procedure was re-read %d times, want once, inside its lock", n)
	}
}
