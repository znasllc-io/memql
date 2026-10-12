package emailrules

// live_version_test.go -- an email rule's row names the version that is live
// and says what happened to it (memql#6022, Task 3b fix round):
//
//   - a breaker that stops the rule marks it paused, with the sentence the app
//     shows, written once by the node that recorded the pause;
//   - an edit that is refused leaves the row naming the version that is still
//     running, so turning the rule off retires THAT version;
//   - an activation that went live is recorded even if its caller hung up, and
//     an attempt the caller abandoned before it took effect records nothing.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

const (
	testOwner = "v1:identity:user:owner1"
	testRule  = "v1:campaigns:emailRule:ab12cd34"
)

// rowEngine is fakeEngine with a rule row that changes: every
// recordEmailRuleGeneration is applied to it, so later reads see what was
// recorded. It honours a cancelled context the way a database driver does,
// and records the actor each call ran as.
type rowEngine struct {
	*fakeEngine
	actors []string
}

var recordArg = regexp.MustCompile(`(status|bundleId|constructName|lastError): "((?:[^"\\]|\\.)*)"`)

func (e *rowEngine) Execute(ctx context.Context, q string) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	actor := ""
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		actor = ac.UserId
	}
	e.actors = append(e.actors, actor)
	out, err := e.fakeEngine.Execute(ctx, q)
	if err == nil && strings.HasPrefix(q, "mutation recordEmailRuleGeneration") && e.rule != nil {
		for _, m := range recordArg.FindAllStringSubmatch(q, -1) {
			e.rule[m[1]] = strings.ReplaceAll(m[2], `\"`, `"`)
		}
	}
	return out, err
}

// records are the rule-row records in the order they were made, with the
// actor and origin each ran under.
func (e *rowEngine) records() (calls []string, actors []string, internal []bool) {
	for i, c := range e.fakeEngine.calls {
		if strings.HasPrefix(c, "mutation recordEmailRuleGeneration") {
			calls = append(calls, c)
			actors = append(actors, e.actors[i])
			internal = append(internal, e.fakeEngine.internal[i])
		}
	}
	return calls, actors, internal
}

func liveRule(bundleID string) map[string]any {
	r := ruleRow()
	r["status"] = "active"
	r["bundleId"] = bundleID
	r["constructName"] = ConstructNameFor(testRule)
	return r
}

// ruleBundle is the bundle a rule's activation at version writes, as a read
// returns it once a breaker paused it: its id derived from the rule and the
// version, and the rule named in its dry-run report.
func ruleBundle(ruleID string, version int) (string, map[string]any) {
	id := "v1:authoring:bundle:" + bundleIDFor(ruleID, version)
	return id, map[string]any{
		"id": id, "version": version, "status": "paused",
		"dryRunReport": map[string]any{"ok": true, "generator": "component/emailrules", "rule": ruleID},
	}
}

// TestABreakerPauseMarksTheRulePaused: the node that records a breaker's pause
// marks the rule the paused bundle was built for -- status paused, the
// sentence the app shows, the bundle kept so turning the rule on again
// supersedes it. It reads and writes as the rule's owner, the write with
// internal origin. Turning the rule on again clears the sentence.
func TestABreakerPauseMarksTheRulePaused(t *testing.T) {
	paused, bundle := ruleBundle(testRule, 1)
	e := &rowEngine{fakeEngine: &fakeEngine{rule: liveRule(paused), bundle: bundle}}

	if err := recordBreakerPause(context.Background(), e, testOwner, paused); err != nil {
		t.Fatalf("record the breaker's pause: %v", err)
	}
	if e.rule["status"] != "paused" || e.rule["lastError"] != BreakerStoppedSentence || e.rule["bundleId"] != paused {
		t.Fatalf("the rule row reads status %q, lastError %q, bundleId %q; want paused, %q, and the paused bundle",
			e.rule["status"], e.rule["lastError"], e.rule["bundleId"], BreakerStoppedSentence)
	}
	for i, actor := range e.actors {
		if actor != testOwner {
			t.Fatalf("call %q ran as %q; every read and write of the pause runs as the rule's owner", e.fakeEngine.calls[i], actor)
		}
	}
	calls, _, internal := e.records()
	if len(calls) != 1 || !internal[0] {
		t.Fatalf("the pause was recorded %d times (internal origin %v); want once, with internal origin", len(calls), internal)
	}

	// Turning it on again: the new version supersedes the paused one, and the
	// sentence goes.
	res, err := NewActivator(e, deps()).Activate(ownerCtx(auth.RoleOwner), testOwner, testRule)
	if err != nil {
		t.Fatalf("turn the rule on again: %v", err)
	}
	if e.rule["status"] != "active" || e.rule["lastError"] != "" || e.rule["bundleId"] != res.BundleID {
		t.Fatalf("after turning it on again the rule reads status %q, lastError %q, bundleId %q (activated %q)",
			e.rule["status"], e.rule["lastError"], e.rule["bundleId"], res.BundleID)
	}
}

// TestABreakerPauseOfABundleTheRuleNoLongerRunsChangesNothing: the pause marks
// a rule only while the rule reads active and runs the paused bundle. A bundle
// that is not an email rule's is not read at all; a rule that moved on -- an
// edit armed a newer version, or it was turned off -- is left as it is, and so
// is one that does not read active; and a bundle whose row names a rule that
// does not derive its id names no rule.
func TestABreakerPauseOfABundleTheRuleNoLongerRunsChangesNothing(t *testing.T) {
	v1, bundle := ruleBundle(testRule, 1)
	v2, _ := ruleBundle(testRule, 2)
	turnedOff := ruleRow()
	turnedOff["status"] = "paused"
	turnedOff["bundleId"] = ""
	refused := liveRule(v1)
	refused["status"] = "failed"
	refused["lastError"] = "authoring: the generated automation does not compile"
	otherRules, claimed := ruleBundle("v1:campaigns:emailRule:other0001", 1)
	claimed["dryRunReport"] = map[string]any{"rule": testRule}

	for _, c := range []struct {
		name     string
		rule     map[string]any
		bundle   map[string]any
		bundleID string
		readRule bool
	}{
		{"an edit armed a newer version", liveRule(v2), bundle, v1, true},
		{"the rule was turned off", turnedOff, bundle, v1, true},
		{"the rule does not read active", refused, bundle, v1, true},
		{"the bundle's row names a rule that does not derive it", liveRule(otherRules), claimed, otherRules, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := map[string]any{"status": c.rule["status"], "bundleId": c.rule["bundleId"]}
			e := &rowEngine{fakeEngine: &fakeEngine{rule: c.rule, bundle: c.bundle}}
			if err := recordBreakerPause(context.Background(), e, testOwner, c.bundleID); err != nil {
				t.Fatalf("record the pause: %v", err)
			}
			if calls, _, _ := e.records(); len(calls) != 0 || e.rule["status"] != before["status"] || e.rule["bundleId"] != before["bundleId"] {
				t.Fatalf("the pause changed a rule that does not run the bundle: %v (status %q, bundleId %q)", calls, e.rule["status"], e.rule["bundleId"])
			}
			readRule := false
			for _, q := range e.fakeEngine.calls {
				readRule = readRule || strings.HasPrefix(q, "query emailRuleById")
			}
			if readRule != c.readRule {
				t.Fatalf("the rule was read: %v, want %v (calls %v)", readRule, c.readRule, e.fakeEngine.calls)
			}
		})
	}

	e := &rowEngine{fakeEngine: &fakeEngine{rule: liveRule(v1), bundle: bundle}}
	if err := recordBreakerPause(context.Background(), e, testOwner, "v1:authoring:bundle:planner-goal-1"); err != nil {
		t.Fatalf("a bundle that is not an email rule's: %v", err)
	}
	if len(e.fakeEngine.calls) != 0 {
		t.Fatalf("a bundle that is not an email rule's was read: %v", e.fakeEngine.calls)
	}
}

// pauseRegistrar is an engine that takes a pause listener, as the live engine
// does.
type pauseRegistrar struct {
	*rowEngine
	listeners map[string]memql.AuthoredPauseListener
}

func (r *pauseRegistrar) SetAuthoredPauseListener(name string, l memql.AuthoredPauseListener) {
	if r.listeners == nil {
		r.listeners = map[string]memql.AuthoredPauseListener{}
	}
	r.listeners[name] = l
}

// TestBindRegistersTheBreakerPauseListener: the App's one handoff to this
// package also hands the engine the listener, so no other wiring is needed --
// and the listener it hands over is the one that marks the rule.
func TestBindRegistersTheBreakerPauseListener(t *testing.T) {
	paused, bundle := ruleBundle(testRule, 1)
	r := &pauseRegistrar{rowEngine: &rowEngine{fakeEngine: &fakeEngine{rule: liveRule(paused), bundle: bundle}}}
	Bind(r, deps())
	t.Cleanup(func() { Bind(nil, nil) })
	listener := r.listeners[IntegrationName]
	if listener == nil {
		t.Fatalf("Bind registered no breaker-pause listener on the engine (have %v)", r.listeners)
	}
	if err := listener(context.Background(), testOwner, paused); err != nil {
		t.Fatalf("the registered listener: %v", err)
	}
	if r.rule["status"] != "paused" || r.rule["lastError"] != BreakerStoppedSentence {
		t.Fatalf("the listener Bind registered left the rule reading %q / %q", r.rule["status"], r.rule["lastError"])
	}
}

// TestARefusedEditKeepsTheRuleNamingTheVersionThatIsLive: an edit whose new
// version is refused leaves the version that was live armed and sending, so
// the row keeps naming it and keeps reading active, and says why the edit did
// not take. Turning the rule off then retires the version that is running.
func TestARefusedEditKeepsTheRuleNamingTheVersionThatIsLive(t *testing.T) {
	const live = "v1:authoring:bundle:emrlivever01"
	e := &rowEngine{fakeEngine: &fakeEngine{rule: liveRule(live), bundle: map[string]any{"id": live, "version": 1, "status": "active"}}}
	refusal := errors.New("authoring: automation emailRuleAb12cd34 does not compile on this node")
	e.activate = func(_, bundleID string) error {
		if bundleID != live {
			return refusal
		}
		return nil
	}

	if _, err := NewActivator(e, deps()).Activate(ownerCtx(auth.RoleOwner), testOwner, testRule); !errors.Is(err, refusal) {
		t.Fatalf("a refused edit must say so, got %v", err)
	}
	lastError, _ := e.rule["lastError"].(string)
	if e.rule["status"] != "active" || e.rule["bundleId"] != live {
		t.Fatalf("after a refused edit the rule reads status %q, bundleId %q; want active, still naming the live version %q",
			e.rule["status"], e.rule["bundleId"], live)
	}
	if !strings.HasPrefix(lastError, refusal.Error()) || !strings.HasSuffix(lastError, " "+PreviousVersionStillRunning) {
		t.Fatalf("after a refused edit lastError is %q; want the refusal followed by %q", lastError, PreviousVersionStillRunning)
	}

	var retired string
	e.retire = func(_, bundleID string) error { retired = bundleID; return nil }
	if _, err := NewActivator(e, deps()).Retire(ownerCtx(auth.RoleOwner), testOwner, testRule); err != nil {
		t.Fatalf("turn the rule off: %v", err)
	}
	if retired != live {
		t.Fatalf("turning the rule off retired %q; the version that is running is %q", retired, live)
	}
}

// TestARefusedFirstActivationStillReadsFailed: with no version live before it,
// a refused activation is what it always was -- the rule reads failed, with
// the refusal alone.
func TestARefusedFirstActivationStillReadsFailed(t *testing.T) {
	e := &rowEngine{fakeEngine: &fakeEngine{rule: ruleRow()}}
	refusal := errors.New("authoring: automation emailRuleAb12cd34 does not compile on this node")
	e.activate = func(_, _ string) error { return refusal }

	if _, err := NewActivator(e, deps()).Activate(ownerCtx(auth.RoleOwner), testOwner, testRule); !errors.Is(err, refusal) {
		t.Fatalf("a refused first activation must say so, got %v", err)
	}
	if e.rule["status"] != "failed" || e.rule["lastError"] != refusal.Error() {
		t.Fatalf("a refused first activation reads status %q, lastError %q; want failed with the refusal alone",
			e.rule["status"], e.rule["lastError"])
	}
}

// TestALiveActivationIsRecordedWhenTheCallerHangsUp: the caller hangs up right
// after the activation went live. It did go live, so the row records it -- left
// naming the old version, turning the rule off would retire the wrong bundle
// and turning it on would be refused as a conflict with the live one.
func TestALiveActivationIsRecordedWhenTheCallerHangsUp(t *testing.T) {
	ctx, hangUp := context.WithCancel(ownerCtx(auth.RoleOwner))
	defer hangUp()
	e := &rowEngine{fakeEngine: &fakeEngine{rule: ruleRow()}}
	e.activate = func(_, _ string) error { hangUp(); return nil }

	res, err := NewActivator(e, deps()).Activate(ctx, testOwner, testRule)
	if err != nil {
		t.Fatalf("an activation that went live must be reported live: %v", err)
	}
	if e.rule["status"] != "active" || e.rule["bundleId"] != res.BundleID || res.BundleID == "" {
		t.Fatalf("the caller hung up after the activation went live and the rule reads status %q, bundleId %q (activated %q)",
			e.rule["status"], e.rule["bundleId"], res.BundleID)
	}
}

// TestAnAttemptTheCallerAbandonsLeavesTheRuleAsItWas: a caller that hangs up
// before the activation took effect changed nothing -- the activation undoes
// what it did before its commit point -- so nothing is recorded: the row is
// not marked failed for a hang-up, and an edit's row keeps naming the live
// version without a "context canceled" reason. Only what happened is recorded
// whatever the caller does.
func TestAnAttemptTheCallerAbandonsLeavesTheRuleAsItWas(t *testing.T) {
	const live = "v1:authoring:bundle:emrlivever02"
	for _, c := range []struct {
		name   string
		rule   map[string]any
		bundle map[string]any
	}{
		{"a first activation", ruleRow(), nil},
		{"an edit", liveRule(live), map[string]any{"id": live, "version": 1, "status": "active"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := map[string]any{"status": c.rule["status"], "bundleId": c.rule["bundleId"], "lastError": c.rule["lastError"]}
			ctx, hangUp := context.WithCancel(ownerCtx(auth.RoleOwner))
			defer hangUp()
			e := &rowEngine{fakeEngine: &fakeEngine{rule: c.rule, bundle: c.bundle}}
			e.activate = func(_, bundleID string) error {
				if bundleID == live {
					return nil
				}
				hangUp()
				return context.Canceled
			}

			if _, err := NewActivator(e, deps()).Activate(ctx, testOwner, testRule); !errors.Is(err, context.Canceled) {
				t.Fatalf("an abandoned activation must report the hang-up, got %v", err)
			}
			if calls, _, _ := e.records(); len(calls) != 0 {
				t.Fatalf("an abandoned activation recorded %v on the rule; nothing took effect", calls)
			}
			for k, v := range before {
				if e.rule[k] != v {
					t.Fatalf("an abandoned activation changed the rule's %s from %v to %v", k, v, e.rule[k])
				}
			}
		})
	}
}
