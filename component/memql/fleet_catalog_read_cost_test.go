package memql

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// askingCatalog records every catalog a read asks the reader for.
type askingCatalog struct {
	mu     sync.Mutex
	asked  []string
	models []FleetModel
}

func (c *askingCatalog) Catalog(_ context.Context, actingUserId string) ([]FleetModel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, actingUserId)
	return c.models, nil
}

// ONE CATALOG READ PER fleetModels / inferenceStatus / desktopAccess ROW
// (memql#5660).
//
// fleetCatalogForCaller read the person's catalog and then the shared one and
// merged them. Since design G8 the person's catalog already holds every
// machine the shared one does, so the second read repeated the cross-owner
// read -- every registration in the cluster -- for an answer already in hand,
// on every Providers page load and every first-run gate. Measured: a person's
// read asked for two catalogs, which against fleetcatalog.Reader is three
// registration reads (own + cross-owner, then cross-owner again); it now asks
// for one, which is two. desktopAccess is held to it as well: a held person's
// page asks every 30 seconds.
func TestACallersCatalogReadAsksForOneCatalog(t *testing.T) {
	type caller struct {
		name string
		ctx  context.Context
		want []string
		// refused: the read answers an error for this caller and asks for no
		// catalog at all.
		refused bool
	}
	person := caller{name: "a person", ctx: userCtx("alice"), want: []string{"alice"}}
	// No acting person is system work: the shared set, and only it.
	system := caller{name: "no acting person", ctx: context.Background(), want: []string{""}}
	reads := []struct {
		name    string
		read    func(*MemQLEngine, context.Context) error
		callers []caller
	}{
		{"fleetModels", func(e *MemQLEngine, ctx context.Context) error {
			_, err := e.evaluateFleetModelsExpression(ctx)
			return err
		}, []caller{person, system}},
		{"inferenceStatus", func(e *MemQLEngine, ctx context.Context) error {
			_, err := e.evaluateInferenceStatusExpression(ctx)
			return err
		}, []caller{person, system}},
		// desktopAccess answers for a person only: a call that names nobody
		// is refused before it reads anything.
		{"desktopAccess", func(e *MemQLEngine, ctx context.Context) error {
			withAppRows(e)
			e.storedRoleForTest = func(context.Context, string) (auth.Role, bool, error) { return auth.RoleWriter, true, nil }
			_, err := e.evaluateDesktopAccess(ctx, nil, 0)
			return err
		}, []caller{person, {name: "no acting person", ctx: context.Background(), refused: true}}},
	}
	for _, r := range reads {
		for _, tc := range r.callers {
			t.Run(r.name+"/"+tc.name, func(t *testing.T) {
				catalog := &askingCatalog{models: []FleetModel{capable("llama3.1:8b")}}
				registry := newProviderRegistry()
				registry.SetFleetCatalog(catalog)
				err := r.read(&MemQLEngine{providers: registry}, tc.ctx)
				if tc.refused != (err != nil) {
					t.Fatalf("err = %v, refused = %v", err, tc.refused)
				}
				if !reflect.DeepEqual(catalog.asked, tc.want) {
					t.Fatalf("catalogs asked = %q, want %q: a person's catalog already holds the shared set", catalog.asked, tc.want)
				}
			})
		}
	}
}
