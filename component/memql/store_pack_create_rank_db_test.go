package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// THE CREATE HOLE ON TWO CLUSTER-OWNER CONCEPTS (Connect Shopify design,
// section 6, decisions D13 and D14).
//
// v1:shopify:store and v1:platform:packState both declare
// @rowAuthz(clusterOwner), which reads like "only an owner writes these". It
// is true for a row that EXISTS: the write guard judges the stored row. A
// CREATE has no stored row, and the guard answers nil
// (rowauthz_write_guard.go), so `createStore` and `setPackEnabled` -- two
// plain inserts -- were open to every signed-in caller. Enabling a pack
// nobody had flipped yet publishes whatever it serves; enabling `wholesale`
// publishes a shopper write endpoint.
//
// The floor is OWNER. The design first set it at developer (D14), and review
// showed two exploits that follow from a developer authoring these rows
// directly: a store row naming a secret it does not own, which the edge then
// publishes, and a pack row under a fresh id that overrides a pack's switch.
// No developer journey needs a direct create -- Connect Shopify writes the
// store under internal origin, and the Modules flip writes the pack the same
// way -- so a developer is refused here like everyone below the owner. The
// matrix pins the rung just below the floor (developer, 300) as refused.
//
// A refusal is only evidence beside proof that nothing landed, so every
// refused case also reads the table: an error raised AFTER the insert would
// otherwise pass.

func TestStoreAndPackCreatesRefuseAnyoneButAnOwner(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)

	cases := []struct {
		role    auth.Role
		allowed bool
	}{
		{auth.RoleWriter, false},
		{auth.RoleAdmin, false},
		{auth.RoleDeveloper, false},
		{auth.RoleOwner, true},
	}
	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			suffix := uniqueSuffix("createrank-" + string(tc.role))
			user := "user-" + suffix
			seedPrincipal(t, eng, user, tc.role)
			ctx := rankActorCtx(user, tc.role)

			storeErr := createStoreFor(t, ctx, eng, suffix)
			packErr := enablePackFor(t, ctx, eng, suffix)
			// The raw insert literal is public language and names no
			// mutation, so a floor declared on createStore alone never sees
			// it. It must be refused at the write seam both paths share.
			rawStoreErr := rawInsertFor(t, ctx, eng, conceptShopifyStoreForTest, "raw-"+storeIdFor(suffix),
				map[string]any{"domain": "raw-" + suffix + ".myshopify.com"})
			rawPackErr := rawInsertFor(t, ctx, eng, conceptPackStateForTest, "raw-"+packDomainFor(suffix),
				map[string]any{"packDomain": "raw-" + packDomainFor(suffix), "enabled": true})

			if tc.allowed {
				for what, err := range map[string]error{
					"createStore": storeErr, "setPackEnabled": packErr,
					"raw store insert": rawStoreErr, "raw packState insert": rawPackErr,
				} {
					if err != nil {
						t.Fatalf("a %s's %s was refused: %v", tc.role, what, err)
					}
				}
				assertRowCount(t, db, conceptShopifyStoreForTest, storeIdFor(suffix), 1)
				assertRowCount(t, db, conceptPackStateForTest, packDomainFor(suffix), 1)
				assertRowCount(t, db, conceptShopifyStoreForTest, "raw-"+storeIdFor(suffix), 1)
				assertRowCount(t, db, conceptPackStateForTest, "raw-"+packDomainFor(suffix), 1)
				return
			}

			for what, err := range map[string]error{
				"createStore": storeErr, "setPackEnabled": packErr,
				"raw store insert": rawStoreErr, "raw packState insert": rawPackErr,
			} {
				if err == nil {
					t.Fatalf("a %s's %s on a fresh id was ALLOWED. The concept is "+
						"@rowAuthz(clusterOwner), but the write guard does not judge a create, so "+
						"without the create floor (create_rank_floor.go) every signed-in caller can write one", tc.role, what)
				}
				if !strings.Contains(err.Error(), `"owner"`) {
					t.Fatalf("%s's refusal was %q, which does not name the role required", what, err)
				}
			}
			assertRowCount(t, db, conceptShopifyStoreForTest, storeIdFor(suffix), 0)
			assertRowCount(t, db, conceptPackStateForTest, packDomainFor(suffix), 0)
			assertRowCount(t, db, conceptShopifyStoreForTest, "raw-"+storeIdFor(suffix), 0)
			assertRowCount(t, db, conceptPackStateForTest, "raw-"+packDomainFor(suffix), 0)
		})
	}
}

// rawInsertFor writes a row with the raw insert literal under ctx, the form
// seedPrincipal uses, and returns the error.
func rawInsertFor(t *testing.T, ctx context.Context, eng *MemQLEngine, conceptName, id string, payload map[string]any) error {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	_, err = eng.Execute(ctx, fmt.Sprintf(`insert(%s, id=%s, payload=%s)`,
		langparser.QuoteString(conceptName), langparser.QuoteString(id), string(b)))
	return err
}

// TestStoreAndPackCreatesAdmitInternalOrigin pins the other half: the floor
// governs principals, and trusted server code stamped for one call is not one
// (create_rank_floor.go, via requires_rank.go). The Shopify first-boot seed,
// the connector, Connect Shopify's writes and the Modules flip all reach these
// inserts that way, so a floor that refused them would break every legitimate
// writer at once.
func TestStoreAndPackCreatesAdmitInternalOrigin(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("createrank-internal")
	user := "user-" + suffix
	seedPrincipal(t, eng, user, auth.RoleWriter)
	ctx := auth.ContextWithInternalOrigin(rankActorCtx(user, auth.RoleWriter))

	if err := createStoreFor(t, ctx, eng, suffix); err != nil {
		t.Fatalf("an internal-origin createStore was refused: %v", err)
	}
	if err := enablePackFor(t, ctx, eng, suffix); err != nil {
		t.Fatalf("an internal-origin setPackEnabled was refused: %v", err)
	}
	if err := rawInsertFor(t, ctx, eng, conceptShopifyStoreForTest, "raw-"+storeIdFor(suffix),
		map[string]any{"domain": "raw-" + suffix + ".myshopify.com"}); err != nil {
		t.Fatalf("an internal-origin raw store insert was refused: %v", err)
	}
	assertRowCount(t, db, conceptShopifyStoreForTest, storeIdFor(suffix), 1)
	assertRowCount(t, db, conceptPackStateForTest, packDomainFor(suffix), 1)
	assertRowCount(t, db, conceptShopifyStoreForTest, "raw-"+storeIdFor(suffix), 1)
}

const (
	conceptShopifyStoreForTest = "v1:shopify:store"
	conceptPackStateForTest    = "v1:platform:packState"
)

func storeIdFor(suffix string) string    { return "store-" + suffix }
func packDomainFor(suffix string) string { return "pack-" + suffix }

func createStoreFor(t *testing.T, ctx context.Context, eng *MemQLEngine, suffix string) error {
	t.Helper()
	_, err := runSiteMutation(t, ctx, eng, "createStore", map[string]any{
		"storeId": storeIdFor(suffix),
		"domain":  suffix + ".myshopify.com",
	})
	return err
}

// enablePackFor flips a pack that has no packState row yet -- the create, which
// is the case the write guard never judged.
func enablePackFor(t *testing.T, ctx context.Context, eng *MemQLEngine, suffix string) error {
	t.Helper()
	_, err := runSiteMutation(t, ctx, eng, "setPackEnabled", map[string]any{
		"id":         packDomainFor(suffix),
		"packDomain": packDomainFor(suffix),
		"enabled":    true,
	})
	return err
}

// assertRowCount counts the stored versions of (concept, bare id) straight off
// the table, under no actor at all -- so a zero here means nothing was written,
// not that a tier hid it.
func assertRowCount(t *testing.T, db *bun.DB, conceptName, bareId string, want int) {
	t.Helper()
	n, err := db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).
		Where("concept = ?", conceptName).
		Where("id = ?", conceptName+":"+bareId).
		Count(context.Background())
	if err != nil {
		t.Fatalf("count %s/%s: %v", conceptName, bareId, err)
	}
	if (want == 0) != (n == 0) {
		t.Fatalf("%s/%s has %d stored versions, want %d", conceptName, bareId, n, want)
	}
}
