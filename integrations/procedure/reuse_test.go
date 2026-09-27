package procedure

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/work"
)

// reuse_test.go -- the reuse label: decided by evidence in the sweep,
// overridden by a person as a version (epic memql#5414, #5418; design D24).

const (
	reuseOwner = testOwner
	otherOwner = "v1:identity:user:bob"
)

// reuseWorld holds each owner's constructs, runs, automation steps and goals,
// answers every read under that owner's actor only (the composite tier's
// answer to anybody else: zero rows and no error), and applies the two reuse
// writes back to the stored rows.
type reuseWorld struct {
	eng *fakeEngine
	i   *Integration
	mu  sync.Mutex
	// constructs and procedures are each owner's catalogued automations and
	// learned procedures; runs, steps and goals their work.
	constructs, procedures, runs, steps, goals map[string][]map[string]any
	// policy answers feedbackPolicyCurrent when set.
	policy map[string]any
}

func newReuseWorld(t *testing.T) *reuseWorld {
	t.Helper()
	w := &reuseWorld{
		eng:        newFakeEngine(),
		constructs: map[string][]map[string]any{}, procedures: map[string][]map[string]any{},
		runs: map[string][]map[string]any{}, steps: map[string][]map[string]any{}, goals: map[string][]map[string]any{},
	}
	w.eng.answer("usersForSeedSweep", func(recordedCall, string) ([]map[string]any, string) {
		return []map[string]any{{"id": reuseOwner}, {"id": otherOwner}}, ""
	})
	owned := func(name string, rows func() map[string][]map[string]any) {
		w.eng.answer(name, func(c recordedCall, _ string) ([]map[string]any, string) {
			w.mu.Lock()
			defer w.mu.Unlock()
			var out []map[string]any
			for _, r := range rows()[c.Actor] {
				out = append(out, copyRow(t, r))
			}
			return out, ""
		})
	}
	owned("cataloguedConstructsForOwner", func() map[string][]map[string]any { return w.constructs })
	owned("learnedProceduresForOwner", func() map[string][]map[string]any { return w.procedures })
	owned("workSignedRunsForOwner", func() map[string][]map[string]any { return w.runs })
	owned("workAutomationStepsForOwner", func() map[string][]map[string]any { return w.steps })
	owned("workGoalsForOwner", func() map[string][]map[string]any { return w.goals })
	w.eng.answer("authoringConstructById", func(c recordedCall, _ string) ([]map[string]any, string) {
		args := parseCallArgs(t, c.Query)
		if r := w.construct(c.Actor, str(args, "constructId")); r != nil {
			return []map[string]any{copyRow(t, r)}, ""
		}
		return nil, ""
	})
	w.eng.answer("feedbackPolicyCurrent", func(recordedCall, string) ([]map[string]any, string) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.policy == nil {
			return nil, ""
		}
		return []map[string]any{copyRow(t, w.policy)}, ""
	})
	apply := func(c recordedCall) {
		args := parseCallArgs(t, c.Query)
		r := w.construct(c.Actor, str(args, "constructId"))
		if r == nil {
			t.Errorf("%s wrote a construct %v that %s does not own", c.Name(), args["constructId"], c.Actor)
			return
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		for k, v := range args {
			if k != "constructId" {
				r[k] = v
			}
		}
	}
	w.eng.onWrite("recordConstructReuse", apply)
	w.eng.onWrite("recordConstructReuseOverride", apply)
	w.i = newTestIntegration(w.eng)
	return w
}

func (w *reuseWorld) construct(owner, id string) map[string]any {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, set := range []map[string][]map[string]any{w.constructs, w.procedures} {
		for _, r := range set[owner] {
			if str(r, "id") == id {
				return r
			}
		}
	}
	return nil
}

func authoredAutomation(id, name string) map[string]any {
	return map[string]any{
		"id": id, "name": name, "kind": "automation", "status": "active", "catalogued": true,
		"ownerUserId": reuseOwner,
	}
}

func signedRun(id, sig, goalId string, fields map[string]any) map[string]any {
	row := map[string]any{
		"id": id, "status": "succeeded", "goalSignature": sig, "goalId": goalId,
		"automationName": "workRun_" + id, "ownerUserId": reuseOwner,
	}
	for k, v := range fields {
		row[k] = v
	}
	return row
}

func automationStep(runId, name string) map[string]any {
	return map[string]any{
		"id": "v1:work:step:" + runId + "-" + name, "runId": runId, "stepType": "automation", "status": "done",
		"call": map[string]any{"construct": "automation", "name": name},
	}
}

func (w *reuseWorld) sweep(t *testing.T, dryRun bool) ReuseSweepResult {
	t.Helper()
	res, err := w.i.ReuseSweep(maintenanceCtx("sweepConstructReuse"), dryRun)
	if err != nil {
		t.Fatalf("ReuseSweep: %v", err)
	}
	return res
}

// seedTwoGoals gives the owner an authored automation used by two different
// goals -- once as a goal's compiled template, once as a section of another
// goal -- and a learned procedure served by one goal's replay, plus the
// procedure's own replay run, which served no goal and must not count.
func (w *reuseWorld) seedTwoGoals() {
	w.constructs[reuseOwner] = []map[string]any{authoredAutomation("v1:authoring:construct:summarise", "summariseTickets")}
	w.procedures[reuseOwner] = []map[string]any{{
		"id": "v1:authoring:construct:learned", "name": "learnedProcedure_abc", "kind": "automation", "status": "active",
		"ladder": "trusted",
	}}
	w.runs[reuseOwner] = []map[string]any{
		signedRun("v1:work:run:r1", "sig-tickets", "v1:work:goal:g1", map[string]any{"templateConstructId": "v1:authoring:construct:summarise"}),
		signedRun("v1:work:run:r2", "sig-weekly", "v1:work:goal:g2", nil),
		signedRun("v1:work:run:r3", "sig-tickets", "v1:work:goal:g1", map[string]any{
			"automationName": replayAutomationName, "variables": map[string]any{"procedureConstructId": "v1:authoring:construct:learned"},
		}),
		signedRun("v1:work:run:r4", "sig-tickets", "v1:work:goal:g1", map[string]any{
			"templateConstructId": "v1:authoring:construct:learned", "triggeredBy": "procedure:shadow",
		}),
	}
	// The section call: an automation step of goal g2's run, its runId in
	// the canonical spelling an adopted goal run's steps carry.
	w.steps[reuseOwner] = []map[string]any{automationStep("v1:work:run:r2", "summariseTickets")}
	w.goals[reuseOwner] = []map[string]any{
		{"id": "v1:work:goal:g1", "accountIds": []any{"v1:accounts:account:acme"}},
		{"id": "v1:work:goal:g2", "accountIds": []any{}},
	}
}

// TestTwoGoalSignaturesMakeAConstructReusable (#5418 acceptance, through the
// sweep): the authored automation served two distinct goal signatures -- one
// as a compiled template, one as another goal's section -- so the evidence
// makes it reusable. The learned procedure served one goal through
// replayLearnedProcedure (its own replay run is not a use), tied to one
// account, so it is account-specific. Every write runs as the owner under
// internal origin, and the evidence is stored with its counts.
func TestTwoGoalSignaturesMakeAConstructReusable(t *testing.T) {
	w := newReuseWorld(t)
	w.seedTwoGoals()

	res := w.sweep(t, false)
	if res.Owners != 2 || res.Constructs != 2 || len(res.Changed) != 2 || res.Errors != 0 {
		t.Fatalf("result = %+v", res)
	}
	authored := w.construct(reuseOwner, "v1:authoring:construct:summarise")
	if authored["reuse"] != "reusable" {
		t.Fatalf("the automation two goals used is %v, want reusable", authored["reuse"])
	}
	ev := obj(authored, "reuseEvidence")
	if !reflect.DeepEqual(ev["goalSignatures"], []any{"sig-tickets", "sig-weekly"}) || ev["signatureCount"] != float64(2) ||
		ev["uses"] != float64(2) || !reflect.DeepEqual(ev["accountIds"], []any{"v1:accounts:account:acme"}) ||
		ev["decidedAt"] != testNow.Format(timeLayout) {
		t.Fatalf("evidence = %v", ev)
	}
	learned := w.construct(reuseOwner, "v1:authoring:construct:learned")
	if learned["reuse"] != "accountSpecific" || intOf(obj(learned, "reuseEvidence"), "uses") != 1 {
		t.Fatalf("the procedure one goal's replay served is %v with %v, want accountSpecific from one use",
			learned["reuse"], learned["reuseEvidence"])
	}
	writes := w.eng.callsTo("recordConstructReuse")
	if len(writes) != 2 {
		t.Fatalf("%d label writes, want 2", len(writes))
	}
	for _, c := range writes {
		if !c.Internal || c.Actor != reuseOwner {
			t.Errorf("a label write ran as %q (internal %v), want the owner under internal origin", c.Actor, c.Internal)
		}
	}
	for _, name := range []string{"workSignedRunsForOwner", "workAutomationStepsForOwner", "workGoalsForOwner", "cataloguedConstructsForOwner"} {
		for _, c := range w.eng.callsTo(name) {
			if c.Internal || (c.Actor != reuseOwner && c.Actor != otherOwner) {
				t.Errorf("%s ran as %q (internal %v), want each owner's own read", name, c.Actor, c.Internal)
			}
		}
	}

	// The control: without the section call it served one goal, tied to one
	// account -- not reusable.
	one := newReuseWorld(t)
	one.seedTwoGoals()
	one.steps[reuseOwner] = nil
	one.sweep(t, false)
	if got := one.construct(reuseOwner, "v1:authoring:construct:summarise")["reuse"]; got != "accountSpecific" {
		t.Fatalf("one goal (tied to one account) must not make it reusable, got %v", got)
	}
}

// TestTheSweepThresholdIsTheFeedbackPolicysValue: the threshold is the
// seeded row's value, not a constant.
func TestTheSweepThresholdIsTheFeedbackPolicysValue(t *testing.T) {
	w := newReuseWorld(t)
	w.seedTwoGoals()
	w.policy = map[string]any{"validateAnswers": true, "reusableAfterSignatures": float64(3)}
	w.sweep(t, false)
	if got := w.construct(reuseOwner, "v1:authoring:construct:summarise")["reuse"]; got == "reusable" {
		t.Fatalf("two signatures under a threshold of three must not be reusable, got %v", got)
	}
}

// TestTheSweepWritesNothingWhenNothingChanged: a second sweep over the same
// work finds every label and every count where the first left them -- only
// the time it looked differs, and that is not a change -- so it writes
// nothing. A dry run reports what would change and writes nothing either.
func TestTheSweepWritesNothingWhenNothingChanged(t *testing.T) {
	w := newReuseWorld(t)
	w.seedTwoGoals()

	dry := w.sweep(t, true)
	if len(dry.Changed) != 2 || !dry.DryRun || len(w.eng.writes()) != 0 {
		t.Fatalf("a dry run reported %v and wrote %d", dry.Changed, len(w.eng.writes()))
	}
	w.sweep(t, false)
	first := len(w.eng.writes())
	w.i.SetNow(func() time.Time { return testNow.Add(6 * time.Hour) })
	again := w.sweep(t, false)
	if len(again.Changed) != 0 || len(w.eng.writes()) != first {
		t.Fatalf("an unchanged sweep reported %v and wrote %d more", again.Changed, len(w.eng.writes())-first)
	}

	// The control: one more use of the procedure changes its evidence.
	w.mu.Lock()
	w.runs[reuseOwner] = append(w.runs[reuseOwner], signedRun("v1:work:run:r5", "sig-tickets", "v1:work:goal:g1", map[string]any{
		"automationName": replayAutomationName, "variables": map[string]any{"procedureConstructId": "learned"},
	}))
	w.mu.Unlock()
	changed := w.sweep(t, false)
	if len(changed.Changed) != 1 || changed.Changed[0] != "v1:authoring:construct:learned" {
		t.Fatalf("a new use (named by the bare id) must change exactly the procedure's evidence, got %v", changed.Changed)
	}
	if got := intOf(obj(w.construct(reuseOwner, "v1:authoring:construct:learned"), "reuseEvidence"), "uses"); got != 2 {
		t.Fatalf("uses = %d, want 2", got)
	}
}

// TestTheSweepRefusesAnyCallerButTheMaintenancePrincipal: the sweep reads
// every owner's work, so a person -- a cluster owner included -- and an
// ordinary automation's synthetic reader are refused before anything is
// read. The maintenance principal and trusted server-side Go under internal
// origin run it.
func TestTheSweepRefusesAnyCallerButTheMaintenancePrincipal(t *testing.T) {
	refused := map[string]context.Context{
		"a person":        personCtx(reuseOwner),
		"a cluster owner": auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "v1:identity:user:root", Role: auth.RoleOwner}),
		"an automation's reader": auth.ContextWithAccess(context.Background(), &auth.AccessContext{
			UserId: "system:automation:somethingElse", Role: auth.RoleReader, Synthetic: true,
		}),
		"nobody": context.Background(),
	}
	for name, ctx := range refused {
		t.Run(name, func(t *testing.T) {
			w := newReuseWorld(t)
			w.seedTwoGoals()
			if _, err := w.i.handleReuseSweep(ctx, map[string]any{"dryRun": false}, 0); err == nil ||
				!strings.Contains(err.Error(), "maintenance principal") {
				t.Fatalf("err = %v, want the sweep refused", err)
			}
			if n := len(w.eng.recorded()); n != 0 {
				t.Fatalf("a refused sweep made %d calls (%s)", n, w.eng.summary())
			}
		})
	}
	for name, ctx := range map[string]context.Context{
		"the maintenance principal": maintenanceCtx("sweepConstructReuse"),
		"internal origin":           auth.ContextWithInternalOrigin(context.Background()),
	} {
		t.Run(name, func(t *testing.T) {
			w := newReuseWorld(t)
			w.seedTwoGoals()
			nodes, err := w.i.handleReuseSweep(ctx, map[string]any{"dryRun": true}, 0)
			if err != nil || len(nodes) != 1 {
				t.Fatalf("%s must run the sweep: %v", name, err)
			}
			reply := decodeReply(t, nodes)
			if reply["owners"] != float64(2) || reply["dryRun"] != true || len(reply["changed"].([]any)) != 2 {
				t.Fatalf("reply = %v", reply)
			}
		})
	}
}

// TestTheSweepNeverTouchesAnOverride: a person labelled the construct
// themselves. The evidence changes and is written -- the label a person sees
// stays theirs -- and the override is never an argument of the sweep's write.
func TestTheSweepNeverTouchesAnOverride(t *testing.T) {
	w := newReuseWorld(t)
	w.seedTwoGoals()
	override := map[string]any{"label": "goalSpecific", "by": reuseOwner, "at": testNow.Format(timeLayout), "version": float64(3)}
	w.constructs[reuseOwner][0]["reuseOverride"] = override
	w.sweep(t, false)

	for _, c := range w.eng.writes() {
		if c.Name() == "recordConstructReuseOverride" {
			t.Fatalf("the sweep wrote an override: %s", c.Query)
		}
		if _, present := argsOf(t, c)["reuseOverride"]; present {
			t.Fatalf("the sweep's write carries the override: %s", c.Query)
		}
	}
	row := w.construct(reuseOwner, "v1:authoring:construct:summarise")
	if row["reuse"] != "reusable" || !reflect.DeepEqual(row["reuseOverride"], override) {
		t.Fatalf("row = reuse %v, override %v; want the evidence written and the override as it was", row["reuse"], row["reuseOverride"])
	}
	if got := work.EffectiveReuse(work.ParseReuseLabel(str(row, "reuse")), work.ParseReuseLabel(str(obj(row, "reuseOverride"), "label"))); got != work.ReuseGoalSpecific {
		t.Fatalf("the label a person sees is %q, want theirs", got)
	}
}

// TestAnOverrideIsAVersionAndTheEvidenceKeepsCounting (#5418 acceptance): two
// overrides are versions 1 and 2 -- the server's count, by the caller, at the
// clock -- and a sweep afterwards still writes the evidence while the
// override stays version 2. Handing the label back to the evidence is a third
// version whose label is empty. Somebody who does not own the construct is
// refused construct_not_found and writes nothing.
func TestAnOverrideIsAVersionAndTheEvidenceKeepsCounting(t *testing.T) {
	w := newReuseWorld(t)
	w.seedTwoGoals()
	const id = "v1:authoring:construct:summarise"
	set := func(ctx context.Context, label string) (map[string]any, error) {
		nodes, err := w.i.handleSetReuse(ctx, map[string]any{"constructId": id, "label": label}, 0)
		if err != nil {
			return nil, err
		}
		return decodeReply(t, nodes), nil
	}

	first, err := set(personCtx(reuseOwner), "goalSpecific")
	if err != nil {
		t.Fatalf("first override: %v", err)
	}
	second, err := set(personCtx(reuseOwner), "reusable")
	if err != nil {
		t.Fatalf("second override: %v", err)
	}
	if v := obj(first, "override")["version"]; v != float64(1) {
		t.Fatalf("first override version = %v, want 1", v)
	}
	if o := obj(second, "override"); o["version"] != float64(2) || o["label"] != "reusable" || o["by"] != reuseOwner ||
		o["at"] != testNow.Format(timeLayout) || second["reuse"] != "reusable" {
		t.Fatalf("second reply = %v", second)
	}
	for _, c := range w.eng.callsTo("recordConstructReuseOverride") {
		if !c.Internal || c.Actor != reuseOwner {
			t.Errorf("an override write ran as %q (internal %v)", c.Actor, c.Internal)
		}
	}

	w.sweep(t, false)
	row := w.construct(reuseOwner, id)
	if row["reuse"] != "reusable" || intOf(obj(row, "reuseEvidence"), "uses") != 2 {
		t.Fatalf("the evidence must keep counting under the override: %v / %v", row["reuse"], row["reuseEvidence"])
	}
	if o := obj(row, "reuseOverride"); o["version"] != float64(2) || o["label"] != "reusable" {
		t.Fatalf("the sweep moved the override: %v", o)
	}

	back, err := set(personCtx(reuseOwner), "evidence")
	if err != nil {
		t.Fatalf("hand back to the evidence: %v", err)
	}
	if o := obj(back, "override"); o["version"] != float64(3) || o["label"] != "" || back["reuse"] != "reusable" {
		t.Fatalf("evidence reply = %v, want version 3 with an empty label and the evidence's label", back)
	}

	before := len(w.eng.writes())
	if _, err := set(personCtx(otherOwner), "goalSpecific"); err == nil || !strings.HasPrefix(err.Error(), "construct_not_found") {
		t.Fatalf("err = %v, want construct_not_found for somebody else's construct", err)
	}
	if _, err := set(personCtx(reuseOwner), "everyone"); err == nil {
		t.Fatal("a label outside the closed set was accepted")
	}
	if len(w.eng.writes()) != before {
		t.Fatalf("a refused override wrote %d rows", len(w.eng.writes())-before)
	}
}

// TestEveryStatementTheReuseLabelsRenderParses holds the reuse sweep's, the
// override's and the corpus judgment's calls to the real front end and the
// loaded registry, and every @serverOnly one to internal origin -- the class
// of defect a recording fake cannot see.
func TestEveryStatementTheReuseLabelsRenderParses(t *testing.T) {
	w := newReuseWorld(t)
	w.seedTwoGoals()
	w.sweep(t, false)
	if _, err := w.i.handleSetReuse(personCtx(reuseOwner), map[string]any{"constructId": "v1:authoring:construct:summarise", "label": "goalSpecific"}, 0); err != nil {
		t.Fatalf("setReuse: %v", err)
	}
	calls := w.eng.recorded()

	recs := twoRecordings()
	judged := newVersionsEngine()
	seedCorpus(t, judged.fakeEngine, recs...)
	judged.versions[parentOf(recs[0])] = []map[string]any{parentVersion("delegate", 1, recs[0].runId, true)}
	if _, err := newTestIntegration(judged).loadCorpus(context.Background(), corpusKeyFor()); err != nil {
		t.Fatalf("loadCorpus: %v", err)
	}
	calls = append(calls, judged.recorded()...)

	assertStatementsParseAndResolve(t, calls, []string{
		"usersForSeedSweep", "cataloguedConstructsForOwner", "learnedProceduresForOwner", "workSignedRunsForOwner",
		"workAutomationStepsForOwner", "workGoalsForOwner", "feedbackPolicyCurrent", "recordConstructReuse",
		"authoringConstructById", "recordConstructReuseOverride", "workStepVersions", "workObservationsForOwnerRun",
	})
}

// decodeReply reads a capability's one-row reply.
func decodeReply(t *testing.T, nodes []memorynodes.MemoryNode) map[string]any {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("reply has %d rows, want 1", len(nodes))
	}
	var reply map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &reply); err != nil {
		t.Fatalf("reply: %v", err)
	}
	return reply
}
