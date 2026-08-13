package automations

// trigger_validation_test.go -- memql#3614 (an automation loads wired to
// nothing, in six ways, all silent) and memql#3615 (cron expressions are not
// validated at load).
//
// Every case here loaded CLEANLY before the fix, with an automation that never
// fires. The class-level guards at the bottom sweep the shipped tree so the
// corpus cannot regress into any of them.

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func triggerTestLoader(t *testing.T) *Loader {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewLoader(LoaderOptions{Logger: logger})
}

// triggerTestLoaderWithRegistry builds a loader over the full embedded concept
// registry, mirroring engine bootstrap. Needed for the concept-existence gate,
// which is skipped when no registry is available.
func triggerTestLoaderWithRegistry(t *testing.T) *Loader {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := memoryNodes.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry is empty after LoadUnifiedConcepts")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewLoader(LoaderOptions{Logger: logger, Registry: registry})
}

const triggerProbeBody = ` {
  step noop { logic noopLogic { } }
}`

// ---------------------------------------------------------------------------
// #3614 item 1 -- an unrecognised event= silently DISCARDED concept=/partition=
// ---------------------------------------------------------------------------

// TestTrigger_UnrecognisedEventKindRejectsStructuredKwargs pins the live defect:
// `@trigger(event="deploy.requested", concept="v1:cluster:deployment",
// partition="*")` shipped in dsl/deployment/automations.memql and compiled to
// the bare `deploy.requested`, so the concept scoping the author wrote was
// never in effect and nothing said so. Same for a typo'd or mis-capitalised
// node.* kind, where the concept vanishing is the whole failure.
func TestTrigger_UnrecognisedEventKindRejectsStructuredKwargs(t *testing.T) {
	loader := triggerTestLoader(t)

	cases := []struct {
		name    string
		trigger string
	}{
		{
			name:    "live-shape-deploy-requested",
			trigger: `@trigger(event="deploy.requested", concept="v1:cluster:deployment", partition="*")`,
		},
		{
			name:    "typo-node-create",
			trigger: `@trigger(event="node.create", concept="v1:cluster:node", partition="*")`,
		},
		{
			name:    "capitalisation-node-Created",
			trigger: `@trigger(event="node.Created", concept="v1:cluster:node")`,
		},
		{
			name:    "already-composed-topic-plus-concept",
			trigger: `@trigger(event="graph.node.created.v1:cluster:node", concept="v1:cluster:node")`,
		},
		{
			name:    "partition-only",
			trigger: `@trigger(event="system.startup", partition="*")`,
		},
		{
			name:    "schedule-trigger-carrying-concept",
			trigger: `@trigger(schedule="0 */10 * * * *", concept="v1:cluster:node")`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.trigger + "\nautomation strayKwargProbe" + triggerProbeBody
			_, err := loader.compileMemQL(src, "test:"+tc.name)
			if err == nil {
				t.Fatalf("expected a refusal for %s -- the kwargs are dropped, so the scoping is not in effect", tc.trigger)
			}
			// The message has to name the dropping, not just say "invalid".
			for _, want := range []string{"strayKwargProbe", "dropped"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error must mention %q, got: %v", want, err)
				}
			}
		})
	}
}

// TestTrigger_RawTopicWithoutStrayKwargsStillLoads is the other half: a raw
// application / system topic is a legitimate subscription and must keep
// working. Refusing every unrecognised event= would have taken out
// system.startup, system.shutdown, the already-composed graph.node.* spelling,
// and deploy.requested -- whose publisher lives in the memql-cockpit repo.
func TestTrigger_RawTopicWithoutStrayKwargsStillLoads(t *testing.T) {
	loader := triggerTestLoader(t)

	for _, topic := range []string{
		"system.startup",
		"system.shutdown",
		"deploy.requested",
		"cognition.response.requested",
		"graph.node.updated.v1:planner:plan",
	} {
		t.Run(topic, func(t *testing.T) {
			src := `@trigger(event="` + topic + `")` + "\nautomation rawTopicProbe" + triggerProbeBody
			auto, err := loader.compileMemQL(src, "test:raw:"+topic)
			if err != nil {
				t.Fatalf("raw topic %q must still load: %v", topic, err)
			}
			if auto.Trigger == nil || auto.Trigger.Event != topic {
				t.Fatalf("raw topic %q must subscribe verbatim, got %+v", topic, auto.Trigger)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// #3614 item 3 -- a typo'd kwarg / @trigger() / no trigger all loaded enabled
// ---------------------------------------------------------------------------

// TestTrigger_NoReachableTriggerIsRefused pins the class: three spellings that
// all produced `topic="" schedule="" eventTriggered=false scheduled=false
// enabled=true` -- registered in s.automations, counted in automationCount,
// subscribed to nothing and scheduled for never.
//
// The AUTHORED scheduler has always refused this ("neither an event trigger
// nor a schedule"); the core loader did not. They agree now.
func TestTrigger_NoReachableTriggerIsRefused(t *testing.T) {
	loader := triggerTestLoader(t)

	cases := []struct {
		name     string
		preamble string
		// want is the substring the refusal must carry. The issue's own
		// `evnt=` example ALSO carries a stray concept=, so it is caught one
		// gate earlier -- still a refusal, different (and more specific)
		// wording.
		want string
	}{
		{
			name:     "typoed-event-kwarg",
			preamble: `@trigger(evnt="node.created")`,
			want:     "neither an event trigger nor a schedule",
		},
		{
			name:     "typoed-event-kwarg-with-concept",
			preamble: `@trigger(evnt="node.created", concept="v1:cluster:node")`,
			want:     "dropped",
		},
		{
			name:     "typoed-schedule-kwarg",
			preamble: `@trigger(cron="0 0 4 * * *")`,
			want:     "neither an event trigger nor a schedule",
		},
		{
			name:     "empty-trigger",
			preamble: `@trigger()`,
			want:     "neither an event trigger nor a schedule",
		},
		{
			name:     "no-trigger-at-all",
			preamble: `@description("no trigger")`,
			want:     "neither an event trigger nor a schedule",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.preamble + "\nautomation unwiredProbe" + triggerProbeBody
			_, err := loader.compileMemQL(src, "test:"+tc.name)
			if err == nil {
				t.Fatalf("expected a refusal: %s loads wired to nothing", tc.preamble)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected a refusal mentioning %q, got: %v", tc.want, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// #3614 item 4 -- a nonexistent concept id in concept= was accepted
// ---------------------------------------------------------------------------

// TestTrigger_NonexistentConceptIsRefused pins the registry check. Only
// `strings.Contains(conceptId, ":")` was verified, so `v1:cluster:nodeZZZ`
// compiled to `graph.node.created.v1:cluster:nodeZZZ` -- a topic no CDC event
// can ever carry. The loader already held the registry.
func TestTrigger_NonexistentConceptIsRefused(t *testing.T) {
	loader := triggerTestLoaderWithRegistry(t)

	src := `@trigger(event="node.created", concept="v1:cluster:nodeZZZ")` +
		"\nautomation deadConceptProbe" + triggerProbeBody
	_, err := loader.compileMemQL(src, "test:dead-concept")
	if err == nil {
		t.Fatal("expected a refusal: v1:cluster:nodeZZZ is not a registered concept")
	}
	if !strings.Contains(err.Error(), "v1:cluster:nodeZZZ") || !strings.Contains(err.Error(), "not a registered concept") {
		t.Errorf("error must name the unresolvable concept, got: %v", err)
	}

	// The same shape with a REAL concept must still compile to the canonical
	// topic -- the gate must not have narrowed the working case.
	good := `@trigger(event="node.created", concept="v1:cluster:node")` +
		"\nautomation liveConceptProbe" + triggerProbeBody
	auto, err := loader.compileMemQL(good, "test:live-concept")
	if err != nil {
		t.Fatalf("a registered concept must still compile: %v", err)
	}
	if auto.Trigger == nil || auto.Trigger.Event != "graph.node.created.v1:cluster:node" {
		t.Fatalf("unexpected canonical topic: %+v", auto.Trigger)
	}
}

// ---------------------------------------------------------------------------
// #3614 item 5 -- two @trigger annotations, last one silently wins
// ---------------------------------------------------------------------------

// TestTrigger_DuplicateTriggerIsRefused pins that the first @trigger is no
// longer discarded in silence. The parser folds attributes in order, so an
// automation carrying two triggers wired itself to whichever came second.
func TestTrigger_DuplicateTriggerIsRefused(t *testing.T) {
	loader := triggerTestLoaderWithRegistry(t)

	cases := []struct{ name, preamble string }{
		{
			name: "two-event-triggers",
			preamble: `@trigger(event="node.created", concept="v1:cluster:node")` + "\n" +
				`@trigger(event="node.deleted", concept="v1:cluster:node")`,
		},
		{
			name: "event-then-schedule",
			preamble: `@trigger(event="system.startup")` + "\n" +
				`@trigger(schedule="0 */10 * * * *")`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.preamble + "\nautomation doubleTriggerProbe" + triggerProbeBody
			_, err := loader.compileMemQL(src, "test:"+tc.name)
			if err == nil {
				t.Fatal("expected a refusal: all but the last @trigger are discarded")
			}
			if !strings.Contains(err.Error(), "exactly one is allowed") {
				t.Errorf("error must state the one-trigger rule, got: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// #3615 -- cron expressions were not validated at load
// ---------------------------------------------------------------------------

// TestCron_InvalidExpressionsRefusedAtLoad walks the exact list from
// memql#3615. Every one of these used to load with scheduled=true; the
// scheduler's AddFunc error was swallowed by `logError(...); continue`, so boot
// succeeded and the automation never ran.
func TestCron_InvalidExpressionsRefusedAtLoad(t *testing.T) {
	loader := triggerTestLoader(t)

	for _, expr := range []string{
		"not a cron at all",
		"*/10 * * * *",
		"0 2 * * *",
		"0 0 0 * * * *",
		"@yearlyish",
		"0 99 * * * *",
	} {
		t.Run(expr, func(t *testing.T) {
			src := `@trigger(schedule="` + expr + `")` + "\nautomation cronProbe" + triggerProbeBody
			_, err := loader.compileMemQL(src, "test:cron")
			if err == nil {
				t.Fatalf("expected a refusal for schedule %q -- it loads scheduled=true and never fires", expr)
			}
			if !strings.Contains(err.Error(), "invalid cron schedule") {
				t.Errorf("expected an invalid-cron refusal, got: %v", err)
			}
		})
	}
}

// TestCron_FiveFieldSpellingNamesItsSixFieldEquivalent is the sharp edge.
// `0 2 * * *` is crontab.guru's "daily at 2am" -- the spelling every operator
// knows, and silently dead here. The refusal has to hand back the exact string
// to write, because the alternative (promoting it silently) would leave the
// corpus carrying two field-count conventions at once, which is how
// `*/5 * * * * *` gets read as "every 5 minutes".
func TestCron_FiveFieldSpellingNamesItsSixFieldEquivalent(t *testing.T) {
	cases := []struct{ expr, want string }{
		{expr: "0 2 * * *", want: "0 0 2 * * *"},
		{expr: "*/10 * * * *", want: "0 */10 * * * *"},
		{expr: "*/5 * * * *", want: "0 */5 * * * *"},
	}

	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			err := validateCronExpression(tc.expr)
			if err == nil {
				t.Fatalf("5-field %q must be refused, not promoted behind the author's back", tc.expr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal must name the 6-field equivalent %q, got: %v", tc.want, err)
			}
			if !strings.Contains(err.Error(), "5-field") {
				t.Errorf("refusal must say WHY (the 5-field crontab spelling), got: %v", err)
			}
		})
	}
}

// TestCron_ValidSixFieldExpressionsAccepted keeps the gate from swallowing the
// corpus. Every one of these is a shipped spelling.
func TestCron_ValidSixFieldExpressionsAccepted(t *testing.T) {
	for _, expr := range []string{
		"0 */10 * * * *",
		"0 0 4 * * *",
		"0 45 2 * * *",
		"0 */5 * * * *",
		"0 0 * * * *",
		"@daily",
		"@every 1h30m",
	} {
		t.Run(expr, func(t *testing.T) {
			if err := validateCronExpression(expr); err != nil {
				t.Fatalf("valid schedule %q refused: %v", expr, err)
			}
		})
	}
}

// TestCron_SubMinuteDetection covers the OTHER direction of the 6-vs-5
// ambiguity -- the one that cannot be refused because it is legal.
// `*/5 * * * * *` reads as "every 5 minutes" to anyone with crontab habits and
// fires every 5 seconds; the loader warns rather than refusing.
func TestCron_SubMinuteDetection(t *testing.T) {
	cases := []struct {
		expr string
		want bool
	}{
		{expr: "*/5 * * * * *", want: true},
		{expr: "0,30 * * * * *", want: true},
		{expr: "* * * * * *", want: true},
		{expr: "0-10 * * * * *", want: true},
		{expr: "@every 30s", want: true},
		{expr: "0 */5 * * * *", want: false},
		{expr: "0 0 4 * * *", want: false},
		{expr: "30 * * * * *", want: false},
		{expr: "@daily", want: false},
		{expr: "@every 5m", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.expr, func(t *testing.T) {
			if got := cronFiresSubMinute(tc.expr); got != tc.want {
				t.Errorf("cronFiresSubMinute(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Class-level guards over the SHIPPED tree
// ---------------------------------------------------------------------------

// TestShippedTree_EveryAutomationIsWiredToSomething is the corpus pin. It is
// deliberately an assertion about every automation rather than about the ones
// this issue found: a future automation that loses its trigger to a typo, a
// mis-capitalised event kind, a dropped concept kwarg, a retired concept id or
// an unparseable cron fails HERE, at the class, rather than by not running in
// production.
func TestShippedTree_EveryAutomationIsWiredToSomething(t *testing.T) {
	loader := triggerTestLoaderWithRegistry(t)

	automations, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("the shipped tree must load clean: %v", err)
	}
	if len(automations) == 0 {
		t.Fatal("no automations loaded -- the guard would be vacuous")
	}

	for _, a := range automations {
		if !a.IsEventTriggered() && !a.IsScheduled() {
			t.Errorf("automation %q (%s) is wired to nothing", a.Name, a.Origin)
			continue
		}
		if a.IsScheduled() {
			if err := validateCronExpression(a.Schedule); err != nil {
				t.Errorf("automation %q (%s): %v", a.Name, a.Origin, err)
			}
		}
	}
}

// TestShippedTree_EveryGraphTriggerNamesARegisteredConcept pins item 4 across
// the corpus: a graph.node.* topic whose concept segment does not resolve can
// never match a CDC event.
func TestShippedTree_EveryGraphTriggerNamesARegisteredConcept(t *testing.T) {
	loader := triggerTestLoaderWithRegistry(t)
	registry := memoryNodes.DefaultRegistry()

	automations, err := loader.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	checked := 0
	for _, a := range automations {
		if !a.IsEventTriggered() {
			continue
		}
		concept := extractConceptFromTopic(a.Trigger.Event)
		if concept == "" {
			continue
		}
		checked++
		if _, err := registry.Get(concept); err != nil {
			t.Errorf("automation %q subscribes to %q, but concept %q is not registered: %v",
				a.Name, a.Trigger.Event, concept, err)
		}
	}
	if checked == 0 {
		t.Fatal("no concept-scoped triggers found -- the guard would be vacuous")
	}
}
