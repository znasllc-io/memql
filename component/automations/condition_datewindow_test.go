package automations

import (
	"context"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// Regression for the #2256 condition hardening (#2254 date windows, #2257
// empty-string compares). These conditions are exactly the per-row gates the
// #2235 forEach sweeps evaluate; in the string evaluator they were
// constant-false (date windows) or over-fired (`!= ""`). They are written the
// edition-2026 way and evaluated through EvalCondition over the run, as a
// forEach filter is.
//
// Multi-node note: no node-local state is read; the triggering row travels
// with the event, so the behaviour is identical across the 2-replica mesh.

// evalItemCond evaluates `cond` with `item` bound as the forEach loop item and
// a fixed run clock (the seeded `timestamp`, which `now` reads), mirroring the
// sweep's per-row gate.
func evalItemCond(t *testing.T, item map[string]any, now, cond string) bool {
	t.Helper()
	e := NewEvaluator()
	e.SetItem(item, "item")
	e.SetCustom("timestamp", now)
	got, err := evalV1Cond(t, e, cond)
	if err != nil {
		t.Fatalf("%s: %v", cond, err)
	}
	return got
}

// #2254 gap (i)+(ii): a date-window gate `addDuration(ts, "P30D") < now`
// must fire for a row whose cooldown elapsed long ago. Pre-fix this was
// `0 < 0` -> false and the account-deletion / access-expiry / worker-retention
// crons never ran.
func TestCondition_DateWindow_Past_Fires(t *testing.T) {
	item := map[string]any{"payload": map[string]any{"deletionScheduledAt": "2020-01-01T00:00:00Z"}}
	cond := `addDuration(item.payload.deletionScheduledAt, "P30D") < now`
	if !evalItemCond(t, item, "2026-06-27T00:00:00Z", cond) {
		t.Errorf("%s = false for a row 5y past its cooldown, want true (#2254)", cond)
	}
}

// The same gate must NOT fire when the window has not yet elapsed.
func TestCondition_DateWindow_Future_DoesNotFire(t *testing.T) {
	item := map[string]any{"payload": map[string]any{"deletionScheduledAt": "2026-06-20T00:00:00Z"}}
	cond := `addDuration(item.payload.deletionScheduledAt, "P30D") < now`
	// 2026-06-20 + 30d = 2026-07-20, which is AFTER now (2026-06-27).
	if evalItemCond(t, item, "2026-06-27T00:00:00Z", cond) {
		t.Errorf("%s = true before the window elapsed, want false (#2254)", cond)
	}
}

// Even a bare timestamp comparison (no addDuration) must compare as dates, not
// coerce both sides to 0. Pre-fix `<` returned false for any RFC3339 operand.
func TestCondition_BareTimestampCompare(t *testing.T) {
	item := map[string]any{"payload": map[string]any{"scheduledAt": "2020-01-01T00:00:00Z"}}
	if !evalItemCond(t, item, "2026-06-27T00:00:00Z", "item.payload.scheduledAt < now") {
		t.Errorf("item.payload.scheduledAt < now = false for a 2020 date, want true (#2254)")
	}
	itemFuture := map[string]any{"payload": map[string]any{"scheduledAt": "2030-01-01T00:00:00Z"}}
	if evalItemCond(t, itemFuture, "2026-06-27T00:00:00Z", "item.payload.scheduledAt < now") {
		t.Errorf("item.payload.scheduledAt < now = true for a 2030 date, want false (#2254)")
	}
}

// The full compound gate from the issue: the cooldown is derived through
// `+` and `??` inside addDuration(...), feeding gap (i) date arithmetic.
func TestCondition_DateWindow_ConcatCoalesceCooldown(t *testing.T) {
	// cooldown present -> "P" + "15" + "D" = P15D; 2026-06-01 + 15d = 2026-06-16 < now.
	item := map[string]any{"payload": map[string]any{
		"deletionScheduledAt": "2026-06-01T00:00:00Z",
		"cooldownDays":        "15",
	}}
	cond := `addDuration(item.payload.deletionScheduledAt, "P" + (item.payload.cooldownDays ?? "30") + "D") < now`
	if !evalItemCond(t, item, "2026-06-27T00:00:00Z", cond) {
		t.Errorf("compound P15D window = false, want true (#2254/#2256)")
	}
	// cooldown absent -> ?? falls back to "30" -> P30D; 2026-06-20 + 30d = 2026-07-20 > now.
	itemDefault := map[string]any{"payload": map[string]any{"deletionScheduledAt": "2026-06-20T00:00:00Z"}}
	if evalItemCond(t, itemDefault, "2026-06-27T00:00:00Z", cond) {
		t.Errorf("compound default-P30D window = true before elapsing, want false (#2254/#2256)")
	}
}

// #2257 gap (iii): `X != ""` must fire ONLY for a non-empty value, not for an
// empty-string field and not for an absent field. Pre-fix the empty-string row
// over-fired (the kill-switch over-suspend).
func TestCondition_NotEmptyString_GatesCorrectly(t *testing.T) {
	scoped := map[string]any{"payload": map[string]any{"computerUseScope": "full"}}
	empty := map[string]any{"payload": map[string]any{"computerUseScope": ""}}
	missing := map[string]any{"payload": map[string]any{}}

	cond := `item.payload.computerUseScope != ""`
	if !evalItemCond(t, scoped, "2026-06-27T00:00:00Z", cond) {
		t.Errorf("%s = false for a scoped row, want true", cond)
	}
	if evalItemCond(t, empty, "2026-06-27T00:00:00Z", cond) {
		t.Errorf("%s = true for an EMPTY-string row, want false (#2257 over-fire)", cond)
	}
	if evalItemCond(t, missing, "2026-06-27T00:00:00Z", cond) {
		t.Errorf("%s = true for a MISSING-field row, want false (#2257)", cond)
	}
}

// And the `== ""` mirror: fires for empty + missing, not for a scoped value.
func TestCondition_EqualsEmptyString_GatesCorrectly(t *testing.T) {
	scoped := map[string]any{"payload": map[string]any{"computerUseScope": "full"}}
	empty := map[string]any{"payload": map[string]any{"computerUseScope": ""}}
	missing := map[string]any{"payload": map[string]any{}}

	cond := `item.payload.computerUseScope == ""`
	if evalItemCond(t, scoped, "2026-06-27T00:00:00Z", cond) {
		t.Errorf("%s = true for a scoped row, want false", cond)
	}
	if !evalItemCond(t, empty, "2026-06-27T00:00:00Z", cond) {
		t.Errorf("%s = false for an empty-string row, want true (#2257)", cond)
	}
	if !evalItemCond(t, missing, "2026-06-27T00:00:00Z", cond) {
		t.Errorf("%s = false for a missing field, want true (#2257)", cond)
	}
}

// The compound kill-switch gate combining the empty-check with an enabled flag
// (#2257): suspend only when the user disabled compute-use AND the plan has a
// real scope. Empty / missing scope must not suspend.
func TestCondition_KillSwitchCompound(t *testing.T) {
	cond := `item.payload.computerUseScope != "" && item.payload.enabled == false`
	scoped := map[string]any{"payload": map[string]any{"computerUseScope": "full", "enabled": false}}
	empty := map[string]any{"payload": map[string]any{"computerUseScope": "", "enabled": false}}
	if !evalItemCond(t, scoped, "2026-06-27T00:00:00Z", cond) {
		t.Errorf("compound kill-switch = false for a scoped+disabled plan, want true (#2257)")
	}
	if evalItemCond(t, empty, "2026-06-27T00:00:00Z", cond) {
		t.Errorf("compound kill-switch = true for an empty-scope plan, want false (#2257 over-suspend)")
	}
}

// daysBetween as a comparison operand: the streak gate "today is exactly one
// day after the last activity".
func TestCondition_DaysBetweenOperand(t *testing.T) {
	cond := `daysBetween(item.payload.lastActiveAt, now) == 1`
	item := map[string]any{"payload": map[string]any{"lastActiveAt": "2026-07-13T09:00:00Z"}}
	if !evalItemCond(t, item, "2026-07-14T09:00:00Z", cond) {
		t.Errorf("%s = false for a row exactly one day old, want true (#2541)", cond)
	}
	sameDay := map[string]any{"payload": map[string]any{"lastActiveAt": "2026-07-14T08:00:00Z"}}
	if evalItemCond(t, sameDay, "2026-07-14T09:00:00Z", cond) {
		t.Errorf("%s = true for a same-day row, want false (#2541)", cond)
	}
}

// #2707 (the #2620 ruling): the seven calendar builtins retired under the
// 2026.08 epoch are not condition operands. In the string evaluator a
// condition naming one fell through to a constant-false literal; in edition
// 2026 it is refused -- it never resolves a calendar value, and never reads
// true. addDuration and daysBetween remain first-class operands (tests above).
func TestCondition_RetiredCalendarBuiltinsDoNotResolve(t *testing.T) {
	item := map[string]any{"payload": map[string]any{
		"startedAt": "2024-07-14T00:00:00Z",
	}}
	cases := []string{
		`quarter(now) == 3`,
		`isAnniversary(item.payload.startedAt, now) == true`,
		`year(now) == 2026`,
	}
	for _, cond := range cases {
		cond := cond
		t.Run(cond, func(t *testing.T) {
			e := NewEvaluator()
			e.SetItem(item, "item")
			e.SetCustom("timestamp", "2026-07-14T09:00:00Z")
			n, err := languageParser.ParseV1Expression(cond)
			if err != nil {
				return // refused at parse
			}
			got, err := e.EvalV1Condition(context.Background(), n)
			if err == nil && got {
				t.Errorf("%s = true; retired builtins must not resolve as condition operands (#2707)", cond)
			}
			if err == nil {
				t.Errorf("%s evaluated without error; a retired builtin must be refused, not read as false", cond)
			}
		})
	}
}
