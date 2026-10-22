package ci

// The native Go selector partitions packages into database and non-database
// steps. Keep its tree list equal to the canonical script, and ensure each
// selected package reaches only its partition instead of running once per
// shard through a second literal package list (memql#3160, memql#5485).

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestDBTestsLaneTakesItsSetFromThePlanner(t *testing.T) {
	spec := nativePipeline(t)
	cmd := exec.Command("bash", "scripts/ci/db-gated-packages.sh", "--trees")
	cmd.Dir = RepoRoot()
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	canonical := strings.Fields(string(raw))
	if spec.Select == nil || len(canonical) == 0 {
		t.Fatal("missing database selection")
	}
	declared := slices.Clone(spec.Select.DBGated)
	slices.Sort(canonical)
	slices.Sort(declared)
	if !slices.Equal(canonical, declared) {
		t.Fatalf("native DB trees %v differ from canonical %v", declared, canonical)
	}
	for _, tc := range []struct{ name, filter string }{{"db-tests", "db-gated"}, {"go-tests", "not-db-gated"}} {
		step := nativeRequiredStep(t, tc.name)
		if step.Packages != "affected" || step.Only != tc.filter || step.Shards < 1 {
			t.Errorf("%s does not consume its disjoint selected partition: %+v", tc.name, step)
		}
	}
	db := nativeRequiredStep(t, "db-tests")
	if !slices.Contains(db.Services, "postgres") || !strings.Contains(db.Run, "export MEMQL_REQUIRE_DB=1\n") {
		t.Error("native DB tests must fail if their isolated database is unreachable")
	}
	if !strings.Contains(db.Run, "python3 scripts/ci/pipeline-db-tests.py\n") || strings.Contains(db.Run, "go test") {
		t.Error("database packages must come only from the shared budget runner's MEMQL_PACKAGES selection")
	}
	// The untagged DB selection cannot exercise agent-only persistence tests.
	// The tagged lane must execute them against a required database, uncached.
	agent := nativeRequiredStep(t, "test-agent")
	if !slices.Contains(agent.Services, "postgres") || !strings.Contains(agent.Run, "export MEMQL_REQUIRE_DB=1\n") || !strings.Contains(agent.Run, "export MEMQL_DATABASE_DSN=") {
		t.Error("agent-tagged persistence tests must fail if their isolated database is unreachable")
	}
	if !strings.Contains(agent.Run, "go test -count=1 -tags agent -timeout=300s ./app/ ./integrations/agent/ ./integrations/agent/worker/ ./integrations/pipelinesteps/ ./component/node/\n") {
		t.Error("agent-tagged tests must retain their package set and execute uncached")
	}
	goStep := nativeRequiredStep(t, "go-tests")
	if !strings.Contains(goStep.Run, "go test -count=1 -timeout=600s $MEMQL_PACKAGES\n") {
		t.Error("non-database tests no longer execute the native selected packages uncached")
	}
}
