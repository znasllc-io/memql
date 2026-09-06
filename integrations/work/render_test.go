package work

import (
	"strings"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// render_test.go -- every call this package composes is handed to the REAL
// MemQL parser.
//
// # Why this test exists, stated precisely
//
// A recording fake accepts whatever string it is given, so the whole package
// can stay green while nothing is written. That is not a hypothetical: the
// legacy object-literal wrapper `name({...})` was REMOVED from the language
// (#2335, Story 9) and parser.go:6254 now refuses it outright -- for reads and
// writes alike. A renderer emitting that shape produces a call the engine
// cannot parse, every mutation is a no-op, and the only evidence is a log line.
//
// So the assertion here is not "the string looks right". It is that
// langparser -- the same parser the engine's Execute path runs -- ACCEPTS the
// call and recovers the arguments that were put in. That catches the removed
// wrapper, a mis-escaped string, a stray comma and a malformed nested object,
// none of which a fake can notice, and it needs no database.
//
// # The second class it catches: a nil rendered as `null`
//
// An optional `object` field given `null` fails the concept's type check and
// the engine refuses the WHOLE row. "The caller said nothing" and "the caller
// said null" must therefore not render the same way, so the cases below drive
// every renderer with its optional fields ABSENT and assert the argument is
// missing rather than null.

// parseCall runs one rendered call through the real parser and returns the
// named arguments it recovered.
func parseCall(t *testing.T, rendered string) map[string]any {
	t.Helper()
	// The renderers prefix `mutation ` / `query `; the parser's expression
	// entry point takes the call itself.
	call := rendered
	for _, prefix := range []string{"mutation ", "query "} {
		call = strings.TrimPrefix(call, prefix)
	}
	expr, err := langparser.ParseExpression(call)
	if err != nil {
		t.Fatalf("the real parser REFUSED a call this package composes:\n\t%s\n%v", call, err)
	}
	fn, ok := expr.(*langparser.FunctionCallExpr)
	if !ok {
		t.Fatalf("call %q parsed as %T, not a function call", call, expr)
	}
	return fn.Args
}

// TestEveryRenderedCallParses drives each renderer twice -- once with every
// optional field supplied, once with none -- and parses both.
func TestEveryRenderedCallParses(t *testing.T) {
	at := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		// render composes the call the way the store does.
		render func() string
		// wantArgs must be present with these values.
		wantArgs map[string]any
		// wantAbsent must not appear at all -- not as null, not as {}.
		wantAbsent []string
	}{
		{
			name: "createWorkGoal, everything supplied",
			render: func() string {
				return "mutation " + call("createWorkGoal", map[string]any{
					"goalId":       "v1:work:goal:g1",
					"statement":    `reconcile "September" — 100% of it`,
					"origin":       "user",
					"accountIds":   optStrings([]string{"v1:accounts:account:acme"}),
					"input":        optMap(map[string]any{"month": "2026-09", "nested": map[string]any{"a": 1}}),
					"ceilings":     optMap(map[string]any{"maxModelCalls": 12}),
					"requestedVia": "nexus",
				})
			},
			wantArgs: map[string]any{"goalId": "v1:work:goal:g1", "origin": "user", "requestedVia": "nexus"},
		},
		{
			name: "createWorkGoal, every optional ABSENT",
			render: func() string {
				return "mutation " + call("createWorkGoal", map[string]any{
					"goalId":     "v1:work:goal:g1",
					"statement":  "x",
					"origin":     "user",
					"accountIds": optStrings(nil),
					"input":      optMap(nil),
					"ceilings":   optMap(nil),
				})
			},
			wantArgs:   map[string]any{"goalId": "v1:work:goal:g1"},
			wantAbsent: []string{"input", "ceilings", "accountIds"},
		},
		{
			name: "createWorkRun",
			render: func() string {
				return "mutation " + call("createWorkRun", map[string]any{
					"runId": "v1:work:run:r1", "goalId": "v1:work:goal:g1",
					"automationName": compilingAutomationName, "templateFingerprint": "",
					"input": optMap(nil), "mode": modeLive, "status": runStatusCompiling,
					"nodeId": "agent-1", "startedAt": rfc(at),
				})
			},
			wantArgs:   map[string]any{"runId": "v1:work:run:r1", "mode": modeLive, "status": runStatusCompiling},
			wantAbsent: []string{"input"},
		},
		{
			name: "updateWorkRun clearing a wait -- an EXPLICIT empty object survives",
			render: func() string {
				return "mutation " + call("updateWorkRun", map[string]any{
					"runId":     "v1:work:run:r1",
					"status":    runStatusRunning,
					"waitingOn": map[string]any{},
				})
			},
			wantArgs: map[string]any{"runId": "v1:work:run:r1", "status": runStatusRunning},
		},
		{
			name: "createWorkApproval",
			render: func() string {
				return "mutation " + call("createWorkApproval", map[string]any{
					"approvalId": "v1:work:approval:a1", "runId": "v1:work:run:r1", "stepKey": "s1",
					"kind": "sideEffect", "artifactHash": "deadbeef",
					"subject":     optMap(map[string]any{"command": `rm -rf "/tmp/x"`}),
					"options":     optMaps(nil),
					"evidence":    optMap(map[string]any{"tier": "high", "ruleId": "shell.destructive"}),
					"requestedAt": rfc(at), "expiresAt": rfcOrEmpty(time.Time{}),
				})
			},
			wantArgs:   map[string]any{"approvalId": "v1:work:approval:a1", "artifactHash": "deadbeef"},
			wantAbsent: []string{"options"},
		},
		{
			name: "decideWorkApproval with no answer",
			render: func() string {
				return "mutation " + call("decideWorkApproval", map[string]any{
					"approvalId": "v1:work:approval:a1", "decision": "approved",
					"decidedBy": "u-alice", "decidedAt": rfc(at), "answer": optMap(nil),
				})
			},
			wantArgs:   map[string]any{"decision": "approved", "decidedBy": "u-alice"},
			wantAbsent: []string{"answer"},
		},
		{
			name: "an owner-scoped read",
			render: func() string {
				return "query " + call("workRunForOwner", map[string]any{"runId": "v1:work:run:r1"})
			},
			wantArgs: map[string]any{"runId": "v1:work:run:r1"},
		},
		{
			name: "a read with no arguments at all",
			render: func() string {
				return "query workApprovalsForOwner()"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rendered := tc.render()
			// The removed wrapper, named so a regression says what it is.
			if strings.Contains(rendered, "({") {
				t.Fatalf("the call uses the REMOVED object-literal wrapper, which the parser refuses outright:\n\t%s", rendered)
			}
			args := parseCall(t, rendered)
			for k, want := range tc.wantArgs {
				got, present := args[k]
				if !present {
					t.Errorf("argument %q did not survive the parse of\n\t%s", k, rendered)
					continue
				}
				if got != want {
					t.Errorf("argument %q parsed as %#v, want %#v", k, got, want)
				}
			}
			for _, k := range tc.wantAbsent {
				if v, present := args[k]; present {
					t.Errorf("optional argument %q was rendered as %#v; an absent optional must be ABSENT -- null fails the concept's type check and the engine refuses the whole row, and {} is a clear the caller did not ask for", k, v)
				}
			}
		})
	}
}

// TestTheParserRefusesTheRemovedWrapper is this file's NEGATIVE CONTROL.
//
// A gate is only evidence if it fails when the thing it guards is broken, and
// it must fail for the RIGHT reason. So the removed object-literal form is
// composed here deliberately and the parser is asked about it: if this ever
// stops erroring, TestEveryRenderedCallParses above has stopped proving
// anything, and the error text is checked so a future failure for some
// unrelated syntax reason does not read as this one.
func TestTheParserRefusesTheRemovedWrapper(t *testing.T) {
	broken := `createWorkGoal({"goalId": "v1:work:goal:g1", "statement": "x"})`
	_, err := langparser.ParseExpression(broken)
	if err == nil {
		t.Fatal("the parser ACCEPTED the removed object-literal wrapper, so this file's parse assertions prove nothing")
	}
	if !strings.Contains(err.Error(), "object-literal call args are removed") {
		t.Errorf("the parser refused the wrapper for some other reason, so this control is not pinning what it claims:\n\t%v", err)
	}
}

// TestNothingRendersNull is the blunt form of the same rule over every
// renderer, because `null` is the spelling that costs a row rather than a
// field.
func TestNothingRendersNull(t *testing.T) {
	rendered := []string{
		call("createWorkGoal", map[string]any{"goalId": "g", "input": optMap(nil), "ceilings": optMap(nil), "accountIds": optStrings(nil)}),
		call("createWorkRun", map[string]any{"runId": "r", "input": optMap(nil)}),
		call("createWorkApproval", map[string]any{"approvalId": "a", "subject": optMap(nil), "options": optMaps(nil), "evidence": optMap(nil)}),
		call("decideWorkApproval", map[string]any{"approvalId": "a", "answer": optMap(nil)}),
	}
	for _, r := range rendered {
		if strings.Contains(r, "null") {
			t.Errorf("a rendered call carries null:\n\t%s", r)
		}
	}
}

// TestStringsAreQuotedByTheLexersOwnGrammar.
//
// langparser.QuoteString lives beside the lexer whose escape set it targets;
// Go's %q is a DIFFERENT grammar and emits \x00, \a, \v and \xNN, every one of
// which readString rejects -- so one such byte makes the whole statement
// unparseable. A goal statement is a person's free text, so this is not a
// theoretical difference.
func TestStringsAreQuotedByTheLexersOwnGrammar(t *testing.T) {
	nasty := "tab\there, quote\" here, newline\nhere, unicode ünïcödé, emoji-free"
	rendered := call("createWorkGoal", map[string]any{"goalId": "g", "statement": nasty})
	args := parseCall(t, rendered)
	if got := args["statement"]; got != nasty {
		t.Errorf("the statement did not survive quoting + parsing:\n got  %q\n want %q", got, nasty)
	}
}
