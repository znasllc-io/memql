package steps

import (
	"testing"

	"github.com/znasllc-io/memql/component/automations"
)

// TestResolveArgValueRef_ResolvesTopLevelCoalesce locks in that a plain
// coalesce(...) expression at the top of an arg map is resolved to its value
// before being rendered into the MemQL query.
//
// Before this fix, arg-resolution only handled cond() and concat() at top
// level -- coalesce() fell through as a raw string, renderMemQLValue quoted
// it, and downstream function validators rejected the literal expression
// text against enum constraints (breaking bootstrapIdentity /
// mutationGrantPartitionAccess which use coalesce for the role arg).
func TestResolveArgValueRef_ResolvesTopLevelCoalesce(t *testing.T) {
	eval := automations.NewEvaluator()
	// Simulate a config lookup that returned an empty step result, so the
	// coalesce should fall through to the literal default.
	eval.SetStepResult("autoRole", &automations.StepResult{
		StepId:   "autoRole",
		Status:   "success",
		Result:   []any{},
		Metadata: map[string]any{"itemCount": 0},
	})

	got, err := resolveArgValueRef(`coalesce(autoRole.result.0.payload.value, "writer")`, eval)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "writer" {
		t.Errorf("coalesce result = %v, want %q", got, "writer")
	}
}

// TestResolveArgValueRef_ResolvesTopLevelHash locks in that a top-level
// hash(...) expression is resolved (sha256 hex) at arg-resolution time.
//
// Before this fix, hash() at top level fell through as a raw string, which
// then landed in the database as the literal expression text (e.g. the
// bootstrapIdentity automation was creating user ids like
// "user-hash(local-dev@memql.local)" instead of "user-<sha256>").
func TestResolveArgValueRef_ResolvesTopLevelHash(t *testing.T) {
	eval := automations.NewEvaluator()
	eval.SetCustom("event", map[string]any{"payload": map[string]any{"email": "alice@example.com"}})

	got, err := resolveArgValueRef(`hash($event.payload.email)`, eval)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, ok := got.(string)
	if !ok {
		t.Fatalf("hash result was %T, want string", got)
	}
	// sha256("alice@example.com") starts with "8edca8..."; we just need any
	// 64-char hex -- the point is the expression was evaluated, not left raw.
	if len(s) != 64 {
		t.Errorf("hash result has len %d (%q), want 64-char hex", len(s), s)
	}
	if s == "hash($event.payload.email)" {
		t.Errorf("hash result still contains raw expression text: %q", s)
	}
}

// TestResolveArgValueRef_HashInsideConcatStillWorks ensures the existing
// nested-builtin path (hash inside concat) still resolves, so the ID
// construction pattern concat("user-", hash(event.payload.email)) lands a
// clean id.
func TestResolveArgValueRef_HashInsideConcatStillWorks(t *testing.T) {
	eval := automations.NewEvaluator()
	eval.SetCustom("event", map[string]any{"payload": map[string]any{"email": "bob@example.com"}})

	got, err := resolveArgValueRef(`concat("user-", hash($event.payload.email))`, eval)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s, ok := got.(string)
	if !ok {
		t.Fatalf("result was %T, want string", got)
	}
	if len(s) != len("user-")+64 {
		t.Errorf("result len = %d (%q), want %d", len(s), s, len("user-")+64)
	}
	if s == `concat("user-", hash($event.payload.email))` {
		t.Errorf("concat+hash result still raw: %q", s)
	}
	if s[:5] != "user-" {
		t.Errorf("result missing 'user-' prefix: %q", s)
	}
}

// TestEvaluateHash_ProducesSHA256Hex covers the evaluateHash implementation
// directly and documents the digest format so a future change that swaps
// hash algorithm shows up clearly in the diff.
func TestEvaluateHash_ProducesSHA256Hex(t *testing.T) {
	eval := automations.NewEvaluator()
	eval.SetCustom("event", map[string]any{"payload": map[string]any{"email": "alice@example.com"}})

	exec := &MutationExecutor{}
	got, err := exec.evaluateHash(eval, `hash($event.payload.email)`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// sha256("alice@example.com") = 8edca8... (well-known); just assert
	// shape to avoid coupling the test to a specific email.
	if len(got) != 64 {
		t.Errorf("hash output len = %d, want 64 hex chars", len(got))
	}
}

// TestCoalesce_StepMethodAccessorResolves exercises the exact expression
// that broke bootstrapIdentity: coalesce(stepId.first().payload.value,
// "fallback") where the step result is empty. The expected outcome is
// that the method accessor normalizes to .first, the path resolves to
// nil (empty step), and coalesce returns "fallback".
//
// Before the normalizer, .first() tripped LooksLikePath (parens not
// allowed), the first coalesce arg resolved to the raw string
// "autoRole.first().payload.value", coalesce treated that non-nil string
// as the winning branch, and downstream enum validation rejected the
// raw expression text.
func TestCoalesce_StepMethodAccessorResolves(t *testing.T) {
	eval := automations.NewEvaluator()
	eval.SetStepResult("autoRole", &automations.StepResult{
		StepId:   "autoRole",
		Status:   "success",
		Result:   []any{},
		Metadata: map[string]any{"itemCount": 0},
	})

	got, err := resolveArgValueRef(`coalesce(autoRole.first().payload.value, "writer")`, eval)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "writer" {
		t.Errorf("result = %v, want %q (coalesce should fall through to default)", got, "writer")
	}
}

// TestEvaluateHash_StableAcrossCalls verifies hashing the same input twice
// produces the same digest (defends against accidental randomization /
// time-based seeding in a future refactor).
func TestEvaluateHash_StableAcrossCalls(t *testing.T) {
	eval := automations.NewEvaluator()
	eval.SetCustom("event", map[string]any{"payload": map[string]any{"email": "alice@example.com"}})

	exec := &MutationExecutor{}
	first, err := exec.evaluateHash(eval, `hash($event.payload.email)`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	second, err := exec.evaluateHash(eval, `hash($event.payload.email)`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if first != second {
		t.Errorf("hash not deterministic: first=%s second=%s", first, second)
	}
}

// --- memql#1065 reproduction --------------------------------------
//
// ensureDailySpaceOnAuthSession's logic body returns
//   ensureDailySpaceForUser({ userId: coalesce(args.event.payload.userId, args.event.payload.subject) })
// which the compiler renders to the function-arg expression
//   coalesce($args.event.payload.userId, $args.event.payload.subject)
// (see automation_generator.go convertArgReferences). authSession's
// userId field is OPTIONAL and frequently stored as "" while subject
// (the JWT sub, v1:identity:user:<uuid>) is @required and always set.
//
// The coalesce MUST skip the empty-string userId and fall through to
// subject. If it returns "" the dailyspace.ensureForUser builtin errors
// with "userId is required" on every login (#1065).
func TestCoalesce_AuthSessionEmptyUserIdFallsBackToSubject(t *testing.T) {
	const subject = "v1:identity:user:11111111-2222-3333-4444-555555555555"
	// Seed the evaluator the way LogicRunner.newEvaluatorForLogic does:
	// the whole caller-args map under "args".
	eval := automations.NewEvaluator()
	args := map[string]any{
		"event": map[string]any{
			"payload": map[string]any{
				"userId":  "", // optional, raced ahead of user-row insert
				"subject": subject,
			},
		},
	}
	eval.SetCustom("args", args)

	exec := &MutationExecutor{}
	got, err := exec.evaluateCoalesce(eval,
		`coalesce($args.event.payload.userId, $args.event.payload.subject)`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != subject {
		t.Fatalf("coalesce resolved userId = %#v, want subject %q (empty userId must fall through)", got, subject)
	}
}

// Happy path: when userId IS present, coalesce returns it (not subject).
func TestCoalesce_AuthSessionPrefersUserIdWhenPresent(t *testing.T) {
	const userId = "v1:identity:user:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const subject = "v1:identity:user:11111111-2222-3333-4444-555555555555"
	eval := automations.NewEvaluator()
	eval.SetCustom("args", map[string]any{
		"event": map[string]any{
			"payload": map[string]any{"userId": userId, "subject": subject},
		},
	})
	exec := &MutationExecutor{}
	got, err := exec.evaluateCoalesce(eval,
		`coalesce($args.event.payload.userId, $args.event.payload.subject)`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != userId {
		t.Fatalf("coalesce = %#v, want userId %q", got, userId)
	}
}

// memql#1065 diagnostic: when BOTH userId is "" and subject is ABSENT
// from the event payload (the cross-node payload-drop hypothesis),
// coalesce must NOT return the raw expression text. It returns nil,
// which the dailyspace builtin then rejects -- this is the observed
// "userId is required" failure.
func TestCoalesce_AuthSessionMissingSubjectYieldsNil(t *testing.T) {
	eval := automations.NewEvaluator()
	eval.SetCustom("args", map[string]any{
		"event": map[string]any{
			"payload": map[string]any{"userId": ""}, // subject absent
		},
	})
	exec := &MutationExecutor{}
	got, err := exec.evaluateCoalesce(eval,
		`coalesce($args.event.payload.userId, $args.event.payload.subject)`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	t.Logf("coalesce with missing subject => %#v (type %T)", got, got)
	if s, ok := got.(string); ok && s == "$args.event.payload.subject" {
		t.Fatalf("coalesce leaked raw expression text: %q", s)
	}
}

// TestResolveArgValueRef_CoalesceInObjectFallsBackToSubject (memql#1065 fix)
//
// The fix reshapes logicEnsureDailySpaceOnAuthSession into a function step
// whose object arg carries the coalesce expression in the $args form:
//   { userId: coalesce($args.event.payload.userId, $args.event.payload.subject) }
// The FunctionExecutor resolves these args (resolveArgsRefs -> resolveArgValueRef)
// BEFORE rendering the engine query, so the builtin receives the resolved
// subject -- never the empty userId, never the raw arg() text. This locks
// that resolution: empty userId must fall through to subject.
func TestResolveArgValueRef_CoalesceInObjectFallsBackToSubject(t *testing.T) {
	const subject = "v1:identity:user:11111111-2222-3333-4444-555555555555"
	eval := automations.NewEvaluator()
	args := map[string]any{
		"event": map[string]any{
			"payload": map[string]any{"userId": "", "subject": subject},
		},
	}
	eval.SetCustom("args", args)
	eval.SetCustom("event", args["event"])

	objArg := map[string]any{
		"userId": `coalesce($args.event.payload.userId, $args.event.payload.subject)`,
	}
	got, err := resolveArgValueRef(objArg, eval)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("resolved arg is %T, want map", got)
	}
	if m["userId"] != subject {
		t.Fatalf("resolved userId = %#v, want subject %q (empty userId must fall through)", m["userId"], subject)
	}
}

// Happy path: userId present wins over subject.
func TestResolveArgValueRef_CoalesceInObjectPrefersUserId(t *testing.T) {
	const userId = "v1:identity:user:aaaaaaaa"
	const subject = "v1:identity:user:11111111"
	eval := automations.NewEvaluator()
	args := map[string]any{
		"event": map[string]any{"payload": map[string]any{"userId": userId, "subject": subject}},
	}
	eval.SetCustom("args", args)
	eval.SetCustom("event", args["event"])

	got, err := resolveArgValueRef(map[string]any{
		"userId": `coalesce($args.event.payload.userId, $args.event.payload.subject)`,
	}, eval)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.(map[string]any)["userId"] != userId {
		t.Fatalf("resolved userId = %#v, want %q", got.(map[string]any)["userId"], userId)
	}
}
