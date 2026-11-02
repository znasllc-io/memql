package pipelines

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInDeployWindow(t *testing.T) {
	zone := "America/Los_Angeles"
	at := func(clock string) time.Time {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := time.ParseInLocation("2006-01-02 15:04", "2026-11-05 "+clock, loc)
		if err != nil {
			t.Fatal(err)
		}
		return parsed.UTC()
	}
	across := TrainSettings{TimeZone: zone, Deployment: &TrainDeployment{WindowStart: "22:00", WindowEnd: "07:00"}}
	within := TrainSettings{TimeZone: zone, Deployment: &TrainDeployment{WindowStart: "01:00", WindowEnd: "07:00"}}
	for _, tc := range []struct {
		name  string
		s     TrainSettings
		clock string
		want  bool
	}{
		{"before a window across midnight", across, "21:59", false},
		{"its start", across, "22:00", true},
		{"after midnight", across, "03:00", true},
		{"its end is out", across, "07:00", false},
		{"before a plain window", within, "00:59", false},
		{"inside a plain window", within, "06:59", true},
		{"after a plain window", within, "12:00", false},
		{"no window at all", TrainSettings{TimeZone: zone, Deployment: &TrainDeployment{}}, "03:00", false},
		{"no deployment at all", TrainSettings{TimeZone: zone}, "03:00", false},
	} {
		if got := tc.s.InDeployWindow(at(tc.clock)); got != tc.want {
			t.Errorf("%s at %s: %v, want %v", tc.name, tc.clock, got, tc.want)
		}
	}
}

// Over every migration this repository really has, because a rule that reads text is
// only as good as the text it has seen.
func TestClassifyRealMigrations(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "database", "20*.go"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no Go migrations found: %v", err)
	}
	want := map[string]MigrationKind{
		"20260912000000_site_owner_restamp.go":                MigrationData,
		"20260921000000_module_readiness_history_collapse.go": MigrationData,
		// It runs its own statements through a local helper, so it asks. That is the
		// cost of the rule, and one press of a button.
		"20260926000000_work_recovery_index.go": MigrationData,
	}
	for _, path := range matches {
		name := filepath.Base(path)
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		expected, named := want[name]
		if !named {
			expected = MigrationIndexes // Every other one in this directory only adds an index.
		}
		if got := ClassifyMigration(string(body)); got != expected {
			t.Errorf("%s reads as %s, want %s", name, got, expected)
		}
	}
	sql, err := filepath.Glob(filepath.Join("..", "database", "memory-nodes", "migrations", "*.up.sql"))
	if err != nil || len(sql) == 0 {
		t.Fatalf("no SQL migrations found: %v", err)
	}
	indexes, data := 0, 0
	for _, path := range sql {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		switch ClassifySQLMigration(string(body)) {
		case MigrationIndexes:
			indexes++
		case MigrationData:
			data++
		}
	}
	if data == 0 || indexes == 0 {
		t.Errorf("of %d SQL migrations, %d read as indexes and %d as data; a rule that says one thing of all of them reads nothing", len(sql), indexes, data)
	}
	t.Logf("%d SQL migrations: %d indexes only, %d tables or data", len(sql), indexes, data)
}

func TestClassifyMigrationText(t *testing.T) {
	wrap := func(body string) string {
		return "package database\nfunc register(m *migrate.Migrations, db *bun.DB) {\n m.MustRegister(func(ctx context.Context, db *bun.DB) error {\n" + body + "\n return nil\n })\n}\n"
	}
	for name, tc := range map[string]struct {
		source string
		want   MigrationKind
	}{
		"a helper beside the migrations":        {"package database\nfunc quoteIdentifier(s string) string { return s }\n", MigrationNone},
		"not Go at all":                         {"m.MustRegister(", MigrationData},
		"an index":                              {wrap("db.ExecContext(ctx, `CREATE INDEX CONCURRENTLY IF NOT EXISTS i ON t (a)`)"), MigrationIndexes},
		"a unique index":                        {wrap("db.ExecContext(ctx, `create unique index i on t (a)`)"), MigrationIndexes},
		"an index through the helper":           {wrap("ensurePayloadLookupIndex(ctx, db, logger, table, name, field, nil)"), MigrationIndexes},
		"a column":                              {wrap("db.ExecContext(ctx, `ALTER TABLE t ADD COLUMN c text`)"), MigrationData},
		"a table":                               {wrap("db.ExecContext(ctx, `CREATE TABLE t (a int)`)"), MigrationData},
		"a replaced function":                   {wrap("db.ExecContext(ctx, `CREATE OR REPLACE FUNCTION f() RETURNS int`)"), MigrationData},
		"rows, quoted":                          {wrap("db.ExecContext(ctx, `UPDATE \"MemoryNodes\" SET a = 1`)"), MigrationData},
		"rows, unquoted":                        {wrap("db.ExecContext(ctx, `UPDATE work_run_heads SET a = 1`)"), MigrationData},
		"rows, lower case and across lines":     {wrap("db.ExecContext(ctx, `update work_run_heads\n  set a = 1`)"), MigrationData},
		"an index and rows":                     {wrap("db.ExecContext(ctx, `CREATE INDEX i ON t (a); DELETE FROM t`)"), MigrationData},
		"an index, and a helper it cannot read": {wrap("ensurePayloadLookupIndex(ctx, db, logger, table, name, field, nil)\n restampEverything(ctx, db)"), MigrationData},
		"a helper it cannot read":               {wrap("return restampEverything(ctx, db)"), MigrationData},
		"a statement written elsewhere":         {wrap("ensureThisIndex(ctx)\n db.ExecContext(ctx, statementFromElsewhere)"), MigrationData},
		"nothing about an index":                {wrap("db.ExecContext(ctx, `VACUUM t`)"), MigrationData},
		"a change named only in a comment":      {wrap("// DELETE FROM t would be wrong\n db.ExecContext(ctx, `CREATE INDEX i ON t (a)`)"), MigrationIndexes},
	} {
		if got := ClassifyMigration(tc.source); got != tc.want {
			t.Errorf("%s reads as %s, want %s", name, got, tc.want)
		}
	}
	for name, tc := range map[string]struct {
		source string
		want   MigrationKind
	}{
		"nothing":            {"-- only a comment\n", MigrationNone},
		"an index":           {"CREATE INDEX IF NOT EXISTS i ON t (a);\n", MigrationIndexes},
		"session and index":  {"SET lock_timeout = '5s';\ncreate unique index i on t (a);\n", MigrationIndexes},
		"a table":            {"CREATE TABLE t (a int);\n", MigrationData},
		"an index and rows":  {"CREATE INDEX i ON t (a);\nUPDATE t SET a = 1;\n", MigrationData},
		"a statement unseen": {"SELECT do_things();\n", MigrationData},
	} {
		if got := ClassifySQLMigration(tc.source); got != tc.want {
			t.Errorf("SQL, %s reads as %s, want %s", name, got, tc.want)
		}
	}
}

func TestSchemaAndContractChangesInAPatch(t *testing.T) {
	patch := "@@ -1,3 +1,6 @@\n+++ b/x.go\n context\n+	_, err := db.Exec(ctx, `CREATE TABLE IF NOT EXISTS t (a int)`)\n+	// ALTER TABLE t would be wrong\n-	return `SELECT 1`\n+	PipelineStepContract = 6\n-	contracts := map[string]int{\"workerHost.pipeline_step\": 5}\n+	x := 1\n"
	lines := PatchLines(patch)
	if got := SchemaChanges(lines); len(got) != 1 || !strings.Contains(got[0], "CREATE TABLE") {
		t.Errorf("schema changes = %q", got)
	}
	if got := ContractChanges(lines); len(got) != 2 {
		t.Errorf("contract changes = %q", got)
	}
	if got := SchemaChanges(PatchLines("+	fmt.Println(\"update the table\")\n+	insert := 1\n")); len(got) != 0 {
		t.Errorf("ordinary code read as a schema change: %q", got)
	}
	if got := DifferingLines("a\nb\n// c\n", "a\nd\n// e\n"); strings.Join(got, ",") != "b,d" {
		t.Errorf("differing lines = %q", got)
	}
	for path, want := range map[string]bool{
		"component/memql/embedder_tables.go":          true,
		"component/memql/embedder_tables_test.go":     false,
		"cockpit/internal/store/schema.go":            false,
		"clients/os/src/x.sql":                        false,
		"integrations/release/journal.sql":            true,
		"component/database/migrations/0001.down.sql": false,
		"README.md": false,
	} {
		if got := EngineCodePath(path); got != want {
			t.Errorf("EngineCodePath(%s) = %v", path, got)
		}
	}
	for path, want := range map[string]bool{
		"integrations/agent/worker/pipeline_purpose.go": true,
		"cockpit/internal/worker/tools/capabilities.go": true,
		"core/computerpermission/contract.go":           true,
		"cockpit/internal/worker/tools/x_test.go":       false,
		"component/memql/plugins.go":                    false,
	} {
		if got := WorkerContractPath(path); got != want {
			t.Errorf("WorkerContractPath(%s) = %v", path, got)
		}
	}
}

// The files that really state a contract read as stating one, so the rule above is not
// looking for words nobody writes.
func TestRealContractFilesReadAsContracts(t *testing.T) {
	for _, path := range []string{"integrations/agent/worker/pipeline_purpose.go", "cockpit/internal/worker/tools/capabilities.go"} {
		body, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		if got := ContractChanges(PatchLines(strings.ReplaceAll("+"+string(body), "\n", "\n+"))); len(got) == 0 {
			t.Errorf("%s states no contract this rule can see", path)
		}
	}
}

func TestMigrationPath(t *testing.T) {
	for path, want := range map[string]bool{
		"component/database/20261012010000_x.go":                   true,
		"component/database/20261012010000_x_test.go":              false,
		"component/database/memory-nodes/migrations/0001.up.sql":   true,
		"component/database/memory-nodes/migrations/0001.down.sql": false,
		"component/database/memory-nodes/store.go":                 false,
		"component/other/database/x.go":                            false,
		"docs/notes.sql":                                           false,
	} {
		if got := MigrationPath(path); got != want {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
}

func TestRemovedPipelineSteps(t *testing.T) {
	before := []string{"checks.unit", "checks.lint", "images.edge"}
	after := []string{"checks.unit", "checks.new"}
	if got := strings.Join(RemovedPipelineSteps(before, after), ","); got != "checks.lint,images.edge" {
		t.Errorf("removed = %s", got)
	}
	if got := RemovedPipelineSteps(after, append(append([]string(nil), after...), "checks.more")); len(got) != 0 {
		t.Errorf("adding a step reads as removing one: %v", got)
	}
}

func TestVersionAfter(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"0.31.12", "v0.31.11", true},
		{"v0.31.11", "0.31.11", false},
		{"0.31.9", "0.31.11", false},
		{"1.0.0", "0.99.99", true},
		{"not", "0.31.11", false},
		{"0.31.11", "not", false},
	} {
		if got := VersionAfter(tc.a, tc.b); got != tc.want {
			t.Errorf("VersionAfter(%s, %s) = %v", tc.a, tc.b, got)
		}
	}
	if got := EngineRefVersion("PRODUCT=znas\nENGINE_REF=v0.31.11\nREGISTRY=x\n"); got != "v0.31.11" {
		t.Errorf("EngineRefVersion = %q", got)
	}
}

func TestHasDataGuard(t *testing.T) {
	if HasDataGuard([]DeployGuard{{Code: DeployGuardStep}, {Code: DeployGuardContract}}) {
		t.Error("a removed step and a contract were read as data")
	}
	for _, code := range []string{DeployGuardData, DeployGuardUnknown} {
		if !HasDataGuard([]DeployGuard{{Code: code}}) {
			t.Errorf("%s was not read as data", code)
		}
	}
}
