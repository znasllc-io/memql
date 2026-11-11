package shopify

// children.go -- a child connection read past its first page.
//
// A fetch reads each child connection one page deep (an order's lineItems
// take the first 100) and selects its pageInfo. When the origin says more
// follow, the rest is read here, a page at a time, through the generated
// page operation, and appended to the object BEFORE it is mapped -- so the
// later lines are mapped with the rest, under the same parent and store.

import (
	"context"
	"fmt"

	"github.com/znasllc-io/memql/integrations/shopify/generated"
)

// maxChildPages bounds how deep one child connection is read: its first
// page plus the pages after it, fifty in all. A connection still promising
// more at the ceiling is kept as far as it was read and said in the log.
const maxChildPages = 50

// completeChildren reads the remaining pages of every paged child
// connection on obj and appends their nodes to it.
func (c *Connector) completeChildren(ctx context.Context, store Store, spec *generated.TypeSpec, obj map[string]any) error {
	if spec == nil || obj == nil {
		return nil
	}
	parentGID, _ := obj["id"].(string)
	for _, child := range spec.Children {
		if child.List || child.PageOp == "" {
			continue
		}
		conn, _ := obj[child.Connection].(map[string]any)
		if conn == nil {
			continue
		}
		for pages := 1; ; pages++ {
			hasNext, cursor := pageInfoOf(conn)
			if !hasNext || cursor == "" {
				break
			}
			if pages >= maxChildPages {
				c.logger.Warn("shopify: a child connection is longer than its page ceiling; the rest is not mirrored",
					"store", store.ID, "concept", spec.Concept,
					"connection", child.Connection, "pages", maxChildPages)
				break
			}
			vars := map[string]any{"after": cursor}
			if spec.Singleton == "" {
				vars["id"] = parentGID
			}
			resp, err := c.callWithGrant(ctx, store, spec, child.PageOp, vars)
			if err != nil {
				return fmt.Errorf("shopify: read %s of %s past page %d: %w", child.Connection, parentGID, pages, err)
			}
			data, err := resp.DataMap()
			if err != nil {
				return err
			}
			page, _ := fetchedObject(data, spec)[child.Connection].(map[string]any)
			if page == nil {
				// The parent is gone, or the grant pruned the connection
				// from the document: nothing further to read.
				break
			}
			nodes, _ := conn["nodes"].([]any)
			more, _ := page["nodes"].([]any)
			conn["nodes"] = appendUnseen(nodes, more)
			conn["pageInfo"] = page["pageInfo"]
			if len(more) == 0 {
				// A page with nothing on it: the origin is not advancing,
				// and neither should this loop.
				break
			}
			if _, next := pageInfoOf(conn); next == cursor {
				// The page hands back the cursor it was asked with: asking
				// again would answer the same page, to the ceiling.
				c.logger.Warn("shopify: a child connection's page repeated its cursor; the rest is not mirrored",
					"store", store.ID, "concept", spec.Concept, "connection", child.Connection, "pages", pages+1)
				break
			}
		}
	}
	return nil
}

// appendUnseen appends the nodes whose id is not already among nodes, so a
// page the origin repeats is not mapped twice. A node with no id (a price
// list's price) is appended as it comes.
func appendUnseen(nodes, more []any) []any {
	seen := map[string]bool{}
	for _, n := range nodes {
		if obj, ok := n.(map[string]any); ok {
			if id, _ := obj["id"].(string); id != "" {
				seen[id] = true
			}
		}
	}
	for _, n := range more {
		if obj, ok := n.(map[string]any); ok {
			if id, _ := obj["id"].(string); id != "" {
				if seen[id] {
					continue
				}
				seen[id] = true
			}
		}
		nodes = append(nodes, n)
	}
	return nodes
}

// pageInfoOf reads a connection's pageInfo.
func pageInfoOf(conn map[string]any) (bool, string) {
	info, _ := conn["pageInfo"].(map[string]any)
	if info == nil {
		return false, ""
	}
	hasNext, _ := info["hasNextPage"].(bool)
	cursor, _ := info["endCursor"].(string)
	return hasNext, cursor
}
