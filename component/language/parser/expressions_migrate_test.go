package parser

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// The expressions codemod (memql#5368). Each case is the ONE text a legacy
// clause must become; the structural and truth-table harnesses further down
// check that the text means what the old clause meant.

// xmtPreds is the predicate set the clause cases resolve against: the traits
// and specs the corpus uses, two of them over the @actor envelope.
var xmtPreds = map[string]PredicateInfo{
	"isActiveRecord":        {},
	"isNotDeleted":          {},
	"isNotArchived":         {},
	"statusIsActive":        {},
	"statusIsPending":       {},
	"identityIsWorkerToken": {},
	"requiresOwner":         {Actor: true},
	"forgeDeveloper":        {Actor: true},
}

func xmtRewrite(t *testing.T, src string, preds map[string]PredicateInfo) string {
	t.Helper()
	out, err := RewriteExpressions([]byte(src), preds)
	if err != nil {
		t.Fatalf("RewriteExpressions: %v\nsource:\n%s", err, src)
	}
	return string(out)
}

// xmtQuery wraps clause lines in a struct-form query.
func xmtQuery(clause string) string {
	return "query thing things {\n  args {\n    filter string\n  }\n" + clause + "\n  paginate 20\n}\n"
}

// xmtClauses is every conjunct shape of the corpus inventory, as the filter
// line an author wrote and the line the rewrite writes.
var xmtClauses = []struct{ name, in, want string }{
	{"field against an arg",
		`  filter  status==args.status`,
		`  filter  row => row.status == args.status`},
	{"actor flag",
		`  filter    actor.isClusterOwner==true`,
		`  filter    row => actor.isClusterOwner == true`},
	{"bare trait conjunct",
		`  filter  isActiveRecord`,
		`  filter  row => isActiveRecord(row)`},
	{"owner or cluster owner",
		`  filter  (ownerUserId==actor.userId || actor.isClusterOwner==true)`,
		`  filter  row => row.ownerUserId == actor.userId || actor.isClusterOwner == true`},
	{"guard alone at the top drops its parentheses",
		`  filter   when(args.since) { row.createdAt >= args.since }`,
		`  filter   row => args.since == nil || row.createdAt >= args.since`},
	{"guard under &&",
		`  filter  when(args.todoId) { row.id==args.todoId } && ownerUserId==actor.userId`,
		`  filter  row => (args.todoId == nil || row.id == args.todoId) && row.ownerUserId == actor.userId`},
	{"guards only",
		`  filter  when(args.a) { a==args.a } && when(args.b) { b==args.b }`,
		`  filter  row => (args.a == nil || row.a == args.a) && (args.b == nil || row.b == args.b)`},
	{"row intrinsic",
		`  filter  row.id == args.requestId`,
		`  filter  row => row.id == args.requestId`},
	{"nested path after a trait",
		`  filter  identityIsWorkerToken && credentials.keyHash==args.keyHash`,
		`  filter  row => identityIsWorkerToken(row) && row.credentials.keyHash == args.keyHash`},
	{"not blank",
		`  filter  domainId!=""`,
		`  filter  row => row.domainId != ""`},
	{"blank",
		`  filter  ownerUserId==actor.userId && decision==""`,
		`  filter  row => row.ownerUserId == actor.userId && row.decision == ""`},
	{"null-safe not-true",
		`  filter  ownerUserId==actor.userId && archived!=true`,
		`  filter  row => row.ownerUserId == actor.userId && row.archived != true`},
	{"field in a list",
		`  filter  status in ["superseded", "failed", "rolled_back"]`,
		`  filter  row => row.status in ["superseded", "failed", "rolled_back"]`},
	{"arg in a collection field keeps its order",
		`  filter  ownerUserId==actor.userId && when(args.tag) { args.tag in tags }`,
		`  filter  row => row.ownerUserId == actor.userId && (args.tag == nil || args.tag in row.tags)`},
	{"field in an arg list",
		`  filter  name in args.names && isActiveRecord`,
		`  filter  row => row.name in args.names && isActiveRecord(row)`},
	{"intrinsic ordered against an arg",
		`  filter  statusIsPending && when(args.createdBefore) { row.createdAt<args.createdBefore }`,
		`  filter  row => statusIsPending(row) && (args.createdBefore == nil || row.createdAt < args.createdBefore)`},
	{"now is the clock",
		`  filter  userId==actor.userId && revokedAt=="" && expiresAt>now`,
		`  filter  row => row.userId == actor.userId && row.revokedAt == "" && row.expiresAt > now`},
	{"list membership under ||",
		`  filter  status in ["pending", "failed"] || archived==true`,
		`  filter  row => row.status in ["pending", "failed"] || row.archived == true`},
	{"a caller flag under ||",
		`  filter  isNotArchived || args.includeArchived==true`,
		`  filter  row => isNotArchived(row) || args.includeArchived == true`},
	{"startsWith",
		`  filter  codeReference startsWith args.prefixes`,
		`  filter  row => row.codeReference startsWith args.prefixes`},
	{"an unquoted canonical id is a string",
		`  filter  target==v1:crm:lead`,
		`  filter  row => row.target == "v1:crm:lead"`},
	{"an actor predicate is applied to actor",
		`  filter  status == "needs_validation" && forgeDeveloper`,
		`  filter  row => row.status == "needs_validation" && forgeDeveloper(actor)`},
	{"null is nil",
		`  filter  refreshCadenceDays != null`,
		`  filter  row => row.refreshCadenceDays != nil`},
	{"a bare intrinsic is the intrinsic",
		`  filter  createdBy==actor.userId`,
		`  filter  row => row.createdBy == actor.userId`},
	{"not in",
		`  filter  kind not in ["a", "b"]`,
		`  filter  row => !(row.kind in ["a", "b"])`},
	{"a bang the old surface refused is the negation",
		`  filter  !(status == "open")`,
		`  filter  row => !(row.status == "open")`},
	{"coalesce on the value side",
		`  filter  stage == args.stage ?? "draft"`,
		`  filter  row => row.stage == args.stage ?? "draft"`},
	{"canonicalId's bare concept name is written as a string",
		`  filter  campaignId == canonicalId(args.campaignId, campaign)`,
		`  filter  row => row.campaignId == canonicalId(args.campaignId, "campaign")`},
	{"the filter( spelling gets its own word",
		`  filter(status==args.status)`,
		`  filter row => row.status == args.status`},
	{"a trailing comment stays",
		`  filter  status==args.status // the only selector`,
		`  filter  row => row.status == args.status // the only selector`},
	{"an or-group under && with a guard, wrapped past 110 columns",
		`  filter    (ownerUserId==actor.userId || actor.isClusterOwner==true) && when(args.status) { status==args.status }`,
		"  filter    row => (row.ownerUserId == actor.userId || actor.isClusterOwner == true)\n" +
			"                && (args.status == nil || row.status == args.status)"},
	{"the guard under || (dsl/observability/queries.memql:53)",
		`  filter  bucket==args.bucket && windowStart>=args.windowStart && windowStart<args.windowEnd && (when(args.codeReference) { codeReference==args.codeReference } || codeReference startsWith args.prefixes)`,
		"  filter  row => row.bucket == args.bucket\n" +
			"              && row.windowStart >= args.windowStart\n" +
			"              && row.windowStart < args.windowEnd\n" +
			"              && ((args.codeReference != nil && row.codeReference == args.codeReference) || row.codeReference startsWith args.prefixes)"},
	{"the multi-line router clause (dsl/router/queries.memql:64)",
		"  filter   when(args.since) { row.createdAt >= args.since }\n" +
			"        && when(args.level) { level == args.level }\n" +
			"        && when(args.door) { door == args.door }\n" +
			"        && when(args.rule) { rule == args.rule }\n" +
			"        && when(args.outcome) { outcome == args.outcome }",
		"  filter   row => (args.since == nil || row.createdAt >= args.since)\n" +
			"               && (args.level == nil || row.level == args.level)\n" +
			"               && (args.door == nil || row.door == args.door)\n" +
			"               && (args.rule == nil || row.rule == args.rule)\n" +
			"               && (args.outcome == nil || row.outcome == args.outcome)"},
	{"the Shopify family, wrapped past 110 columns",
		`  filter    storeId==args.storeId && isNotDeleted && actor.isClusterOwner==true && when(args.since) { updatedAt>=args.since }`,
		"  filter    row => row.storeId == args.storeId\n" +
			"                && isNotDeleted(row)\n" +
			"                && actor.isClusterOwner == true\n" +
			"                && (args.since == nil || row.updatedAt >= args.since)"},
	{"a clause at 110 columns stays on one line",
		`  filter  storeId==args.storeId && gid==args.gid && actor.isClusterOwner==true`,
		`  filter  row => row.storeId == args.storeId && row.gid == args.gid && actor.isClusterOwner == true`},
}

func TestRewriteExpressions_Filters(t *testing.T) {
	for _, tc := range xmtClauses {
		t.Run(tc.name, func(t *testing.T) {
			got := xmtRewrite(t, xmtQuery(tc.in), xmtPreds)
			if want := xmtQuery(tc.want); got != want {
				t.Errorf("\n got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// A clause the rewrite cannot convert faithfully is refused: the file comes
// back unchanged and the error names the line, the query and the clause.
func TestRewriteExpressions_RefusesWhatItCannotConvert(t *testing.T) {
	cases := []struct{ name, clause, why string }{
		{"a bare name that is no predicate", `  filter  isNotAThing && status==args.status`, `"isNotAThing" is neither a spec nor a trait`},
		{"payload.<intrinsic> names the payload field", `  filter  payload.id == args.id`, "payload.id names the PAYLOAD field"},
		{"row.<field> is not an intrinsic", `  filter  row.status == args.status`, "row.status is not a row intrinsic"},
		{"a comment inside a multi-line clause", "  filter  a==args.a // first\n          && b==args.b", "a comment inside the clause"},
		{"a negated guard", `  filter  !when(args.x) { a==args.x }`, "no negation of a deletion"},
		{"a reserved head", `  filter  config.x == 1`, "config is a reserved engine name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(xmtQuery(tc.clause))
			out, err := RewriteExpressions(src, xmtPreds)
			if err == nil {
				t.Fatalf("want a refusal (%s), got:\n%s", tc.why, out)
			}
			if !strings.Contains(err.Error(), tc.why) || !strings.Contains(err.Error(), "line 5:") || !strings.Contains(err.Error(), "query things") {
				t.Errorf("refusal %q does not name the line, the query and the reason %q", err, tc.why)
			}
			if string(out) != string(src) {
				t.Errorf("a refused file must come back unchanged, got:\n%s", out)
			}
		})
	}
}

func TestRewriteExpressions_SpecsAndTraits(t *testing.T) {
	src := `use common.shapes.{ actorEnvelope }

/// Row metadata, projected for a spec to read by key.
@row
shape participant participantRow {
  row.createdBy
  participantType
}

@actor
shape actorEnvelope {
  actor.userId
  actor.role
  actor.isClusterOwner
}

/// A spec over a concept.
@description("Matches sendable recipients")
spec recipient isSendableRecipient {
  return subscriptionStatus=="subscribed"
}

/// A spec over an @row shape reads its keys through row.
spec participantRow isSeedParticipant {
  return createdBy == "seed" && participantType == "human"
}

/// A spec over the @actor envelope is applied to actor.
spec actorEnvelope requiresOwner {
  return role == "owner" || isClusterOwner == true
}

@enabled
trait isActiveRecord {
  return active==true
}

spec agent richPredicate {
  return role == "assistant"
    && (roleSlug == "it-support" || roleSlug == "customer-service")
    && kind != "system"
}
`
	want := `use common.shapes.{ actorEnvelope }

/// Row metadata, projected for a spec to read by key.
@row
shape participant participantRow {
  row.createdBy
  participantType
}

@actor
shape actorEnvelope {
  actor.userId
  actor.role
  actor.isClusterOwner
}

/// A spec over a concept.
@description("Matches sendable recipients")
spec recipient isSendableRecipient = row => row.subscriptionStatus == "subscribed"

/// A spec over an @row shape reads its keys through row.
spec participantRow isSeedParticipant = row => row.createdBy == "seed" && row.participantType == "human"

/// A spec over the @actor envelope is applied to actor.
spec actorEnvelope requiresOwner = actor => actor.role == "owner" || actor.isClusterOwner == true

@enabled
trait isActiveRecord = row => row.active == true

spec agent richPredicate = row => row.role == "assistant"
                               && (row.roleSlug == "it-support" || row.roleSlug == "customer-service")
                               && row.kind != "system"
`
	preds, err := CollectPredicates(map[string][]byte{"specs.memql": []byte(src)})
	if err != nil {
		t.Fatalf("CollectPredicates: %v", err)
	}
	for name, actor := range map[string]bool{"isSendableRecipient": false, "isSeedParticipant": false, "requiresOwner": true, "isActiveRecord": false, "richPredicate": false} {
		if info, ok := preds[name]; !ok || info.Actor != actor {
			t.Errorf("preds[%s] = %+v, %v; want Actor=%v", name, info, ok, actor)
		}
	}
	if got := xmtRewrite(t, src, preds); got != want {
		t.Errorf("\n got:\n%s\nwant:\n%s", got, want)
	}
}

// A spec the rewrite has no predicate entry for cannot pick its parameter.
func TestRewriteExpressions_SpecNotInThePredicateSet(t *testing.T) {
	src := "spec actorEnvelope requiresOwner {\n  return role == \"owner\"\n}\n"
	if _, err := RewriteExpressions([]byte(src), map[string]PredicateInfo{}); err == nil || !strings.Contains(err.Error(), "not in the predicate set") {
		t.Errorf("err = %v, want the spec named as missing from the predicate set", err)
	}
}

// The six trigger filters of dsl/, exactly.
func TestRewriteExpressions_TriggerFilters(t *testing.T) {
	cases := map[string]string{
		`@filter(payload.preferences.computerUseEnabled == false)`:                                             `@filter(row => row.preferences.computerUseEnabled == false)`,
		`@filter(payload.status != "running" && payload.status != "compiling" && payload.status != "waiting")`: `@filter(row => row.status != "running" && row.status != "compiling" && row.status != "waiting")`,
		`@filter(payload.status == "stored")`:                                                                  `@filter(row => row.status == "stored")`,
		`@filter(payload.kind == "file" && payload.archived == true)`:                                          `@filter(row => row.kind == "file" && row.archived == true)`,
		`@filter(payload.status == "archived")`:                                                                `@filter(row => row.status == "archived")`,
		`@filter(payload.naturalKeyValue!=null)`:                                                               `@filter(row => row.naturalKeyValue != nil)`,
		// Every other root keeps its name.
		`@filter(event.payload.kind == "x" && args.flag == true)`: `@filter(row => event.payload.kind == "x" && args.flag == true)`,
		// The quoted spelling holds the same expression.
		`@filter("payload.status == \"open\"")`: `@filter(row => row.status == "open")`,
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			src := "@trigger(event=\"node.updated\", concept=\"v1:x:y\", partition=\"*\")\n" + in + "\nautomation a {\n  step s {\n    logic l ( event )\n  }\n}\n"
			got := xmtRewrite(t, src, nil)
			if !strings.Contains(got, "\n"+want+"\n") {
				t.Errorf("\n got:\n%s\nwant the line %s", got, want)
			}
		})
	}
	// The terse header's filter (dsl/data/automations.memql:8).
	terse := "@filter(payload.naturalKeyValue!=null)\nautomation conflictDetection @trigger(event=\"node.created\", concept=\"v1:data:record\", partition=\"*\") => logic conflictDetection\n"
	if got, want := xmtRewrite(t, terse, nil), strings.Replace(terse, "payload.naturalKeyValue!=null", "row => row.naturalKeyValue != nil", 1); got != want {
		t.Errorf("terse automation:\n got %q\nwant %q", got, want)
	}
	// A filter carried as a @trigger argument is refused by name.
	if _, err := RewriteExpressions([]byte("@trigger(event=\"node.created\", filter=\"payload.a == 1\")\nautomation a {\n}\n"), nil); err == nil || !strings.Contains(err.Error(), "move it to its own @filter") {
		t.Errorf("err = %v, want the @trigger filter argument refused", err)
	}
}

func TestRewriteExpressions_InProcess(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"cond nested four deep in the else branch (dsl/workbench/logic.memql:34)",
			`    isTerminal := cond(st == "succeeded", true, cond(st == "failed", true, cond(st == "cancelled", true, cond(st == "abandoned", true, false))))`,
			`    isTerminal := st == "succeeded" ? true : st == "failed" ? true : st == "cancelled" ? true : st == "abandoned" ? true : false`},
		{"cond as a return value",
			`    return cond(args.event.payload.node.type == "bff", true, false)`,
			`    return args.event.payload.node.type == "bff" ? true : false`},
		{"cond in the then branch is bracketed",
			`    verdict := cond(gateRequired == true, cond(clear == true, "clear", "blocked"), "clear")`,
			`    verdict := gateRequired == true ? (clear == true ? "clear" : "blocked") : "clear"`},
		{"cond as an operand is bracketed",
			`    label := "run-" + cond(ok == true, "a", "b")`,
			`    label := "run-" + (ok == true ? "a" : "b")`},
		{"concat with coalescing arguments",
			`    x := concat("PT-", w.first().payload.value ?? "90", "S")`,
			`    x := "PT-" + (w.first().payload.value ?? "90") + "S"`},
		{"concat as a call argument",
			`    c := query expired( createdBefore: addDuration(now, concat("P-", windowDays.first().payload.value ?? "90", "D")) )`,
			`    c := query expired( createdBefore: addDuration(now, "P-" + (windowDays.first().payload.value ?? "90") + "D") )`},
		{"hash(concat(...)) on one line",
			`    id: hash(concat(hash(args.sendingIdentity), hash(args.domain)))`,
			`    id: hash(hash(args.sendingIdentity) + hash(args.domain))`},
		{"concat as a coalesce operand is bracketed, the inner one is not",
			`    id: args.id ?? concat("node-", hash(concat(hash(args.nodeType), hash(now))))`,
			`    id: args.id ?? ("node-" + hash(hash(args.nodeType) + hash(now)))`},
		{"hash(concat(...)) across lines keeps its lines (dsl/deployment/mutations.memql:287)",
			"      id: hash(concat(\n        shortId(args.deploymentId), \":\",\n        args.nodeType\n      ))",
			"      id: hash(\n        shortId(args.deploymentId) + \":\" +\n        args.nodeType\n      )"},
		{"a multi-line concat value keeps its lines and gains brackets (dsl/agents/automations.memql:82)",
			"      prompt: concat(\n        \"Produce: \",\n        args.goal,\n        \"\\nFormat: \",\n        args.format ?? \"markdown\"\n      )",
			"      prompt: (\n        \"Produce: \" +\n        args.goal +\n        \"\\nFormat: \" +\n        (args.format ?? \"markdown\")\n      )"},
		{"exists on a step condition (dsl/workbench/automations.memql:54)",
			"    if steps.terminal.result == true && exists(id) {\n      builtin workbenchTeardownDirectory ( runId: id )\n    }",
			"    if steps.terminal.result == true && id != nil {\n      builtin workbenchTeardownDirectory ( runId: id )\n    }"},
		{"exists on a payload path (dsl/cluster/automations.memql:101)",
			"    if steps.refreshInfra.result == true && exists(payload.identityProvider) {\n      mutation m ( x: 1 )\n    }",
			"    if steps.refreshInfra.result == true && payload.identityProvider != nil {\n      mutation m ( x: 1 )\n    }"},
		{"exists opening a forEach where clause",
			"    forEach item in xs.result where exists(item.payload.at) && item.payload.at < now {\n      logic l ( item )\n    }",
			"    forEach item in xs.result where item.payload.at != nil && item.payload.at < now {\n      logic l ( item )\n    }"},
		{"exists as a whole value",
			`    ok := exists(args.ownerUserId)`,
			`    ok := args.ownerUserId != nil`},
		{"exists under a tighter operator is bracketed",
			`    gone := !exists(args.x) || exists(args.y) == true`,
			`    gone := !(args.x != nil) || (args.y != nil) == true`},
		{"exists over a coalesce needs no brackets inside",
			`    set := exists(args.a ?? args.b)`,
			`    set := args.a ?? args.b != nil`},
		{"canonicalId's bare concept name is written as a string (dsl/campaigns/mutations.memql:431)",
			"      id: hash(concat(\n        hash(canonicalId(args.campaignId, campaign)),\n        hash(canonicalId(args.recipientId, recipient))\n      ))",
			"      id: hash(\n        hash(canonicalId(args.campaignId, \"campaign\")) +\n        hash(canonicalId(args.recipientId, \"recipient\"))\n      )"},
		{"a quoted canonicalId concept stays",
			`    x := canonicalId(args.id, "v1:campaigns:campaign")`,
			`    x := canonicalId(args.id, "v1:campaigns:campaign")`},
		{"null is nil",
			`    x := args.a ?? null`,
			`    x := args.a ?? nil`},
		{"a construct that shares a name, a method, and a longer name are not the forms",
			`    a := logic concat(x: 1) + args.list.concat(2) + myconcat(3)`,
			`    a := logic concat(x: 1) + args.list.concat(2) + myconcat(3)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "logic l {\n  body {\n" + tc.in + "\n  }\n}\n"
			want := "logic l {\n  body {\n" + tc.want + "\n  }\n}\n"
			if got := xmtRewrite(t, src, nil); got != want {
				t.Errorf("\n got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// `+` adds two numbers where concat joined their text, so a concat whose first
// two arguments are not certainly strings has no exact `+` spelling.
func TestRewriteExpressions_ConcatOfUntypedArgumentsIsRefused(t *testing.T) {
	src := []byte("logic l {\n  body {\n    return concat(args.a, args.b)\n  }\n}\n")
	out, err := RewriteExpressions(src, nil)
	if err == nil || !strings.Contains(err.Error(), "line 3:") || !strings.Contains(err.Error(), "toString") {
		t.Fatalf("err = %v, want a refusal on line 3 naming toString", err)
	}
	if string(out) != string(src) {
		t.Errorf("a refused file must come back unchanged")
	}
}

func TestRewriteExpressions_ToolHandlers(t *testing.T) {
	cases := map[string]string{
		`@handler(type="query", query="mutation completeTodo(todoId: \"$args.todoId\", payload: $args.payload)")`: `@handler(type="query", query="mutation completeTodo(todoId: args.todoId, payload: args.payload)")`,
		`@handler(type="query", query="paginate(query searchUsers(active: $args.active), $args.limit)")`:          `@handler(type="query", query="paginate(query searchUsers(active: args.active), args.limit)")`,
		// Only a query handler carries placeholders.
		`@handler(type="function", name="$args.x")`: `@handler(type="function", name="$args.x")`,
	}
	for in, want := range cases {
		src := in + "\n@executionTime(\"fast\")\ntool t {\n  x string\n}\n"
		if got := xmtRewrite(t, src, nil); !strings.HasPrefix(got, want+"\n") {
			t.Errorf("\n got %s\nwant %s", strings.SplitN(got, "\n", 2)[0], want)
		}
	}
	// A placeholder inside a string of the query was interpolated; v1 does
	// not interpolate, so it is refused rather than turned into literal text.
	embedded := "@handler(type=\"query\", query=\"query q(title: \\\"Re: $args.title\\\")\")\ntool t {\n  title string\n}\n"
	if _, err := RewriteExpressions([]byte(embedded), nil); err == nil || !strings.Contains(err.Error(), "does not interpolate") {
		t.Errorf("err = %v, want an embedded placeholder refused", err)
	}
}

// Everything outside the five positions -- and every mention of their words in
// comments and strings -- stays byte-identical, and an untouched file comes
// back as the input slice itself.
func TestRewriteExpressions_LeavesEverythingElseAlone(t *testing.T) {
	src := []byte(`// filter status==args.status -- a comment, not a clause
/* cond(a, b, c), concat(x, y), exists(z) and null */
concept note {
  /// Says filter, cond(, concat(, exists( and null.
  body string @description("concat(a, b) is prose; so are filter x==y, $args.x and null")
}

/// A logic body with the words only in a comment and a string.
logic describe {
  args {
    x any
  }
  body {
    // cond(p, a, b) in a comment
    msg := "cond(a, b, c) and concat(x) and exists(y) and null"
    return msg
  }
}

query note notes {
  filter  row => row.body != "" && isActiveRecord(row)
  sort    "row.createdAt", "desc"
}

spec note isLong = row => row.body != ""

@filter(row => row.status == "open")
automation onNote {
  step s {
    logic describe ( x: event )
  }
}
`)
	out, err := RewriteExpressions(src, xmtPreds)
	if err != nil {
		t.Fatalf("RewriteExpressions: %v", err)
	}
	if string(out) != string(src) {
		t.Fatalf("an untouched file changed:\n%s", out)
	}
	if &out[0] != &src[0] {
		t.Errorf("an untouched file must come back as the input slice itself")
	}
}

// The migration channel runs per release, so a second pass is a no-op and an
// already-migrated file returns the input slice unchanged.
func TestRewriteExpressions_Idempotent(t *testing.T) {
	src := xmtQuery(`  filter  when(args.status) { status==args.status } && isActiveRecord`) +
		"\nspec actorEnvelope requiresOwner {\n  return role == \"owner\"\n}\n" +
		"\n@filter(payload.status == \"archived\")\nautomation a {\n  step s {\n    if exists(id) {\n      logic l ( x: concat(\"a-\", hash(id)) )\n    }\n  }\n}\n" +
		"\n@handler(type=\"query\", query=\"query q(x: \\\"$args.x\\\")\")\ntool q {\n  x string\n}\n"
	once := xmtRewrite(t, src, xmtPreds)
	if once == src {
		t.Fatal("the fixture must change on the first pass")
	}
	twice, err := RewriteExpressions([]byte(once), xmtPreds)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if string(twice) != once {
		t.Errorf("not idempotent:\n once:\n%s\ntwice:\n%s", once, twice)
	}
	in := []byte(once)
	if again, _ := RewriteExpressions(in, xmtPreds); &again[0] != &in[0] {
		t.Errorf("an already-migrated file must come back as the input slice itself")
	}
}

func TestCollectPredicates_RefusesAnAmbiguousName(t *testing.T) {
	files := map[string][]byte{
		"a.memql": []byte("@actor\nshape env {\n  actor.role\n}\nspec env isBoss {\n  return role == \"owner\"\n}\n"),
		"b.memql": []byte("trait isBoss {\n  return boss == true\n}\n"),
	}
	_, err := CollectPredicates(files)
	if err == nil || !strings.Contains(err.Error(), "isBoss is an actor predicate at a.memql:5 and a row predicate at b.memql:1") {
		t.Errorf("err = %v, want the two declarations named", err)
	}
	// Both editions of a declaration are read.
	preds, err := CollectPredicates(map[string][]byte{
		"c.memql": []byte("@actor\nshape env {\n  actor.role\n}\nspec env isBoss = actor => actor.role == \"owner\"\ntrait isOn = row => row.on == true\n"),
	})
	if err != nil || !preds["isBoss"].Actor || preds["isOn"].Actor {
		t.Errorf("migrated declarations: %+v, %v", preds, err)
	}
}

// ----------------------------------------------------------------------------
// The structural harness: every converted clause, mapped back to the legacy
// tree shape by an independent reverse mapper, must equal the legacy parse of
// the original -- with each when-guard expanded to the form it takes under its
// connective. This catches a converter that loses a segment, flips an
// operator or misplaces a value; lowering-level verification lands with Lower.
// ----------------------------------------------------------------------------

func TestRewriteExpressions_ConversionRoundTripsToTheLegacyTree(t *testing.T) {
	for _, tc := range xmtClauses {
		clause := xmtClauseText(tc.in)
		t.Run(tc.name, func(t *testing.T) {
			legacy, err := ParseExpression(clause)
			if err != nil {
				t.Fatalf("legacy parse: %v", err)
			}
			v1, err := xmConverter{mode: xmFilter, param: "row", preds: xmtPreds}.convert(clause)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			want := xmtNormalize(legacy, true)
			got := xmtReverse(t, v1)
			if !reflect.DeepEqual(want, got) {
				t.Errorf("round trip drifted for %s:\n want %s\n  got %s", clause, xmtJSON(want), xmtJSON(got))
			}
		})
	}
}

// xmtClauseText returns the clause a filter line holds, joined as the engine
// joins a multi-line clause.
func xmtClauseText(lines string) string {
	var parts []string
	for _, l := range strings.Split(lines, "\n") {
		if i := strings.Index(l, "//"); i >= 0 {
			l = l[:i]
		}
		parts = append(parts, strings.TrimSpace(l))
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.Join(parts, " "), "filter"))
}

func xmtJSON(n any) string {
	b, err := json.Marshal(n)
	if err != nil {
		return fmt.Sprintf("%#v", n)
	}
	return string(b)
}

// xmtNormalize rewrites a legacy tree into the shape the reverse mapper
// produces: a bare row intrinsic gains its `row.` head, a field's Raw is its
// joined path, the kind-prefixed predicate flag is dropped, and each guard is
// expanded to its connective's form (andCtx: under && or at the top).
func xmtNormalize(n ast.ExpressionNode, andCtx bool) ast.ExpressionNode {
	switch e := n.(type) {
	case *ast.LogicalExpr:
		child := e.Op == ast.LogicalAnd
		return &ast.LogicalExpr{Op: e.Op, Left: xmtNormalize(e.Left, child), Right: xmtNormalize(e.Right, child)}
	case *ast.ConditionalFilterExpr:
		parts := append([]string{"args"}, strings.Split(e.ArgPath, ".")...)
		// The guarded expression keeps its guard's context: under && the
		// guard reads (absent || e) with e in its && form, under || it reads
		// (present && e) with e in its || form.
		test := &ast.ComparisonExpr{Field: ast.FieldReference{Raw: strings.Join(parts, "."), Parts: parts}}
		if andCtx {
			test.Operator = ast.OpMissing
			return &ast.LogicalExpr{Op: ast.LogicalOr, Left: test, Right: xmtNormalize(e.Filter, true)}
		}
		test.Operator = ast.OpNotMissing
		return &ast.LogicalExpr{Op: ast.LogicalAnd, Left: test, Right: xmtNormalize(e.Filter, false)}
	case *ast.ComparisonExpr:
		parts := append([]string(nil), e.Field.Parts...)
		if canon, ok := xmIntrinsic(parts[0]); ok {
			parts = append([]string{"row", canon}, parts[1:]...)
		}
		return &ast.ComparisonExpr{Field: ast.FieldReference{Raw: strings.Join(parts, "."), Parts: parts}, Operator: e.Operator, Value: e.Value}
	case *ast.SpecReferenceExpr:
		return &ast.SpecReferenceExpr{Name: e.Name}
	case *ast.NotExpr:
		return &ast.NotExpr{Target: xmtNormalize(e.Target, true)}
	}
	return n
}

// xmtReverse maps a converted v1 tree back to the legacy shape.
func xmtReverse(t *testing.T, n ast.ExpressionNode) ast.ExpressionNode {
	t.Helper()
	switch e := n.(type) {
	case *ast.ParenExpr:
		return xmtReverse(t, e.Inner)
	case *ast.CallExpr:
		if len(e.Args) != 1 {
			t.Fatalf("a predicate application takes one receiver: %s", ast.FormatExpr(e))
		}
		return &ast.SpecReferenceExpr{Name: e.Name}
	case *ast.UnaryExpr:
		if in, ok := e.Operand.(*ast.BinaryExpr); ok && in.Op == "in" {
			return xmtComparison(t, in.Left, ast.OpOut, in.Right)
		}
		return &ast.NotExpr{Target: xmtReverse(t, e.Operand)}
	case *ast.BinaryExpr:
		switch e.Op {
		case "&&":
			return &ast.LogicalExpr{Op: ast.LogicalAnd, Left: xmtReverse(t, e.Left), Right: xmtReverse(t, e.Right)}
		case "||":
			return &ast.LogicalExpr{Op: ast.LogicalOr, Left: xmtReverse(t, e.Left), Right: xmtReverse(t, e.Right)}
		case "in":
			// `<scalar> in row.<collection>` is the legacy parser's `has`.
			if root, _ := xmtPathOf(e.Right); root == "row" {
				return xmtComparison(t, e.Right, ast.OpHas, e.Left)
			}
			return xmtComparison(t, e.Left, ast.OpIn, e.Right)
		case "==", "!=":
			if _, isNil := e.Right.(*ast.NilExpr); isNil {
				op := ast.OpMissing
				if e.Op == "!=" {
					op = ast.OpNotMissing
				}
				return xmtComparison(t, e.Left, op, nil)
			}
		}
		return xmtComparison(t, e.Left, ast.ComparisonOperator(e.Op), e.Right)
	}
	t.Fatalf("no legacy shape for %T (%s)", n, ast.FormatExpr(n))
	return nil
}

func xmtComparison(t *testing.T, field ast.ExpressionNode, op ast.ComparisonOperator, value ast.ExpressionNode) *ast.ComparisonExpr {
	t.Helper()
	root, rest := xmtPathOf(field)
	var parts []string
	switch {
	case root == "row" && len(rest) > 0:
		if _, ok := xmIntrinsic(rest[0]); ok {
			parts = append([]string{"row"}, rest...)
		} else {
			parts = rest
		}
	case root == "actor" || root == "args":
		parts = append([]string{root}, rest...)
	default:
		t.Fatalf("the left side %s is not a row, actor or args path", ast.FormatExpr(field))
	}
	out := &ast.ComparisonExpr{Field: ast.FieldReference{Raw: strings.Join(parts, "."), Parts: parts}, Operator: op}
	if value != nil {
		out.Value = xmtValue(t, value)
	}
	return out
}

// xmtValue maps a v1 value back to what the legacy parser hands a comparison.
func xmtValue(t *testing.T, n ast.ExpressionNode) any {
	t.Helper()
	switch e := n.(type) {
	case *ast.LiteralExpr:
		return e.Value
	case *ast.ListExpr:
		out := []any{}
		for _, el := range e.Elems {
			out = append(out, xmtValue(t, el))
		}
		return out
	case *ast.IdentExpr:
		if e.Name == "now" {
			return &ast.TimestampExprFunc{}
		}
	case *ast.CallExpr:
		if e.Name == "canonicalId" && len(e.Args) == 2 {
			if concept, ok := e.Args[1].(*ast.LiteralExpr); ok {
				v, _ := xmtValue(t, e.Args[0]).(ast.ExpressionNode)
				return &ast.CanonicalIdExpr{Value: v, Concept: concept.Value.(string)}
			}
		}
	case *ast.BinaryExpr:
		if e.Op == "??" {
			var arms []ast.ExpressionNode
			var flatten func(ast.ExpressionNode)
			flatten = func(x ast.ExpressionNode) {
				if b, ok := x.(*ast.BinaryExpr); ok && b.Op == "??" {
					flatten(b.Left)
					flatten(b.Right)
					return
				}
				switch v := xmtValue(t, x).(type) {
				case ast.ExpressionNode:
					arms = append(arms, v)
				default:
					arms = append(arms, &ast.LiteralExpr{Value: v})
				}
			}
			flatten(e)
			return &ast.CoalesceExpr{Args: arms}
		}
	}
	if root, rest := xmtPathOf(n); root == "args" {
		return &ast.ArgRefExpr{Path: strings.Join(rest, ".")}
	} else if root == "actor" {
		return &ast.ArgRefExpr{Path: "actor." + strings.Join(rest, ".")}
	}
	t.Fatalf("no legacy value for %s", ast.FormatExpr(n))
	return nil
}

// xmtPathOf splits a member chain into its root identifier and field names.
func xmtPathOf(n ast.ExpressionNode) (string, []string) {
	var rest []string
	for {
		switch e := n.(type) {
		case *ast.MemberExpr:
			rest = append([]string{e.Field}, rest...)
			n = e.Object
		case *ast.IdentExpr:
			return e.Name, rest
		default:
			return "", nil
		}
	}
}

// ----------------------------------------------------------------------------
// The truth-table harness: the when-guard translation checked against the
// ENGINE's semantics rather than against a restatement of the rule. For every
// assignment of guard-argument presence and leaf truth, the legacy clause is
// evaluated the way expandExpressionWithArgs does it -- an absent guard is
// deleted with its connective, a connective that loses both operands is
// deleted whole, and a deleted root filters nothing -- and the converted v1
// clause is evaluated as the plain boolean it is. They must agree everywhere.
// ----------------------------------------------------------------------------

func TestRewriteExpressions_GuardsKeepTheEnginesDropSemantics(t *testing.T) {
	clauses := []string{
		// Every guard shape of the corpus.
		`when(args.since) { row.createdAt >= args.since } && when(args.level) { level == args.level } && when(args.door) { door == args.door } && when(args.rule) { rule == args.rule } && when(args.outcome) { outcome == args.outcome }`,
		`bucket==args.bucket && (when(args.codeReference) { codeReference==args.codeReference } || codeReference startsWith args.prefixes)`,
		`(ownerUserId==actor.userId || actor.isClusterOwner==true) && when(args.status) { status==args.status }`,
		`when(args.since) { row.createdAt >= args.since }`,
		`isActiveRecord && when(args.recordId) { recordId==args.recordId } && when(args.partitionId) { partitionId==args.partitionId }`,
		// The shapes the local rule alone would get wrong: every operand of a
		// connective droppable, in either context.
		`when(args.a) { a==args.a } || when(args.b) { b==args.b }`,
		`x==1 || (when(args.a) { a==args.a } && when(args.b) { b==args.b })`,
		`(when(args.a) { a==args.a } || when(args.b) { b==args.b }) && y==2`,
		`x==1 && (when(args.a) { a==args.a } || when(args.b) { b==args.b } || when(args.c) { c==args.c })`,
		`(when(args.a) { a==args.a } || when(args.b) { b==args.b }) || x==1`,
		// Guards inside guards.
		`when(args.a) { when(args.b) { b==args.b } }`,
		`when(args.a) { a==args.a || when(args.b) { b==args.b } } && x==1`,
		`when(args.a) { a==args.a && when(args.b) { b==args.b } } || x==1`,
	}
	for _, clause := range clauses {
		t.Run(clause, func(t *testing.T) {
			legacy, err := ParseExpression(clause)
			if err != nil {
				t.Fatalf("legacy parse: %v", err)
			}
			v1, err := xmConverter{mode: xmFilter, param: "row", preds: xmtPreds}.convert(clause)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			args, leaves := map[string]bool{}, map[string]bool{}
			xmtCollect(legacy, args, leaves)
			names := append(xmtKeys(args), xmtKeys(leaves)...)
			if len(names) > 16 {
				t.Fatalf("%d variables is too many to enumerate", len(names))
			}
			for bits := 0; bits < 1<<len(names); bits++ {
				env := map[string]bool{}
				for i, n := range names {
					env[n] = bits&(1<<i) != 0
				}
				dropped, v := xmtEvalLegacy(t, legacy, env)
				want := dropped || v
				if got := xmtEvalV1(t, v1, env); got != want {
					t.Fatalf("disagree at %v: legacy %v, v1 %v\nv1: %s", env, want, got, ast.FormatExpr(v1))
				}
			}
		})
	}
}

func xmtKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// xmtPresence reports whether a comparison is a presence test on an argument
// -- `args.x == nil` / `!= nil` -- and returns the argument's variable name.
func xmtPresence(c *ast.ComparisonExpr) (string, bool) {
	if len(c.Field.Parts) >= 2 && c.Field.Parts[0] == "args" && (c.Operator == ast.OpMissing || c.Operator == ast.OpNotMissing) {
		return "arg:" + strings.Join(c.Field.Parts[1:], "."), true
	}
	return "", false
}

func xmtLeafKey(n ast.ExpressionNode) string { return "leaf:" + xmtJSON(xmtNormalize(n, true)) }

func xmtCollect(n ast.ExpressionNode, args, leaves map[string]bool) {
	switch e := n.(type) {
	case *ast.LogicalExpr:
		xmtCollect(e.Left, args, leaves)
		xmtCollect(e.Right, args, leaves)
	case *ast.ConditionalFilterExpr:
		args["arg:"+e.ArgPath] = true
		xmtCollect(e.Filter, args, leaves)
	case *ast.ComparisonExpr:
		if a, ok := xmtPresence(e); ok {
			args[a] = true
			return
		}
		leaves[xmtLeafKey(e)] = true
	default:
		leaves[xmtLeafKey(e)] = true
	}
}

// xmtEvalLegacy evaluates a legacy clause with the engine's drop semantics.
func xmtEvalLegacy(t *testing.T, n ast.ExpressionNode, env map[string]bool) (dropped, value bool) {
	switch e := n.(type) {
	case *ast.LogicalExpr:
		ld, lv := xmtEvalLegacy(t, e.Left, env)
		rd, rv := xmtEvalLegacy(t, e.Right, env)
		switch {
		case ld && rd:
			return true, false
		case ld:
			return false, rv
		case rd:
			return false, lv
		case e.Op == ast.LogicalAnd:
			return false, lv && rv
		default:
			return false, lv || rv
		}
	case *ast.ConditionalFilterExpr:
		if !env["arg:"+e.ArgPath] {
			return true, false
		}
		return xmtEvalLegacy(t, e.Filter, env)
	case *ast.ComparisonExpr:
		if a, ok := xmtPresence(e); ok {
			return false, env[a] == (e.Operator == ast.OpNotMissing)
		}
	}
	return false, env[xmtLeafKey(n)]
}

// xmtEvalV1 evaluates a converted clause as a plain boolean.
func xmtEvalV1(t *testing.T, n ast.ExpressionNode, env map[string]bool) bool {
	switch e := n.(type) {
	case *ast.ParenExpr:
		return xmtEvalV1(t, e.Inner, env)
	case *ast.BinaryExpr:
		switch e.Op {
		case "&&":
			return xmtEvalV1(t, e.Left, env) && xmtEvalV1(t, e.Right, env)
		case "||":
			return xmtEvalV1(t, e.Left, env) || xmtEvalV1(t, e.Right, env)
		}
	}
	legacy := xmtReverse(t, n)
	if c, ok := legacy.(*ast.ComparisonExpr); ok {
		if a, ok := xmtPresence(c); ok {
			return env[a] == (c.Operator == ast.OpNotMissing)
		}
	}
	return env[xmtLeafKey(legacy)]
}
