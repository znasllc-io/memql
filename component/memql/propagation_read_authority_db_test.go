package memql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageast "github.com/znasllc-io/memql/component/language/ast"
	languageparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslimports"
	"github.com/znasllc-io/memql/dsl"
)

var propagationReadConcepts = map[string]string{
	"productContentForPropagation":      "v1:commerce:productContent",
	"customerNoteForPropagation":        "v1:commerce:customerNote",
	"companyLocationNoteForPropagation": "v1:commerce:companyLocationNote",
	"creditLimitForPropagation":         "v1:commerce:creditLimit",
}

// A named adjudication in tierDecidesTheRead must not survive a changed
// authority boundary. Read the parsed annotations and registered concepts,
// never source-text matches that a comment or string could satisfy.
func TestPropagationReadDeclarationsKeepTheirAdmissionContract(t *testing.T) {
	registry := loadedTreeRegistry(t)
	tree, err := dslimports.Load(dsl.Tree())
	if err != nil {
		t.Fatal(err)
	}
	file := tree.Files["commerce/queries.memql"]
	if file == nil {
		t.Fatal("commerce query declarations are missing")
	}
	actors := map[string]bool{}
	for _, def := range file.Definitions {
		if fn, ok := def.(*languageast.FunctionDef); ok {
			for _, attr := range fn.Attributes {
				if attr != nil && attr.Name == "actor" {
					actors[fn.Name] = true
				}
			}
		}
	}
	for name, concept := range propagationReadConcepts {
		t.Run(name, func(t *testing.T) {
			if _, ok := tierDecidesTheRead[name]; !ok {
				t.Fatal("the connector read has no explicit row-authz adjudication")
			}
			fn, err := registry.Get(name)
			if err != nil || fn == nil {
				t.Fatalf("load query: %v", err)
			}
			if fn.FunctionKind != "query" || fn.BoundConcept != concept || !fn.ServerOnly || !actors[name] {
				t.Fatalf("read lost its query binding, server-only boundary, or actor declaration: %#v", fn)
			}
			c, err := memorynodes.Get(concept)
			if err != nil || c == nil {
				t.Fatalf("load concept: %v", err)
			}
			if c.EffectiveOrigin() != "memql" || len(c.MirroredTo) != 1 || c.MirroredTo[0] != "shopify" ||
				c.RowAuthz == nil || *c.RowAuthz != (languageparser.RowAuthzDecl{Tier: languageparser.RowAuthzClusterOwner}) {
				t.Fatalf("%s changed the source authority this adjudication relies on", concept)
			}
		})
	}
}

// The connector is deliberately not a cluster owner. Every named query must
// return its row to that connector while rejecting client calls and keeping
// the ordinary tier intact. Reuse the exact query after the admitted read to
// exercise actor-dependent cached plans and results, too.
func TestPropagationReadsAdmitOnlyTheirDeclaredAuthority(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("propagationauthority")
	owner := rankActorCtx("owner-"+suffix, auth.RoleOwner)
	callers := []struct {
		name   string
		ctx    context.Context
		rows   int
		refuse bool
	}{
		{"shopify", auth.ContextWithInternalOrigin(connectorCtx("shopify")), 1, false},
		{"other connector", auth.ContextWithInternalOrigin(connectorCtx("quickBooks")), 0, false},
		{"browser owner", owner, 0, true},
		{"internal writer", auth.ContextWithInternalOrigin(rankActorCtx("writer-"+suffix, auth.RoleWriter)), 0, false},
		{"internal owner", auth.ContextWithInternalOrigin(owner), 1, false},
	}
	for name, concept := range propagationReadConcepts {
		t.Run(name, func(t *testing.T) {
			rowID := name + "-" + suffix
			canonicalID := concept + ":" + rowID
			seedRawRow(t, context.Background(), db, concept, canonicalID, time.Now().Add(-time.Second), "owner-"+suffix,
				map[string]any{"storeId": "store-" + suffix, "status": "draft", "note": "private note", "summary": "current copy", "limitAmount": "10.00", "currencyCode": "USD"})
			t.Cleanup(func() {
				_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = ? AND id = ?`, concept, canonicalID)
			})
			query := fmt.Sprintf("query %s(rowId: %s)", name, languageparser.QuoteString(rowID))
			for _, caller := range callers {
				t.Run(caller.name, func(t *testing.T) {
					result, err := eng.Execute(caller.ctx, query)
					if caller.refuse {
						if err == nil {
							t.Fatal("browser caller reached a server-only propagation read")
						}
						return
					}
					if err != nil {
						t.Fatalf("read refused: %v", err)
					}
					if got := len(result.Bundle.GetNodes()); got != caller.rows {
						t.Fatalf("read returned %d rows, want %d", got, caller.rows)
					}
				})
			}
			if result, err := eng.Execute(callers[0].ctx, fmt.Sprintf("query %s(rowId: %s)", name, languageparser.QuoteString(rowID+"-absent"))); err != nil || len(result.Bundle.GetNodes()) != 0 {
				t.Fatalf("row-reference read ignored its requested id: %v", err)
			}
		})
	}
}
