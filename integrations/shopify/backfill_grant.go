package shopify

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// These order selections are the exact non-statistical omissions qualified by
// the live statistics projection (mirror_live_test.go). Bulk completion must
// not inherit ordinary reads' unrestricted pruning: losing an amount, a flag
// or a child connection can still produce plausible, incorrect sales figures.
// Unknown fields and every other domain therefore keep their refusal. Widening
// this set requires reviewing the corresponding history consumer, not merely
// observing that Shopify accepts a smaller query.
var bulkOptionalOrderFields = map[string]bool{
	"contract": true, "fulfillmentOrders": true, "paymentTerms": true,
	"staffMember": true, "user": true, "vaultedPaymentMethods": true,
}

func bulkDocumentWithGrant(document string, spec *generated.TypeSpec, denied []string) (string, []string, error) {
	var omitted []string
	for _, field := range denied {
		// A learned FetchDocument denial may not be selected in this bulk
		// operation at all. It neither reduces this export nor blocks it.
		if !bulkSelects(document, field) {
			continue
		}
		if spec.Concept != "order" || !bulkOptionalOrderFields[field] {
			return "", nil, fmt.Errorf("%w: shopify: bulk %s cannot omit denied selection %s without losing history coverage", memqlsync.ErrNotGranted, spec.Concept, field)
		}
		before := document
		if field == "vaultedPaymentMethods" {
			// The generator nests this reference on a single line beside
			// additionalPaymentCollectionUrl. Remove just the qualified
			// reference, preserving its parent and sibling. A changed shape
			// refuses below instead of guessing at GraphQL structure.
			document = strings.ReplaceAll(document,
				"paymentCollectionDetails { additionalPaymentCollectionUrl vaultedPaymentMethods { id } }",
				"paymentCollectionDetails { additionalPaymentCollectionUrl }")
		} else {
			document = pruneSelections(document, []string{field})
		}
		if document == before || bulkSelects(document, field) {
			return "", nil, memqlsync.Permanentf("shopify: generated bulk selection %s cannot be safely omitted", field)
		}
		omitted = append(omitted, field)
	}
	return document, uniqueSorted(omitted), nil
}

func bulkSelects(document, field string) bool {
	return regexp.MustCompile(`(?:^|[\s{])` + regexp.QuoteMeta(field) + `(?:[\s({]|$)`).MatchString(document)
}

// A bulk-query permission failure is normally a userError inside the mutation
// result. Its field path is [query], not the denied inner selection. Require
// every message to name one qualified field; mixed/unknown errors, root
// refusals and outer GraphQL failures are never guessed into a retry.
func optionalBulkDenial(err error, spec *generated.TypeSpec) []string {
	var ue *UserError
	if spec.Concept != "order" || !errors.As(err, &ue) || ue.Field != "bulkOperationRunQuery" || len(ue.Messages) == 0 {
		return nil
	}
	var fields []string
	for _, message := range ue.Messages {
		message = strings.TrimPrefix(message, "query: ")
		matches := deniedFieldRe.FindAllStringSubmatch(message, -1)
		if !strings.HasPrefix(message, "Access denied for ") || len(matches) != 1 || !bulkOptionalOrderFields[matches[0][1]] {
			return nil
		}
		fields = append(fields, matches[0][1])
	}
	return uniqueSorted(fields)
}

// bulkFieldsDenied merges the process's learned refusals with the exclusions
// recorded by completed or in-flight exports. The rows are already scoped to
// this store by exact sync-state ids; no process cache is mutated by a read.
func bulkFieldsDenied(learned map[string][]string, states []map[string]any) (map[string][]string, error) {
	for _, state := range states {
		concept := mapString(state, "conceptId")
		omitted, err := bulkCursorOmissions(generated.ConceptFromID(concept), mapString(state, "backfillCursor"))
		if err != nil {
			return nil, err
		}
		if len(omitted) != 0 {
			learned[concept] = uniqueSorted(append(learned[concept], omitted...))
		}
	}
	return learned, nil
}

// Cursor exclusions have one bounded, reviewed vocabulary. An incompatible
// or malformed record must not become an apparent full-coverage history on
// a fresh replica. Other cursor fields retain their existing decoding rules.
func bulkCursorOmissions(concept, cursor string) ([]string, error) {
	if strings.TrimSpace(cursor) == "" {
		return nil, nil
	}
	var saved struct {
		Omitted string `json:"omitted"`
	}
	if err := json.Unmarshal([]byte(cursor), &saved); err != nil {
		return nil, memqlsync.Permanentf("shopify: bulk selection coverage could not be read from its cursor")
	}
	if saved.Omitted == "" {
		return nil, nil
	}
	if concept != "order" || len(saved.Omitted) > 128 {
		return nil, memqlsync.Permanentf("shopify: bulk selection coverage is incompatible with its domain")
	}
	fields := strings.Split(saved.Omitted, ",")
	if len(fields) > len(bulkOptionalOrderFields) {
		return nil, memqlsync.Permanentf("shopify: bulk selection coverage exceeds its field bound")
	}
	seen := map[string]bool{}
	for _, field := range fields {
		if !bulkOptionalOrderFields[field] || seen[field] {
			return nil, memqlsync.Permanentf("shopify: bulk selection coverage contains an unqualified or repeated field")
		}
		seen[field] = true
	}
	return fields, nil
}
