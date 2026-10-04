package fleetcatalog

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memql "github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// rowsFixture answers both registration reads from one set of rows -- the
// owner read scoped to the actor it ran as, in either spelling of the id --
// and counts them.
type rowsFixture struct {
	rows []any

	mu     sync.Mutex
	counts map[string]int
}

func (f *rowsFixture) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	f.mu.Lock()
	if f.counts == nil {
		f.counts = map[string]int{}
	}
	f.counts[query]++
	f.mu.Unlock()
	switch query {
	case "query myWorkersWithStatus()":
		claims, _ := auth.ClaimsFromContext(ctx)
		sub, _ := claims["sub"].(string)
		own := []any{}
		for _, row := range f.rows {
			if owner, _ := row.(map[string]any)["ownerUserId"].(string); workerservice.SameSubjectId(owner, sub) {
				own = append(own, row)
			}
		}
		return memql.NewResultWithOutput(own), nil
	case "query allWorkersWithStatus()":
		return memql.NewResultWithOutput(f.rows), nil
	}
	return nil, fmt.Errorf("unexpected query %q", query)
}

func (f *rowsFixture) take() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.counts
	f.counts = map[string]int{}
	return out
}

// everySharingShape is one machine per owner-and-machine consent pair, each
// offering its own model, so a catalog's model set says which machines it
// admitted.
func everySharingShape(now time.Time) []any {
	shapes := []struct {
		id, owner string
		sharing   map[string]any
		serve     string
	}{
		{"alice-private", "v1:identity:user:alice", map[string]any{"mode": "owner"}, "owner"},
		{"bob-everyone", "v1:identity:user:bob", map[string]any{"mode": "cluster"}, "cluster"},
		{"bob-everyone-unagreed", "v1:identity:user:bob", map[string]any{"mode": "cluster"}, "owner"},
		{"bob-private", "v1:identity:user:bob", map[string]any{"mode": "owner"}, "cluster"},
		{"bob-for-alice", "v1:identity:user:bob", map[string]any{"mode": "people", "userIds": []any{"alice"}}, "cluster"},
		{"bob-for-design", "v1:identity:user:bob", map[string]any{"mode": "people", "groupIds": []any{"design"}}, "cluster"},
		{"carol-everyone", "v1:identity:user:carol", map[string]any{"mode": "cluster"}, "cluster"},
	}
	rows := make([]any, 0, len(shapes))
	for _, s := range shapes {
		row := machineRow(s.id, "model-"+s.id, now)
		row["ownerUserId"] = s.owner
		row["sharing"] = s.sharing
		row["capabilityDescriptor"] = map[string]any{"inferenceServe": s.serve}
		rows = append(rows, row)
	}
	return rows
}

func machinesIn(models []memql.FleetModel) map[string]bool {
	out := map[string]bool{}
	for _, m := range models {
		for _, machine := range m.Machines {
			out[m.ModelId+" on "+machine.RegistrationId] = true
		}
	}
	return out
}

// A PERSON'S CATALOG CONTAINS THE SHARED CATALOG, for every person
// (memql#5660). fleetCatalogForCaller reads ONE catalog for a person -- it read
// the person's and then the shared one, and merged them -- and that is right
// only because a machine lent to everyone is lent to every person: ServesPerson
// admits any person under `cluster`, a synthetic actor included. A change that
// narrowed the person catalog below the shared one would make the Providers
// page and the first-run gate lose every machine lent to everyone, silently;
// this is what fails instead.
func TestAPersonsCatalogContainsTheSharedCatalog(t *testing.T) {
	now := time.Now().UTC()
	store := &EngineStore{Engine: &rowsFixture{rows: everySharingShape(now)}}
	groups := func(_ context.Context, userId string) []string {
		if workerservice.SameSubjectId(userId, "v1:identity:user:dee") {
			return []string{"v1:identity:group:design"}
		}
		return nil
	}
	read := func(owner string) map[string]bool {
		t.Helper()
		models, err := (&Reader{Store: store, Now: func() time.Time { return now }, Groups: groups}).Catalog(context.Background(), owner)
		if err != nil {
			t.Fatal(err)
		}
		return machinesIn(models)
	}

	shared := read("")
	if len(shared) != 2 || !shared["model-bob-everyone on bob-everyone"] || !shared["model-carol-everyone on carol-everyone"] {
		t.Fatalf("the shared catalog is the machines lent to everyone with both consents, got %v", shared)
	}
	for _, person := range []string{
		"alice", "v1:identity:user:alice", // listed by name, owns a private machine
		"v1:identity:user:dee",      // in a listed group
		"v1:identity:user:erin",     // on no list, owns nothing
		"v1:identity:user:bob",      // owns machines lent to others
		"system:automation:nightly", // the cluster's own work, as a synthetic actor
	} {
		mine := read(person)
		for machine := range shared {
			if !mine[machine] {
				t.Errorf("%s's catalog lacks %s, which the shared catalog lists: a person's catalog must contain every machine lent to everyone", person, machine)
			}
		}
	}
}

// What one catalog read costs the store: the person's own machines and one
// cross-owner read to find what is lent to them; for system work, the
// cross-owner read alone. Measured for memql#5660 -- these are the figures the
// fold in fleetCatalogForCaller is counted against.
func TestWhatOneCatalogReadCostsTheStore(t *testing.T) {
	now := time.Now().UTC()
	fixture := &rowsFixture{rows: everySharingShape(now)}
	r := &Reader{Store: &EngineStore{Engine: fixture}, Now: func() time.Time { return now }, Groups: func(context.Context, string) []string { return nil }}
	for _, tc := range []struct {
		owner    string
		own, all int
	}{
		{"v1:identity:user:alice", 1, 1},
		{"", 0, 1},
	} {
		if _, err := r.Catalog(context.Background(), tc.owner); err != nil {
			t.Fatal(err)
		}
		got := fixture.take()
		t.Logf("Catalog(%q): myWorkersWithStatus=%d allWorkersWithStatus=%d", tc.owner,
			got["query myWorkersWithStatus()"], got["query allWorkersWithStatus()"])
		if got["query myWorkersWithStatus()"] != tc.own || got["query allWorkersWithStatus()"] != tc.all || len(got) > 2 {
			t.Fatalf("Catalog(%q) reads = %v, want %d own and %d cross-owner", tc.owner, got, tc.own, tc.all)
		}
	}
}
