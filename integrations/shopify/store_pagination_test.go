package shopify

import (
	"context"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

type pagedStoreEngine struct {
	*fakeEngine
	pages  map[string]*memql.ExecuteResult
	failAt string
	calls  int
	query  string
}

func (e *pagedStoreEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	if callName(query) != "storesForConnector" {
		return e.fakeEngine.Execute(ctx, query)
	}
	if memql.CursorFromContext(ctx) == "" {
		e.query = query
	} else if e.query != query {
		return nil, fmt.Errorf("snapshot changed between directory pages")
	}
	e.calls++
	if !memql.FreshReadFromContext(ctx) {
		return nil, fmt.Errorf("directory pages must bypass the node's result cache")
	}
	cursor := memql.CursorFromContext(ctx)
	if e.failAt != "" && cursor == e.failAt {
		return nil, fmt.Errorf("second page unavailable")
	}
	return e.pages[cursor], nil
}

func TestStoreRegistryResolvesStoresBeyondTheFirstPage(t *testing.T) {
	first := make([]map[string]any, 100)
	for n := range first {
		first[n] = map[string]any{"id": fmt.Sprintf("store-%03d", n), "domain": fmt.Sprintf("store-%03d.myshopify.com", n)}
	}
	page := resultWithRows(first)
	page.Meta = &memql.ResultMeta{Cursor: "last-page", HasMore: true}
	last := resultWithRows([]map[string]any{{"id": "last", "domain": "z-last.myshopify.com", "status": StatusLive}})
	engine := &pagedStoreEngine{fakeEngine: newFakeEngine(), pages: map[string]*memql.ExecuteResult{"": page, "last-page": last}}
	registry := NewStoreRegistry(engine, nil)
	store, ok := registry.ByDomain(context.Background(), "z-last.myshopify.com")
	if !ok || store.ID != "last" || engine.calls != 2 {
		t.Fatalf("last-page delivery: store=%+v found=%v reads=%d", store, ok, engine.calls)
	}
	stores, err := registry.Stores(context.Background())
	if err != nil || len(stores) != 101 || engine.calls != 2 {
		t.Fatalf("cached directory: count=%d reads=%d err=%v", len(stores), engine.calls, err)
	}
	engine.failAt = "last-page"
	if partial, err := registry.Stores(memql.ContextWithFreshRead(context.Background())); err == nil || partial != nil {
		t.Fatalf("a failed continuation published a partial directory: %v %v", partial, err)
	}
	stores, err = registry.Stores(context.Background())
	if err != nil || len(stores) != 101 {
		t.Fatalf("failed refresh replaced the complete cached directory: %v %v", len(stores), err)
	}
}

func TestStoreRegistryHonorsFreshReadAcrossReplicas(t *testing.T) {
	engine := &pagedStoreEngine{fakeEngine: newFakeEngine(), pages: map[string]*memql.ExecuteResult{
		"": resultWithRows([]map[string]any{{"id": "one", "domain": "one.myshopify.com", "status": StatusLive}}),
	}}
	registry := NewStoreRegistry(engine, nil)
	if _, ok := registry.ByID(context.Background(), "one"); !ok {
		t.Fatal("initial store missing")
	}
	// Another node changed the row before its cache invalidation arrived.
	engine.pages[""] = resultWithRows([]map[string]any{{"id": "one", "domain": "one.myshopify.com", "status": StatusPaused}})
	if store, _ := registry.ByID(context.Background(), "one"); store.Status != StatusLive {
		t.Fatal("test did not retain the earlier cache")
	}
	if store, ok := registry.ByID(memql.ContextWithFreshRead(context.Background()), "one"); !ok || store.Status != StatusPaused {
		t.Fatalf("fresh read used another node's stale status: %+v %v", store, ok)
	}
}
