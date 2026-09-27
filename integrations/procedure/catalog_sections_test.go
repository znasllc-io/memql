package procedure

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/integrations/planner"
)

// catalog_sections_test.go -- a succeeded goal run's live sections become
// catalogued automations (epic memql#5414, #5418; design D24).

const (
	catalogRunId       = "v1:work:run:goal-a"
	catalogTemplateId  = "v1:authoring:construct:tmpl-a"
	catalogDraftBundle = "v1:authoring:bundle:draft-a"
	catalogHeadline    = "workRun_goal_a"
)

// catalogCtx is the catalogSucceededSections automation as the executor
// presents it: its synthetic actor, under the internal origin a tree-loaded
// automation runs with.
func catalogCtx() context.Context {
	return auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(),
		&auth.AccessContext{UserId: catalogAutomationActor, Synthetic: true}))
}

// sectionFx is one section automation of the run's draft, written the way
// compile writes one: its purpose as @description, its inputs and the goal
// context as arguments, its name carrying its signature.
type sectionFx struct {
	label, purpose string
	inputs         []string
	name, source   string
	sig, stepKey   string
}

func newSectionFx(label, purpose string, inputs ...string) sectionFx {
	sig := work.SectionSignature(work.Section{Purpose: purpose, Inputs: inputs})
	name := planner.SectionAutomationPrefix + sig[:8] + "_" + label
	var args strings.Builder
	for _, in := range inputs {
		args.WriteString("    " + in + " any\n")
	}
	source := "use agents.builtins.{ runAgentTurn }\n\n@description(" + langparser.QuoteString(purpose) + ")\n@template\nautomation " + name +
		" {\n  args {\n" + args.String() + "    overallGoal any\n  }\n" +
		"  answer := builtin runAgentTurn(agentId: \"v1:agents:agent:assistant\", prompt: \"Produce the section.\" + toString(args.overallGoal ?? \"not given\"))\n" +
		"  return answer\n}\n"
	return sectionFx{label: label, purpose: purpose, inputs: inputs, name: name, source: source, sig: sig, stepKey: catalogHeadline + "_" + label}
}

func (s sectionFx) row() map[string]any {
	return map[string]any{
		"id": "v1:authoring:construct:" + s.label, "kind": "automation", "name": s.name, "status": "draft",
		"bundleId": catalogDraftBundle, "targetNamespace": "authored", "source": s.source, "ownerUserId": testOwner,
	}
}

// version is the entry workStepVersions answers for the step that called the
// section: its current version, done, nobody's override.
func (s sectionFx) version() map[string]any {
	return map[string]any{
		"stepId": "goal-a-" + s.stepKey, "key": s.stepKey, "version": float64(1), "attempt": float64(1),
		"stepType": "automation", "call": map[string]any{"construct": "automation", "name": s.name},
		"status": "done", "override": map[string]any{}, "current": true,
	}
}

// catalogWorld is one succeeded goal run whose draft wrote two sections as
// automations of their own, read and written through the recording engine.
type catalogWorld struct {
	eng      *versionsEngine
	i        *Integration
	gate     *passingGate
	run      map[string]any
	sections []sectionFx
}

func newCatalogWorld(t *testing.T) *catalogWorld {
	t.Helper()
	w := &catalogWorld{
		eng: newVersionsEngine(),
		run: map[string]any{
			"id": catalogRunId, "ownerUserId": testOwner, "status": "succeeded", "goalId": "v1:work:goal:a",
			"goalSignature": "sha256:goal-a", "templateConstructId": catalogTemplateId, "staleSteps": []any{},
		},
		sections: []sectionFx{
			newSectionFx("summary", "summarise the month's invoices", "month"),
			newSectionFx("count", "count the invoice lines", "invoiceSummary"),
		},
	}
	w.eng.reply("workRunForOwner", w.run)
	// The owned read: a hint that does not own the run reads nothing.
	w.eng.ownedRead("workRunForOwner", testOwner)
	w.eng.reply("authoringConstructById", map[string]any{
		"id": catalogTemplateId, "kind": "automation", "name": catalogHeadline, "status": "draft",
		"bundleId": catalogDraftBundle, "source": "@template\nautomation " + catalogHeadline + " {\n  return 1\n}\n",
	})
	members := []map[string]any{{"id": catalogTemplateId, "kind": "automation", "name": catalogHeadline, "status": "draft", "bundleId": catalogDraftBundle}}
	var versions []map[string]any
	for _, s := range w.sections {
		members = append(members, s.row())
		versions = append(versions, s.version())
	}
	w.eng.reply("authoringConstructsForBundle", members...)
	w.eng.versions[catalogRunId] = versions
	w.i = newTestIntegration(w.eng)
	w.gate = &passingGate{}
	w.i.SetCompiler(w.gate)
	return w
}

func (w *catalogWorld) catalogue(t *testing.T) map[string]any {
	t.Helper()
	nodes, err := w.i.handleCatalogSections(catalogCtx(), map[string]any{"runId": catalogRunId, "ownerUserId": testOwner}, 0)
	if err != nil {
		t.Fatalf("catalogSections: %v", err)
	}
	return decodeReply(t, nodes)
}

// callsNamed is every recorded call to one construct, in order.
func (w *catalogWorld) callsNamed(name string) []recordedCall { return w.eng.callsTo(name) }

// TestASucceededRunCataloguesItsSections: each section automation of the run's
// draft is written into its owner's catalog as a construct of its own -- a
// bundle holding it alone, active, catalogued under "section:<signature>" with
// the purpose as its match text and the draft as its provenance -- and given
// the SECTION'S SIGNATURE, which it computes from the automation's own purpose
// and declared inputs, last. Every write runs as the owner under internal
// origin. Run again, the same run catalogues nothing: the owner's catalog
// already holds both sections.
func TestASucceededRunCataloguesItsSections(t *testing.T) {
	w := newCatalogWorld(t)
	reply := w.catalogue(t)
	catalogued, _ := reply["catalogued"].([]any)
	if len(catalogued) != 2 || reply["skipped"] != nil || reply["notCatalogued"] != nil {
		t.Fatalf("reply = %v, want both sections catalogued", reply)
	}

	// Each write names the construct it is about; the construct is found by
	// the section automation it copies.
	byConstruct := func(name string) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, c := range w.callsNamed(name) {
			args := argsOf(t, c)
			out[str(args, "constructId")] = args
		}
		return out
	}
	created, catalogues, signatures, statuses := byConstruct("createAuthoringConstruct"), byConstruct("catalogueConstruct"),
		byConstruct("recordConstructGoalSignature"), byConstruct("setConstructStatus")
	if len(created) != 2 || len(catalogues) != 2 || len(signatures) != 2 || len(statuses) != 2 {
		t.Fatalf("want two constructs created, catalogued, signed and activated: %s", w.eng.summary())
	}
	for _, s := range w.sections {
		constructId := ""
		for id, args := range created {
			if args["name"] == s.name {
				constructId = id
				if args["kind"] != "automation" || args["source"] != s.source {
					t.Errorf("the catalogued construct is %v, want a copy of %s", args, s.name)
				}
			}
		}
		if constructId == "" {
			t.Fatalf("no catalogued construct copies %s", s.name)
		}
		if got := str(signatures[constructId], "goalSignature"); got != s.sig {
			t.Errorf("%s was signed %q, want the section's signature %q", s.name, got, s.sig)
		}
		args := catalogues[constructId]
		if args["catalogKey"] != "section:"+s.sig || args["catalogMatchText"] != "kind:automation intent:"+s.purpose || args["fromBundleId"] != catalogDraftBundle {
			t.Errorf("%s catalogued as %v", s.name, args)
		}
		if statuses[constructId]["status"] != "active" {
			t.Errorf("%s was set %v, want active: the section tiers serve only an active construct", s.name, statuses[constructId])
		}
	}

	// The signature is the LAST write of each section, and every write ran as
	// the owner under internal origin.
	var names []string
	for _, c := range w.eng.writes() {
		names = append(names, c.Name())
		if !c.Internal || c.Actor != testOwner || c.Synthetic {
			t.Errorf("%s ran as %q internal=%v synthetic=%v, want the owner under internal origin", c.Name(), c.Actor, c.Internal, c.Synthetic)
		}
	}
	want := "createAuthoringBundle,createAuthoringConstruct,recordBundleValidation,setConstructStatus,catalogueConstruct,recordConstructGoalSignature"
	if got := strings.Join(names, ","); got != want+","+want {
		t.Fatalf("writes = %s\nwant each section as %s", got, want)
	}
	for _, c := range w.eng.recorded() {
		if !c.IsWrite() && c.Actor != testOwner {
			t.Errorf("the read %s ran as %q, want the owner", c.Name(), c.Actor)
		}
	}
	if len(w.gate.compiled) != 2 {
		t.Errorf("Gate 1 compiled %d constructs, want each section on its own", len(w.gate.compiled))
	}

	// Idempotent: with the owner's catalog holding both, nothing is written.
	again := newCatalogWorld(t)
	var held []map[string]any
	for n, s := range again.sections {
		held = append(held, map[string]any{"id": "v1:authoring:construct:held-" + s.label, "goalSignature": s.sig, "catalogued": true, "n": n})
	}
	again.eng.answer("cataloguedConstructsForGoalSignature", func(c recordedCall, _ string) ([]map[string]any, string) {
		for _, h := range held {
			if strings.Contains(c.Query, str(h, "goalSignature")) {
				return []map[string]any{h}, ""
			}
		}
		return nil, ""
	})
	reply = again.catalogue(t)
	if catalogued, _ := reply["catalogued"].([]any); len(catalogued) != 0 || len(again.eng.writes()) != 0 {
		t.Fatalf("an already-catalogued section wrote %s (reply %v)", again.eng.summary(), reply)
	}
	left, _ := reply["notCatalogued"].([]any)
	if len(left) != 2 || !strings.Contains(str(left[0].(map[string]any), "reason"), "already holds") {
		t.Fatalf("notCatalogued = %v, want both, each already held", left)
	}
}

// TestADislikedOrStaleRunCataloguesNothing: a run its owner disliked, or whose
// head holds steps computed against an upstream version no longer current, is
// not the answer that stands -- nothing of it is catalogued, and the reply
// says why. And one section of an otherwise good run is left out when its
// current version ran with a person's override (the automation does not carry
// it) or its owner disliked its current answer.
func TestADislikedOrStaleRunCataloguesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		prepare func(w *catalogWorld)
		skipped string
	}{
		"a disliked run": {
			prepare: func(w *catalogWorld) {
				w.eng.reply("workObservationsForOwnerRun", map[string]any{"kind": "feedback", "data": map[string]any{"verdict": "dislike"}})
			},
			skipped: "disliked the run",
		},
		"a stale head": {
			prepare: func(w *catalogWorld) { w.run["staleSteps"] = []any{catalogHeadline + "_count"} },
			skipped: "no longer current",
		},
		"a run that did not succeed": {
			prepare: func(w *catalogWorld) { w.run["status"] = "failed" },
			skipped: "has not succeeded",
		},
		"a run with no draft": {
			prepare: func(w *catalogWorld) { delete(w.run, "templateConstructId") },
			skipped: "draft of its own",
		},
		"a replay": {
			prepare: func(w *catalogWorld) { w.run["mode"] = "replay" },
			skipped: "replay",
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newCatalogWorld(t)
			tc.prepare(w)
			reply := w.catalogue(t)
			if !strings.Contains(str(reply, "skipped"), tc.skipped) {
				t.Fatalf("skipped = %q, want it to say %q", reply["skipped"], tc.skipped)
			}
			if len(w.eng.writes()) != 0 {
				t.Fatalf("a run that stands for nothing wrote %s", w.eng.summary())
			}
		})
	}

	for name, tc := range map[string]struct {
		prepare func(w *catalogWorld)
		reason  string
	}{
		"a section re-run with an override": {
			prepare: func(w *catalogWorld) {
				w.eng.versions[catalogRunId][1]["override"] = map[string]any{"prompt": "Count only the paid ones.", "requestedBy": testOwner}
			},
			reason: "override",
		},
		"a section whose current answer was disliked": {
			prepare: func(w *catalogWorld) {
				w.eng.reply("workObservationsForOwnerRun", map[string]any{
					"kind": "feedback", "createdAt": testNow.Format(time.RFC3339Nano),
					"data": map[string]any{"verdict": "dislike", "target": map[string]any{"stepKey": catalogHeadline + "_count", "version": float64(1)}},
				})
			},
			reason: "disliked",
		},
		"a section whose step did not finish": {
			prepare: func(w *catalogWorld) { w.eng.versions[catalogRunId][1]["status"] = "failed" },
			reason:  "did not finish",
		},
		"a section no current version called": {
			prepare: func(w *catalogWorld) { w.eng.versions[catalogRunId][1]["current"] = false },
			reason:  "no step of the run called it",
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newCatalogWorld(t)
			tc.prepare(w)
			reply := w.catalogue(t)
			catalogued, _ := reply["catalogued"].([]any)
			left, _ := reply["notCatalogued"].([]any)
			if len(catalogued) != 1 || len(left) != 1 {
				t.Fatalf("reply = %v, want the summary catalogued and the count left out", reply)
			}
			got := left[0].(map[string]any)
			if got["automation"] != w.sections[1].name || !strings.Contains(str(got, "reason"), tc.reason) {
				t.Fatalf("left out %v, want %s with a reason naming %q", got, w.sections[1].name, tc.reason)
			}
			for _, c := range w.callsNamed("recordConstructGoalSignature") {
				if str(argsOf(t, c), "goalSignature") == w.sections[1].sig {
					t.Fatalf("the left-out section was signed anyway")
				}
			}
		})
	}
}

// TestTheCatalogGateAdmitsOnlyItsAutomation: a catalogued signature is served
// to a later goal without planning, so only the catalogSucceededSections
// automation -- its synthetic actor, under internal origin, naming an owner --
// may catalogue. A person (the run's owner included), another automation, the
// maintenance principal and an unstamped call are refused before anything is
// read; and an owner hint that does not own the run reads nothing and writes
// nothing.
func TestTheCatalogGateAdmitsOnlyItsAutomation(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"the run's owner":         personCtx(testOwner),
		"another automation":      auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "system:automation:learnFromSucceededRun", Synthetic: true})),
		"the maintenance actor":   auth.ContextWithInternalOrigin(maintenanceCtx("sweepConstructReuse")),
		"no internal origin":      auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: catalogAutomationActor, Synthetic: true}),
		"a person under internal": auth.ContextWithInternalOrigin(personCtx(testOwner)),
		"no actor":                context.Background(),
	} {
		t.Run(name, func(t *testing.T) {
			w := newCatalogWorld(t)
			if _, err := w.i.handleCatalogSections(ctx, map[string]any{"runId": catalogRunId, "ownerUserId": testOwner}, 0); err == nil {
				t.Fatal("the gate admitted a caller that is not the catalogSucceededSections automation")
			}
			if len(w.eng.recorded()) != 0 {
				t.Fatalf("a refused caller reached the engine: %s", w.eng.summary())
			}
		})
	}

	t.Run("no owner named", func(t *testing.T) {
		w := newCatalogWorld(t)
		if _, err := w.i.handleCatalogSections(catalogCtx(), map[string]any{"runId": catalogRunId}, 0); err == nil || len(w.eng.recorded()) != 0 {
			t.Fatalf("a call naming no owner was not refused before any read (err %v, %s)", err, w.eng.summary())
		}
	})

	t.Run("a hint that does not own the run", func(t *testing.T) {
		w := newCatalogWorld(t)
		_, err := w.i.handleCatalogSections(catalogCtx(), map[string]any{"runId": catalogRunId, "ownerUserId": "v1:identity:user:mallory"}, 0)
		if err == nil || len(w.eng.writes()) != 0 {
			t.Fatalf("a hint that does not own the run was not refused (err %v, writes %s)", err, w.eng.summary())
		}
		if run := w.eng.callTo(t, "workRunForOwner"); run.Actor != "v1:identity:user:mallory" {
			t.Fatalf("the run was read as %q, want the hint's own actor", run.Actor)
		}
	})

	// The control: the automation itself is admitted and catalogues.
	w := newCatalogWorld(t)
	if catalogued, _ := w.catalogue(t)["catalogued"].([]any); len(catalogued) != 2 {
		t.Fatal("the control catalogued nothing, so the refusals above prove nothing")
	}
}

// TestASectionGate1RefusesIsNotCatalogued: a section is carried ALONE into a
// later goal's draft, so it must compile on its own; one Gate 1 refuses, or a
// node with no gate installed, catalogues nothing.
func TestASectionGate1RefusesIsNotCatalogued(t *testing.T) {
	w := newCatalogWorld(t)
	w.i.compiler = failingGate{}
	reply := w.catalogue(t)
	if catalogued, _ := reply["catalogued"].([]any); len(catalogued) != 0 || len(w.eng.writes()) != 0 {
		t.Fatalf("a section Gate 1 refused was catalogued: %v", reply)
	}

	ungated := newCatalogWorld(t)
	ungated.i.compiler = nil
	reply = ungated.catalogue(t)
	left, _ := reply["notCatalogued"].([]any)
	if len(ungated.eng.writes()) != 0 || len(left) != 2 || !strings.Contains(str(left[0].(map[string]any), "reason"), "no compile gate") {
		t.Fatalf("a node with no Gate 1 catalogued, or did not say why: %v", reply)
	}
}

// TestEveryStatementTheSectionCatalogRendersParses holds every call the
// handler renders to the real front end and the loaded registry, and every
// @serverOnly one -- recordConstructGoalSignature -- to internal origin.
func TestEveryStatementTheSectionCatalogRendersParses(t *testing.T) {
	w := newCatalogWorld(t)
	w.catalogue(t)
	assertStatementsParseAndResolve(t, w.eng.recorded(), []string{
		"workRunForOwner", "workObservationsForOwnerRun", "authoringConstructById", "authoringConstructsForBundle",
		"workStepVersions", "cataloguedConstructsForGoalSignature", "createAuthoringBundle", "createAuthoringConstruct",
		"recordBundleValidation", "setConstructStatus", "catalogueConstruct", "recordConstructGoalSignature",
	})
}
