package database

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/uptrace/bun/migrate"
)

// THE PAIR IS GO, REGISTERED ONCE, AND NAMED FROM ITS FILES. bun derives a Go
// migration's version and comment from the file its Register call sits in, so
// a registration moved into another file would run under the wrong version --
// possibly one a cluster has already recorded as applied. And bun's Register
// appends without looking, so a set registered twice would run each twice.
func TestTheConceptIndexMigrationsAreGoRegisteredOnceUnderTheirFileNames(t *testing.T) {
	m := migrate.NewMigrations()
	registerTimescaleMigrations(m, nil)
	registerTimescaleMigrations(m, nil)

	byVersion := map[string][]migrate.Migration{}
	for _, mig := range m.Sorted() {
		byVersion[mig.Name] = append(byVersion[mig.Name], mig)
	}
	for _, name := range []string{memoryNodesConceptIndexMigrationName, memoryNodesConceptIndexVerifiedMigrationName} {
		version, comment, _ := strings.Cut(name, "_")
		got := byVersion[version]
		if len(got) != 1 {
			t.Fatalf("migration %s is registered %d times, want exactly once", version, len(got))
		}
		if got[0].Comment != comment {
			t.Errorf("migration %s carries comment %q, want %q: bun names a Go migration after the file its "+
				"Register call sits in, and this one no longer sits in %s.go", version, got[0].Comment, comment, name)
		}
		if got[0].Up == nil || got[0].Down == nil {
			t.Errorf("migration %s: Up set = %v, Down set = %v; both must be functions (20260914000000's "+
				"down is a deliberate no-op, which is a different state from an absent one)", version, got[0].Up != nil, got[0].Down != nil)
		}
	}

	// The SQL pair is gone. Were it still embedded, Discover would pair its
	// files with the same version and the set would carry two answers to one
	// migration.
	leftover, err := fs.Glob(timescaleMigrationsFS, "memory-nodes/migrations/20260913000000_*")
	if err != nil {
		t.Fatal(err)
	}
	if len(leftover) != 0 {
		t.Errorf("the embedded migrations still carry %v; 20260913000000 is a Go migration now", leftover)
	}
}
