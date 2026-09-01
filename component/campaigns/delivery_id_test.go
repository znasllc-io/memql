package campaigns

import (
	"regexp"
	"strings"
	"testing"
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

// TestDeliveryIdDerivationMatchesTheMutation reads the actual mutation and
// asserts each element the Go depends on.
//
// It asserts on the SHAPE rather than on a byte-for-byte string, because the
// file is formatted and reflowed by tooling: what matters is that it is still
// hash(concat(hash(canonicalId(campaignId, campaign)),
// hash(canonicalId(recipientId, recipient)))) in that ORDER, per-part hashed.
func TestDeliveryIdDerivationMatchesTheMutation(t *testing.T) {
	src := campaignsDSL(t, "mutations.memql")
	block := mutationBlock(t, src, "mutate delivery recordCampaignDelivery")

	idExpr := collapsedExprAt(t, block, "id:")
	const want = "id: hash(concat( hash(canonicalId(args.campaignId, campaign)), " +
		"hash(canonicalId(args.recipientId, recipient)) ))"
	if idExpr != want {
		t.Fatalf("recordCampaignDelivery's derived id has changed.\n got: %s\nwant: %s\n\n"+
			"component/campaigns/delivery_id.go re-implements this expression in Go so a tracking token "+
			"can name a delivery row before it is written. A divergence is SILENT: every "+
			"v1:campaigns:engagementEvent would reference a row that does not exist, the counts would "+
			"still tally, and nothing would raise an error. Update deliveryRowID to match, or stop "+
			"deriving the id in Go.", idExpr, want)
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
	if end := strings.Index(rest, "\nmutate "); end > 0 {
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
