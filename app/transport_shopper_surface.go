//go:build bff

package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
)

// transport_shopper_surface.go wires a pack's declared SHOPPER SURFACE onto
// the bff's mux (epic memql#5532, issue memql#5551).
//
// THE BFF ONLY, for the reason the inbound receiver and the unsubscribe
// endpoint are bff-only: this is the node an ingress already routes external
// traffic to, and the caller is a third party -- here a member of the public
// posting a plain HTML form on a merchant's storefront. No internal node
// should carry it.
//
// IT ALWAYS MOUNTS, even with no pack declaring anything. A conditional
// mount would make "has any pack declared a form" and "does the route exist"
// two different questions with the same 404 answer, which is the trap
// mountUnsubscribeEndpoint records for its own secret.

func (a *App) mountShopperSurface() {
	handler := server.NewShopperHandler(
		&ShopperEngineAdapter{Engine: a.engine},
		&shopperSiteReader{engine: a.engine},
		a.Logger,
	)
	for _, path := range server.ShopperSurfacePaths() {
		// Both methods on both prefixes: the handler decides which is legal
		// for which, so a GET to a form path is a 404 from the handler
		// rather than a 405 from the mux. One answer for "there is nothing
		// here" beats two that a prober can tell apart.
		a.handleRoute("GET "+path, handler)
		a.handleRoute("POST "+path, handler)
	}
	if surface := memql.ShopperSurface(); len(surface) > 0 {
		named := make([]string, 0, len(surface))
		for _, entry := range surface {
			named = append(named, entry.Kind+" "+entry.Pack+"/"+entry.Name)
		}
		// SAID AT BOOT, because "what can the public reach on this cluster"
		// is a question an operator should be able to answer from a log line
		// rather than by reading Go.
		a.Logger.Info("shopper surface mounted; these are reachable on any deployable "+
			"whose shopperForms is on",
			"component", memql.ComponentName, "declared", named)
	}
}

// shopperSiteReader resolves a site row under the OWNER'S OWN ACTOR, which
// is what makes the edge's owner stamp self-checking: v1:platform:site is
// composite-owner tier, so a user who does not own the named site reads
// ZERO ROWS and the request refuses itself.
type shopperSiteReader struct {
	engine *memql.MemQLEngine
}

func (s *shopperSiteReader) ShopperSite(ctx context.Context, siteID, ownerUserID string) (*server.ShopperSite, error) {
	siteID = strings.TrimSpace(siteID)
	ownerUserID = strings.TrimSpace(ownerUserID)
	if siteID == "" || ownerUserID == "" || s.engine == nil {
		return nil, nil
	}
	q := fmt.Sprintf("query siteById(siteId: %s)", langparser.QuoteString(siteID))
	result, err := s.engine.Execute(auth.ContextWithUserActor(ctx, ownerUserID), q)
	if err != nil {
		return nil, err
	}
	rows := memql.MaterializeRows(result)
	if len(rows) == 0 {
		// NOT AN ERROR. A site this user cannot read is a request that gets
		// exactly what an unauthenticated one gets.
		return nil, nil
	}
	row := rows[0]
	out := &server.ShopperSite{
		ID:           siteID,
		ShopperForms: rowBoolValue(row["shopperForms"]),
	}
	out.StoreID = bindingStoreID(row["binding"])
	out.PreviewStoreID = bindingStoreID(row["previewBinding"])
	return out, nil
}

// bindingStoreID reads {storeId} out of a binding object, or "".
//
// The binding is carried as the untyped object the row stores, exactly as
// component/edge carries it, because the shape is per-kind and only one kind
// declares one.
func bindingStoreID(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	id, _ := m["storeId"].(string)
	return strings.TrimSpace(id)
}

// rowBoolValue reads a decoded payload boolean. ABSENT AND FALSE ARE ONE
// ANSWER here and that is correct: a site row written before shopperForms
// existed must resolve with no shopper surface, and there is no third state.
func rowBoolValue(v any) bool {
	b, _ := v.(bool)
	return b
}
