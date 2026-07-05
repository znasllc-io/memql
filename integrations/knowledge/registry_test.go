package knowledge

import (
	"testing"
)

// withCleanRegistry snapshots the seed-domain registry, empties it for the
// duration of the test, and restores it afterwards so tests that register
// domains don't leak global state into each other.
func withCleanRegistry(t *testing.T) {
	t.Helper()
	seedDomainMu.Lock()
	savedRegs := seedDomainRegs
	savedIDs := seedDomainIDs
	seedDomainRegs = nil
	seedDomainIDs = map[string]bool{}
	seedDomainMu.Unlock()
	t.Cleanup(func() {
		seedDomainMu.Lock()
		seedDomainRegs = savedRegs
		seedDomainIDs = savedIDs
		seedDomainMu.Unlock()
	})
}

func mustPanic(t *testing.T, wantSubstr string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q, got none", wantSubstr)
		}
		msg, _ := r.(string)
		if msg == "" {
			if err, ok := r.(error); ok {
				msg = err.Error()
			}
		}
		if wantSubstr != "" && !contains(msg, wantSubstr) {
			t.Fatalf("panic message %q does not contain %q", msg, wantSubstr)
		}
	}()
	fn()
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestRegisterSeedDomain_PanicsOnEmptyID(t *testing.T) {
	withCleanRegistry(t)
	mustPanic(t, "empty Domain.ID", func() {
		RegisterSeedDomain(SeedDomainRegistration{Domain: StandardDomain{ID: ""}})
	})
}

func TestRegisterSeedDomain_PanicsOnEngineCatalogCollision(t *testing.T) {
	withCleanRegistry(t)
	// business-administration is a real engine standardDomains catalog id.
	engineID := standardDomains[0].ID
	if engineID == "" {
		t.Fatal("expected a non-empty engine catalog id at index 0")
	}
	mustPanic(t, "collides with the engine standardDomains catalog", func() {
		RegisterSeedDomain(SeedDomainRegistration{Domain: StandardDomain{ID: engineID}})
	})
}

func TestRegisterSeedDomain_PanicsOnDoubleRegistration(t *testing.T) {
	withCleanRegistry(t)
	reg := SeedDomainRegistration{Domain: StandardDomain{ID: "test-double-reg"}}
	RegisterSeedDomain(reg) // first succeeds
	mustPanic(t, "already registered", func() {
		RegisterSeedDomain(reg) // second panics
	})
}

func TestAllSeedDomains_MergeOrder(t *testing.T) {
	withCleanRegistry(t)
	before := allSeedDomains()
	if len(before) != len(standardDomains) {
		t.Fatalf("with an empty registry, allSeedDomains() should equal the engine catalog: got %d, want %d",
			len(before), len(standardDomains))
	}

	RegisterSeedDomain(SeedDomainRegistration{Domain: StandardDomain{ID: "test-merge-a"}})
	RegisterSeedDomain(SeedDomainRegistration{Domain: StandardDomain{ID: "test-merge-b"}})

	after := allSeedDomains()
	if len(after) != len(standardDomains)+2 {
		t.Fatalf("allSeedDomains() should be catalog + 2 registered: got %d, want %d",
			len(after), len(standardDomains)+2)
	}
	// Engine catalog entries come first, in catalog order.
	for i := range standardDomains {
		if after[i].ID != standardDomains[i].ID {
			t.Fatalf("engine catalog entry %d reordered: got %q, want %q", i, after[i].ID, standardDomains[i].ID)
		}
	}
	// Registered domains follow, in registration order.
	if after[len(standardDomains)].ID != "test-merge-a" {
		t.Fatalf("first registered domain out of order: got %q, want %q",
			after[len(standardDomains)].ID, "test-merge-a")
	}
	if after[len(standardDomains)+1].ID != "test-merge-b" {
		t.Fatalf("second registered domain out of order: got %q, want %q",
			after[len(standardDomains)+1].ID, "test-merge-b")
	}
}

func TestRegisteredSeedDomains_ReturnsCopy(t *testing.T) {
	withCleanRegistry(t)
	RegisterSeedDomain(SeedDomainRegistration{Domain: StandardDomain{ID: "test-copy"}})

	got := RegisteredSeedDomains()
	if len(got) != 1 {
		t.Fatalf("expected 1 registered domain, got %d", len(got))
	}
	// Mutating the returned slice must not affect the registry.
	got[0] = SeedDomainRegistration{Domain: StandardDomain{ID: "mutated"}}
	got = append(got, SeedDomainRegistration{Domain: StandardDomain{ID: "extra"}})
	_ = got

	again := RegisteredSeedDomains()
	if len(again) != 1 {
		t.Fatalf("registry length changed after mutating the returned slice: got %d, want 1", len(again))
	}
	if again[0].Domain.ID != "test-copy" {
		t.Fatalf("registry element changed after mutating the returned slice: got %q, want %q",
			again[0].Domain.ID, "test-copy")
	}
}
