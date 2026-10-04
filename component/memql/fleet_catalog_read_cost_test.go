package memql

import (
	"context"
	"reflect"
	"sync"
	"testing"
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

// ONE CATALOG READ PER fleetModels / inferenceStatus ROW (memql#5660).
//
// fleetCatalogForCaller read the person's catalog and then the shared one and
// merged them. Since design G8 the person's catalog already holds every
// machine the shared one does, so the second read repeated the cross-owner
// read -- every registration in the cluster -- for an answer already in hand,
// on every Providers page load and every first-run gate. Measured: a person's
// read asked for two catalogs, which against fleetcatalog.Reader is three
// registration reads (own + cross-owner, then cross-owner again); it now asks
// for one, which is two.
func TestACallersCatalogReadAsksForOneCatalog(t *testing.T) {
	reads := map[string]func(*MemQLEngine, context.Context) error{
		"fleetModels": func(e *MemQLEngine, ctx context.Context) error {
			_, err := e.evaluateFleetModelsExpression(ctx)
			return err
		},
		"inferenceStatus": func(e *MemQLEngine, ctx context.Context) error {
			_, err := e.evaluateInferenceStatusExpression(ctx)
			return err
		},
	}
	for name, read := range reads {
		for _, tc := range []struct {
			caller string
			ctx    context.Context
			want   []string
		}{
			{"a person", userCtx("alice"), []string{"alice"}},
			// No acting person is system work: the shared set, and only it.
			{"no acting person", context.Background(), []string{""}},
		} {
			t.Run(name+"/"+tc.caller, func(t *testing.T) {
				catalog := &askingCatalog{models: []FleetModel{capable("llama3.1:8b")}}
				r := newProviderRegistry()
				r.SetFleetCatalog(catalog)
				if err := read(&MemQLEngine{providers: r}, tc.ctx); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(catalog.asked, tc.want) {
					t.Fatalf("catalogs asked = %q, want %q: a person's catalog already holds the shared set", catalog.asked, tc.want)
				}
			})
		}
	}
}
