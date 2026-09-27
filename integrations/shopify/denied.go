package shopify

import (
	"context"
	"errors"
	"fmt"
	"strings"

	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// denied.go -- asking only for what the grant covers.
//
// A store's OAuth grant is narrower than the mirror's selection whenever an
// operator connected it with fewer scopes than the runbook lists, and
// Shopify answers a field the grant does not cover with ACCESS_DENIED. That
// would be survivable if it only nulled the field, but many such fields are
// non-null in the schema (a variant's availablePublicationsCount), and
// GraphQL's null propagation then blanks the node, the list, and the page:
// one uncovered count on a variant left every variant unmirrored.
//
// Nothing in the introspected schema says which scope a field needs -- the
// recorded 2026-07 schema carries no access-scope descriptions -- so the
// runtime learns it the only way it can: from the refusal. The field is
// recorded for the store and domain, removed from that store's copy of the
// document, and the request is made again. The rows land without the field,
// the store's health names what was left out (fieldsDenied), and the next
// sweep and the next webhook fetch start from the pruned document.

// maxDeniedPrunes bounds how many times one call re-asks after learning a
// denial. Shopify truncates a long error list, so a page can teach its
// denials a few fields at a time.
const maxDeniedPrunes = 4

// callWithGrant runs an operation from the generated document, minus the
// selections this store's grant has already refused, and learns new ones.
//
// The three ways out: a clean answer; a not-granted, when the denial is the
// root connection itself (the domain, not a field of it); or, when nothing
// new can be removed, the partial answer the origin did return -- and a
// permanent failure when it returned none.
func (c *Connector) callWithGrant(ctx context.Context, store Store, spec *generated.TypeSpec, operation string, vars map[string]any) (*AdminResponse, error) {
	for prunes := 0; ; prunes++ {
		resp, err := c.adminCall(ctx, store, c.documentFor(store, spec), operation, vars)
		if err == nil {
			return resp, nil
		}
		var ae *AdminError
		if !errors.As(err, &ae) || !ae.DeniedOnly() {
			return nil, err
		}
		fields := ae.DeniedFields()
		if deniesRoot(fields, spec) {
			return nil, fmt.Errorf("%w: %v", memqlsync.ErrNotGranted, ae)
		}
		if c.noteDeniedFields(store, spec, fields) && prunes < maxDeniedPrunes {
			continue
		}
		if len(ae.Data) > 0 {
			return &AdminResponse{Data: ae.Data, StatusCode: ae.StatusCode}, nil
		}
		return nil, memqlsync.Permanent(err)
	}
}

// deniesRoot reports whether the denial is of the domain's own root: the
// list connection or the singleton field. That is the grant not covering
// the domain, which no pruning changes.
func deniesRoot(fields []string, spec *generated.TypeSpec) bool {
	for _, f := range fields {
		if (spec.ListQuery != "" && f == spec.ListQuery) || (spec.Singleton != "" && f == spec.Singleton) {
			return true
		}
	}
	return false
}

// documentFor is the generated document for this store: the whole of it
// until the grant has refused something, then the same document without
// those selections.
func (c *Connector) documentFor(store Store, spec *generated.TypeSpec) string {
	denied := c.FieldsDenied(store.ID)[generated.ConceptID(spec.Concept)]
	return pruneSelections(spec.FetchDocument, denied)
}

// reservedSelections are the names a document needs to be a document at
// all; a denial naming one of them is never a field to remove.
var reservedSelections = map[string]bool{
	"id": true, "nodes": true, "edges": true, "node": true, "pageInfo": true,
	"hasNextPage": true, "endCursor": true, "__typename": true, "query": true,
}

// pruneSelections removes every selection of the named fields from a
// generated document.
//
// It works on the generator's own shape rather than on GraphQL in general:
// one selection per line, a nested object either on the same line with its
// braces balanced or opened by a trailing brace and closed by a lone brace
// at the opening indentation. The operation header, the root connection and
// the paging fields are never candidates (reservedSelections, and the root
// is handled before this is reached).
func pruneSelections(document string, fields []string) string {
	drop := map[string]bool{}
	for _, f := range fields {
		if f = strings.TrimSpace(f); f != "" && !reservedSelections[f] {
			drop[f] = true
		}
	}
	if len(drop) == 0 {
		return document
	}
	lines := strings.Split(document, "\n")
	out := make([]string, 0, len(lines))
	skipIndent := -1
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		indent := len(line) - len(trimmed)
		if skipIndent >= 0 {
			if trimmed == "}" && indent == skipIndent {
				skipIndent = -1
			}
			continue
		}
		if drop[selectionName(trimmed)] {
			if strings.HasSuffix(trimmed, "{") && strings.Count(trimmed, "{") > strings.Count(trimmed, "}") {
				skipIndent = indent
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// selectionName is the field a selection line starts with: the identifier
// before its arguments, alias colon or nested braces.
func selectionName(line string) string {
	end := 0
	for end < len(line) {
		ch := line[end]
		if ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (end > 0 && ch >= '0' && ch <= '9') {
			end++
			continue
		}
		break
	}
	return line[:end]
}

// noteDeniedFields records, per store and domain, the fields the grant does
// not cover. Reports whether anything NEW was learned, which is what decides
// a retry; says so in the log once per change rather than on every page of
// every sweep. The store's health reads it back (FieldsDenied).
func (c *Connector) noteDeniedFields(store Store, spec *generated.TypeSpec, fields []string) bool {
	if len(fields) == 0 {
		return false
	}
	c.deniedMu.Lock()
	defer c.deniedMu.Unlock()
	if c.denied == nil {
		c.denied = map[string]map[string][]string{}
	}
	perStore := c.denied[store.ID]
	if perStore == nil {
		perStore = map[string][]string{}
		c.denied[store.ID] = perStore
	}
	concept := generated.ConceptID(spec.Concept)
	before := strings.Join(perStore[concept], ",")
	after := uniqueSorted(append(append([]string(nil), perStore[concept]...), fields...))
	perStore[concept] = after
	learned := strings.Join(after, ",") != before
	if learned && c.logger != nil {
		c.logger.Info("shopify: the store's grant does not cover part of a domain; mirrored without those fields",
			"store", store.ID, "concept", concept, "fields", after)
	}
	return learned
}

// FieldsDenied is what the grant has refused so far, per domain, for one
// store: process-local, learned from the origin's answers since start.
func (c *Connector) FieldsDenied(storeID string) map[string][]string {
	c.deniedMu.Lock()
	defer c.deniedMu.Unlock()
	out := map[string][]string{}
	for concept, fields := range c.denied[storeID] {
		out[concept] = append([]string(nil), fields...)
	}
	return out
}
