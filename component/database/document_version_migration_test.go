package database

import (
	"strings"
	"testing"

	"github.com/uptrace/bun/migrate"
)

// TestDocumentVersionMigration_DiscoveredAndPaired asserts the
// document version history migration (memql#1228) is picked up by the
// embed glob the same way every other timescale migration is, and that
// it ships a matched up + down pair. A missing down would silently make
// the migration irreversible.
func TestDocumentVersionMigration_DiscoveredAndPaired(t *testing.T) {
	m := migrate.NewMigrations()
	if err := m.Discover(timescaleMigrationsFS); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var found *migrate.Migration
	sorted := m.Sorted()
	for i := range sorted {
		mig := sorted[i]
		if strings.Contains(mig.Comment, "document_version_history") || strings.Contains(mig.Name, "20260609000000") {
			found = &mig
			break
		}
	}
	if found == nil {
		t.Fatal("document_version_history migration not discovered by the embed glob")
	}
	if found.Up == nil {
		t.Error("document_version_history migration has no Up function (up.sql missing or empty)")
	}
	if found.Down == nil {
		t.Error("document_version_history migration has no Down function (down.sql missing or empty)")
	}
}

// TestDocumentVersionMigration_SqlShape asserts the up/down SQL has the
// load-bearing structure: the up creates the version-history index +
// enables compression guarded on TimescaleDB with no retention drop;
// the down reverses both. Catches a hand-edit that drops the guard or
// accidentally adds a retention policy on the long-lived history.
func TestDocumentVersionMigration_SqlShape(t *testing.T) {
	up := readMigrationSQL(t, "20260609000000_document_version_history.up.sql")
	down := readMigrationSQL(t, "20260609000000_document_version_history.down.sql")

	// Up: the partial history index, scoped to the documentVersion concept.
	if !strings.Contains(up, "memory_nodes_document_version_history_idx") {
		t.Error("up.sql missing the version-history index")
	}
	if !strings.Contains(up, "v1:library:documentVersion") {
		t.Error("up.sql index is not scoped to the documentVersion concept")
	}
	// Up: compression is guarded on the timescaledb extension being present.
	if !strings.Contains(up, "pg_extension WHERE extname = 'timescaledb'") {
		t.Error("up.sql compression block is not guarded on the timescaledb extension")
	}
	if !strings.Contains(up, "add_compression_policy") {
		t.Error("up.sql missing the compression policy")
	}
	// History is long-lived: NO retention policy on the up path.
	if strings.Contains(up, "add_retention_policy") {
		t.Error("up.sql sets a retention policy -- document version history must be long-lived (no scheduled drop)")
	}

	// Down: reverse both the index and the compression.
	if !strings.Contains(down, "DROP INDEX") || !strings.Contains(down, "memory_nodes_document_version_history_idx") {
		t.Error("down.sql does not drop the version-history index")
	}
	if !strings.Contains(down, "remove_compression_policy") {
		t.Error("down.sql does not remove the compression policy")
	}

	// Balanced DO $$ ... $$ blocks on both paths (a stray $$ would make
	// the migration fail at runtime, which the no-DB unit lane can't catch
	// otherwise).
	if c := strings.Count(up, "$$"); c%2 != 0 {
		t.Errorf("up.sql has unbalanced $$ dollar-quote markers (%d)", c)
	}
	if c := strings.Count(down, "$$"); c%2 != 0 {
		t.Errorf("down.sql has unbalanced $$ dollar-quote markers (%d)", c)
	}
}

func readMigrationSQL(t *testing.T, name string) string {
	t.Helper()
	b, err := timescaleMigrationsFS.ReadFile("memory-nodes/migrations/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
