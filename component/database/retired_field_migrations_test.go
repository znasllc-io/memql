package database

import (
	"strings"
	"testing"

	"github.com/uptrace/bun/migrate"
)

// THE FOUR RETIRED-FIELD MIGRATIONS ARE DISCOVERED AND PAIRED (memql#5210).
//
// Its sibling `retired_field_migrations_db_test.go` runs these files against a
// real database and proves the SQL does what its comment says. It cannot prove
// this: it reads each file by name and executes it directly, so a migration
// that the embed glob never picked up would still pass every case there while
// never running on any cluster.
//
// That is not a hypothetical shape in this repo. `TestEmbeddedFileCountsAreStable`
// exists because a `go.mod` landing inside an embedded tree cuts it out of the
// embed with `go build` still exiting 0 and printing nothing. This is the same
// silence one layer up: discovery, rather than embedding.
//
// A MISSING DOWN IS ALSO CHECKED. All four down migrations are deliberate
// no-ops -- the up drops keys whose values are gone once dropped, and inventing
// them back would re-assert exactly the stale data each removal existed to
// delete -- but "deliberately a no-op" and "absent" are different states, and
// only the first one is a decision somebody made. bun reports both as `Down ==
// nil` unless the file is there.
//
// This test is in-package and DATABASE-FREE, which is why it is in a separate
// file from its db-gated sibling: that one has to live in the external test
// package (dbtest imports memory-nodes, which imports this package), and the
// embed handle it would need is unexported.
func TestTheRetiredFieldMigrationsAreDiscoveredAndPaired(t *testing.T) {
	m := migrate.NewMigrations()
	if err := m.Discover(timescaleMigrationsFS); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	sorted := m.Sorted()
	if len(sorted) == 0 {
		t.Fatal("no migrations discovered at all: this test would pass over nothing")
	}

	// Keyed by TIMESTAMP rather than by full name, so renaming a migration for
	// clarity does not fail a test about whether it runs.
	for _, stamp := range []string{
		"20260908020000", // the conversational agent fields
		"20260908030000", // the planner retirement's pointers
		"20260908040000", // the environment collapse
		"20260908050000", // the retired policy slug
		"20260909010000", // remaining agent fields and closed nested blocks
		"20260915220000", // retired system-owned Portal site
		"20260915230000", // current site-health observations
	} {
		var found *migrate.Migration
		for i := range sorted {
			if strings.Contains(sorted[i].Name, stamp) {
				found = &sorted[i]
				break
			}
		}
		if found == nil {
			t.Errorf("migration %s was not discovered by the embed glob, so it would never run "+
				"on any cluster -- and the db-gated sibling reads the file by name, so it would "+
				"not notice", stamp)
			continue
		}
		if found.Up == nil {
			t.Errorf("migration %s has no Up (up.sql missing or empty)", stamp)
		}
		if found.Down == nil {
			t.Errorf("migration %s has no Down. All four downs are deliberate no-ops, but an "+
				"ABSENT down and a down that says why there is nothing to undo are different "+
				"states, and only one of them is a decision", stamp)
		}
	}
}
