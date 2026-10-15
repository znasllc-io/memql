package campaigns

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// delivery_id_test.go -- the drift gate on a derivation the DSL owns and Go
// re-implements (memql#4823).
//
// deliveryRowID reproduces recordCampaignDelivery's `id:` expression so a
// tracking token can name a delivery row that does not exist yet. Two
// implementations of one derivation is a maintenance hazard, and this one has
// the worst possible failure mode: if they diverge, every engagement event
// references a delivery row that is not there, the totals still tally, no
// error is raised anywhere, and the only symptom is that "which send did this
// open come from" stops having an answer.
//
// The send path cannot check itself -- the row it would compare against has
// not been written when the body is rendered. So the check is against the DSL
// SOURCE: parse the id expression out of dsl/campaigns/mutations.memql and
// assert it still has the shape the Go assumes. That is the one check
// available that is not a copy of the thing being checked.

// TestDeliveryIdDerivationMatchesTheMutation reads the actual mutation,
// asserts each element the Go depends on, and then EVALUATES it.
//
// It asserts on the SHAPE rather than on a byte-for-byte string, because the
// file is formatted and reflowed by tooling: what matters is that it is still
// hash(hash(requestId ?? canonicalId(campaignId ?? emailRuleId, "campaign")) +
// hash(canonicalId(recipientId, "recipient"))) in that ORDER, per-part hashed.
// That is the edition-2026 spelling the codemod wrote at the flip -- `+` for
// concat(), the concept names quoted -- and a spelling change is not a
// derivation change. So the assertion that matters is the second one: the
// expression as the file writes it, parsed and evaluated by the engine's
// evaluator (EvalExpr), derives the ids deliveryRowID and singleDeliveryRowID
// derive, and one of each is pinned as a literal so the two cannot move together.
func TestDeliveryIdDerivationMatchesTheMutation(t *testing.T) {
	src := campaignsDSL(t, "mutations.memql")
	block := mutationBlock(t, src, "mutation delivery recordCampaignDelivery")

	idExpr := collapsedExprAt(t, block, "id:")
	const want = `id: hash( hash(args.requestId ?? canonicalId(args.campaignId ?? args.emailRuleId ?? "", "campaign")) + ` +
		`hash(canonicalId(args.recipientId, "recipient")) )`
	if idExpr != want {
		t.Fatalf("recordCampaignDelivery's derived id has changed.\n got: %s\nwant: %s\n\n"+
			"component/campaigns/delivery_id.go re-implements this expression in Go so a tracking token "+
			"can name a delivery row before it is written. A divergence is SILENT: every "+
			"v1:campaigns:engagementEvent would reference a row that does not exist, the counts would "+
			"still tally, and nothing would raise an error. Update deliveryRowID and "+
			"singleDeliveryRowID to match, or stop deriving the id in Go.", idExpr, want)
	}

	derive := deliveryIDFromTheMutation(t)
	for _, pair := range [][2]string{
		{"camp-1", "rec-1"},
		{"v1:campaigns:campaign:camp-1", "v1:campaigns:recipient:rec-1"},
		{"camp-1", "v1:campaigns:recipient:rec-1"},
		{"a:b", "c"},
	} {
		got := derive(map[string]any{"campaignId": pair[0], "recipientId": pair[1]})
		if want := deliveryRowID(pair[0], pair[1]); got != want {
			t.Errorf("for (%q, %q) the mutation derives %s and deliveryRowID derives %s -- every "+
				"engagement event for this pair would reference a delivery row that does not exist",
				pair[0], pair[1], got, want)
		}
	}
	// Rule sends retain the original per-rule identity, including the no-rule
	// recipient-only case, while omitting the false campaign relationship.
	for _, ruleID := range []string{"rule-1", ""} {
		args := map[string]any{"recipientId": "rec-1"}
		if ruleID != "" {
			args["emailRuleId"] = ruleID
		}
		if got, want := derive(args), deliveryRowID(ruleID, "rec-1"); got != want {
			t.Errorf("rule %q id = %v, want existing ledger id %s", ruleID, got, want)
		}
	}
	// A send made under a request is keyed by the REQUEST (memql#6022, F7): the
	// request id stands in the id's first part, whatever campaign or rule the row
	// also names, so each message to a person has a row of its own.
	for _, c := range []struct{ request, rule, recipient string }{
		{"newsletter-abcdefgh", "", "rec-1"},
		{"journey-bbbbbbbb-step1", "", "rec-1"},
		{"journey-bbbbbbbb-step1", "rule-1", "rec-1"},
		{"newsletter-abcdefgh", "", "v1:campaigns:recipient:rec-1"},
	} {
		args := map[string]any{"requestId": c.request, "recipientId": c.recipient}
		if c.rule != "" {
			args["emailRuleId"] = c.rule
		}
		if got, want := derive(args), singleDeliveryRowID(c.request, c.recipient); got != want {
			t.Errorf("for request %q (rule %q) and recipient %q the mutation derives %s and singleDeliveryRowID derives %s",
				c.request, c.rule, c.recipient, got, want)
		}
	}
	// An empty or blank request is no request: the row keeps the key it always had.
	for _, blank := range []string{"", "   "} {
		args := map[string]any{"requestId": blank, "emailRuleId": "rule-1", "recipientId": "rec-1"}
		if got, want := derive(args), deliveryRowID("rule-1", "rec-1"); got != want {
			t.Errorf("a blank request id %q changed the row's key: %s, want %s", blank, got, want)
		}
	}
	const pinned = "9314634c0d8d2e9e189563b1974d63bc197561262375106aac19b00aeb615773"
	if got := deliveryRowID("camp-1", "rec-1"); got != pinned {
		t.Errorf("deliveryRowID(camp-1, rec-1) = %s, want the id the delivery rows already written carry, %s", got, pinned)
	}
	// Computed outside this package (sha256 of sha256("newsletter-abcdefgh") then
	// sha256("v1:campaigns:recipient:rec-1")), so the two derivations cannot drift
	// together.
	const pinnedSingle = "f050471c6208aae326b95e88789aaa908707842e0c548cab049d849479616bab"
	if got := singleDeliveryRowID("newsletter-abcdefgh", "rec-1"); got != pinnedSingle {
		t.Errorf("singleDeliveryRowID(newsletter-abcdefgh, rec-1) = %s, want %s", got, pinnedSingle)
	}
}

// deliveryIDFromTheMutation parses the id expression out of the DSL source of
// recordCampaignDelivery and returns the function that evaluates it over a call's
// arguments, with the engine's own evaluator: what the engine would stamp as the
// row's id for those arguments.
func deliveryIDFromTheMutation(t *testing.T) func(args map[string]any) string {
	t.Helper()
	block := mutationBlock(t, campaignsDSL(t, "mutations.memql"), "mutation delivery recordCampaignDelivery")
	idExpr := collapsedExprAt(t, block, "id:")
	expr, err := languageParser.ParseV1Expression(strings.TrimPrefix(idExpr, "id: "))
	if err != nil {
		t.Fatalf("the id expression does not parse: %v", err)
	}
	// canonicalId resolves its quoted short name to the concept the file
	// imports; the canonicalisation itself is the one deliveryRowID uses, so
	// what is compared is the derivation's STRUCTURE -- which parts are
	// hashed, in which order, joined how.
	concepts := map[string]string{"campaign": campaignConcept, "recipient": recipientConcept}
	opts := memql.EvalOptions{CanonicalID: func(_ context.Context, value any, concept string) (string, error) {
		c, ok := concepts[concept]
		if !ok {
			return "", fmt.Errorf("canonicalId names %q, which is neither campaign nor recipient", concept)
		}
		return canonicalIDFor(c, fmt.Sprint(value)), nil
	}}
	return func(args map[string]any) string {
		t.Helper()
		v, err := memql.EvalExpr(context.Background(), expr, memql.MapScope{"args": args}, opts)
		if err != nil {
			t.Fatalf("evaluating the id for %v: %v", args, err)
		}
		id, ok := v.(string)
		if !ok {
			t.Fatalf("the id for %v is %T, not text", args, v)
		}
		return id
	}
}

// TestARequestIdCannotNameACampaignsLedgerRow: the request id stands where a
// campaign's canonical id does, so what keeps a single send's row from aliasing a
// campaign's is that the two never have the same text. A canonical campaign id
// always carries a colon (v1:campaigns:campaign:<id>), or is empty; a request id
// never does (singleSendRequestID), and is never empty.
func TestARequestIdCannotNameACampaignsLedgerRow(t *testing.T) {
	for _, forged := range []string{
		"v1:campaigns:campaign:camp-1", "campaigns:campaign:camp-1", "a:bcdefgh", "newsletter:abcdefgh", "",
	} {
		if singleSendRequestID.MatchString(forged) {
			t.Errorf("%q is a valid request id; it could alias a campaign's ledger row", forged)
		}
	}
	for _, request := range []string{"newsletter-abcdefgh", "journey-bbbbbbbb-step1", "welcome_signup_once"} {
		if !singleSendRequestID.MatchString(request) {
			t.Fatalf("%q is not a valid request id; the test is not testing what it says", request)
		}
		if canonical := canonicalIDFor(campaignConcept, request); !strings.Contains(canonical, ":") || canonical == request {
			t.Errorf("a campaign id %q canonicalizes to %q, which is not distinguishable from a request id", request, canonical)
		}
		if got, other := singleDeliveryRowID(request, "rec-1"), deliveryRowID(request, "rec-1"); got == other {
			t.Errorf("request %q and campaign %q have the same ledger row %s", request, request, got)
		}
	}
}

// TestSingleDeliveryRowIdIsPerRequestAndPerRecipient: two requests to one person
// are two rows, one request to two people are two rows, and the id is the same hex
// sha256 shape as a campaign's.
func TestSingleDeliveryRowIdIsPerRequestAndPerRecipient(t *testing.T) {
	a := singleDeliveryRowID("newsletter-aaaaaaaa", "rec-1")
	if b := singleDeliveryRowID("journey-bbbbbbbb-step1", "rec-1"); a == b {
		t.Error("two requests to one person share a ledger row")
	}
	if b := singleDeliveryRowID("newsletter-aaaaaaaa", "rec-2"); a == b {
		t.Error("one request to two people shares a ledger row")
	}
	if b := singleDeliveryRowID("newsletter-aaaaaaaa", "v1:campaigns:recipient:rec-1"); a != b {
		t.Errorf("a bare recipient id and its canonical spelling derived different rows (%s vs %s)", a, b)
	}
	if len(a) != 64 {
		t.Errorf("the id is %d characters, want 64 (hex sha256)", len(a))
	}
}

// TestDeliveryRowIdIsPerPartHashed pins the property the derivation exists
// for: no separator inside a caller-supplied id can make two different
// (campaign, recipient) pairs land on one row. Concatenating the raw ids and
// hashing once would be one fewer operation and a genuine collision surface
// (authoring rule 20).
func TestDeliveryRowIdIsPerPartHashed(t *testing.T) {
	// Two pairs whose RAW concatenations are identical and whose per-part
	// hashes are not: "a:b" + "c" versus "a" + "b:c".
	first := deliveryRowID("a:b", "c")
	second := deliveryRowID("a", "b:c")
	if first == second {
		t.Error("two different (campaign, recipient) pairs derived the same delivery id. The parts are " +
			"hashed BEFORE concatenation precisely so no separator inside an id can forge a collision")
	}
}

func TestDeliveryRowIdIsStableAndCanonicalizing(t *testing.T) {
	bare := deliveryRowID("camp-1", "rec-1")
	canonical := deliveryRowID("v1:campaigns:campaign:camp-1", "v1:campaigns:recipient:rec-1")
	if bare != canonical {
		t.Errorf("a bare id and its canonical spelling derived different delivery ids (%s vs %s). "+
			"canonicalId() normalizes both to the tagged form, so the Go must too -- the worker holds "+
			"bare ids and the mutation is handed the same ones", bare, canonical)
	}
	if len(bare) != 64 {
		t.Errorf("delivery id is %d characters, want 64 (hex sha256) -- hash() is hex(sha256(x))", len(bare))
	}
	if deliveryRowID("camp-1", "rec-1") != bare {
		t.Error("the derivation is not deterministic")
	}
}

// --- source helpers -----------------------------------------------------

// mutationBlock returns one mutation's source text.
func mutationBlock(t *testing.T, src, header string) string {
	t.Helper()
	i := strings.Index(src, header)
	if i < 0 {
		t.Fatalf("%q is not in dsl/campaigns/mutations.memql -- it was renamed or removed, and the Go "+
			"that re-implements its derived id is now pointing at nothing", header)
	}
	rest := src[i+len(header):]
	if end := strings.Index(rest, "\nmutation "); end > 0 {
		rest = rest[:end]
	}
	return rest
}

var whitespaceRun = regexp.MustCompile(`\s+`)

// collapsedExprAt extracts the line beginning with `marker` and everything
// up to its balanced close, with runs of whitespace collapsed to one space so
// the assertion survives re-indentation.
func collapsedExprAt(t *testing.T, block, marker string) string {
	t.Helper()
	i := strings.Index(block, marker)
	if i < 0 {
		t.Fatalf("no %q in the mutation block", marker)
	}
	rest := block[i:]
	depth := 0
	end := len(rest)
	for j, ch := range rest {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				end = j + 1
			}
		case '\n':
			if depth == 0 {
				end = j
			}
		}
		if end != len(rest) {
			break
		}
	}
	return strings.TrimSpace(whitespaceRun.ReplaceAllString(rest[:end], " "))
}
