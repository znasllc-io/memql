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

// Over the migrations this repository really has, because a rule that reads text is
// only as good as the text it has seen.
func TestClassifyRealMigrations(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join("..", "database", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	for name, want := range map[string]MigrationKind{
		"20261012010000_email_provider_lookup_index.go":       MigrationIndexes,
		"20261030000000_site_access_lookup_indexes.go":        MigrationIndexes,
		"20261029100000_work_run_goal_lookup_index.go":        MigrationIndexes,
		"20260912000000_site_owner_restamp.go":                MigrationData,
		"20260921000000_module_readiness_history_collapse.go": MigrationData,
	} {
		if got := ClassifyMigration(read(name)); got != want {
			t.Errorf("%s reads as %s, want %s", name, got, want)
		}
	}
	matches, err := filepath.Glob(filepath.Join("..", "database", "memory-nodes", "migrations", "*.up.sql"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no SQL migrations found: %v", err)
	}
	indexes, data := 0, 0
	for _, path := range matches {
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
	if data == 0 {
		t.Errorf("none of %d SQL migrations reads as a change of tables or data", len(matches))
	}
	t.Logf("%d SQL migrations: %d indexes only, %d tables or data", len(matches), indexes, data)
}

func TestClassifyMigrationText(t *testing.T) {
	wrap := func(body string) string {
		return "m.MustRegister(func(ctx context.Context, db *bun.DB) error {\n" + body + "\n})"
	}
	for name, tc := range map[string]struct {
		source string
		want   MigrationKind
	}{
		"a helper beside the migrations":   {`func quoteIdentifier(s string) string { return s }`, MigrationNone},
		"an index":                         {wrap("db.ExecContext(ctx, `CREATE INDEX CONCURRENTLY IF NOT EXISTS i ON t (a)`)"), MigrationIndexes},
		"a unique index":                   {wrap("db.ExecContext(ctx, `create unique index i on t (a)`)"), MigrationIndexes},
		"a column":                         {wrap("db.ExecContext(ctx, `ALTER TABLE t ADD COLUMN c text`)"), MigrationData},
		"a table":                          {wrap("db.ExecContext(ctx, `CREATE TABLE t (a int)`)"), MigrationData},
		"rows":                             {wrap("db.ExecContext(ctx, `UPDATE \"MemoryNodes\" SET a = 1`)"), MigrationData},
		"an index and rows":                {wrap("db.ExecContext(ctx, `CREATE INDEX i ON t (a); DELETE FROM t`)"), MigrationData},
		"a helper it cannot read":          {wrap("return restampEverything(ctx, db)"), MigrationData},
		"a change named only in a comment": {wrap("// DELETE FROM t would be wrong\ndb.ExecContext(ctx, `CREATE INDEX i ON t (a)`)"), MigrationIndexes},
	} {
		if got := ClassifyMigration(tc.source); got != tc.want {
			t.Errorf("%s reads as %s, want %s", name, got, tc.want)
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

func TestChangedWorkerContracts(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", WorkerContractPath))
	if err != nil {
		t.Fatal(err)
	}
	real := WorkerContracts(string(source))
	if real["workerHost.pipeline_step"] == "" {
		t.Fatalf("the real contracts file reads as %v", real)
	}
	if changed := ChangedWorkerContracts(string(source), string(source)); len(changed) != 0 {
		t.Errorf("the same source differs: %v", changed)
	}
	before := `contracts := map[string]int{"workerHost.pipeline_step": 5, "workerHost.fs_read": 2}`
	after := `contracts := map[string]int{"workerHost.pipeline_step": 6}
contracts["workerComputer.input_target"] = 1`
	got := strings.Join(ChangedWorkerContracts(before, after), "; ")
	for _, want := range []string{"workerHost.pipeline_step 5 to 6", "workerComputer.input_target none to 1", "workerHost.fs_read 2 removed"} {
		if !strings.Contains(got, want) {
			t.Errorf("changes %q lack %q", got, want)
		}
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
