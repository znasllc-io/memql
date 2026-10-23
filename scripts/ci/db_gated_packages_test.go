package ci

// The native Go selector partitions packages into database and non-database
// steps. Keep its tree list equal to the canonical script, and ensure each
// selected package reaches only its partition instead of running once per
// shard through a second literal package list (memql#3160, memql#5485).

import (
	"go/build"
	"os/exec"
	"path/filepath"
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
	agent := nativeRequiredStep(t, "test-agent")
	if !slices.Contains(agent.Services, "postgres") || !strings.Contains(agent.Run, "export MEMQL_REQUIRE_DB=1\n") ||
		!strings.Contains(agent.Run, "export MEMQL_DATABASE_DSN='postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable'\n") {
		t.Error("agent-tagged persistence tests must require their isolated database instead of skipping")
	}
	if agent.Timeout != "45m" || agent.CPUMilli != 2750 || !strings.Contains(agent.Run, "export GOFLAGS=-p=1 GOMEMLIMIT=2GiB GOGC=100 GOMAXPROCS=3\n") {
		t.Error("agent-tagged DB tests lost their bounded CPU, memory or overall timeout")
	}
	for _, command := range []string{
		"go test -tags agent -count=1 -timeout=900s -v ./app/\n",
		"go test -tags agent -count=1 -timeout=300s ./integrations/agent/ ./integrations/agent/worker/ ./integrations/pipelinesteps/ ./component/node/\n",
	} {
		if !strings.Contains(agent.Run, command) {
			t.Errorf("agent-tagged persistence tests must run all five packages uncached with separate app and worker budgets; missing %q", command)
		}
	}
	goStep := nativeRequiredStep(t, "go-tests")
	if !strings.Contains(goStep.Run, "go test -count=1 -timeout=600s $MEMQL_PACKAGES\n") {
		t.Error("non-database tests no longer execute the native selected packages uncached")
	}
}

// Untagged DB shards cannot execute an agent-only file. Discover these files
// from the same canonical DB trees rather than keeping another package list.
func TestAgentOnlyDBTestsReachRequiredTaggedLane(t *testing.T) {
	root := RepoRoot()
	command := exec.Command("bash", "scripts/ci/db-gated-packages.sh", "--trees")
	command.Dir = root
	raw, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	trees := strings.Fields(string(raw))
	command = exec.Command("git", "ls-files", "--", "*_test.go")
	command.Dir = root
	raw, err = command.Output()
	if err != nil {
		t.Fatal(err)
	}
	untagged := build.Default
	untagged.GOOS, untagged.GOARCH, untagged.CgoEnabled = "linux", "amd64", true
	untagged.BuildTags = nil
	agent := untagged
	agent.BuildTags = []string{"agent"}
	packages := map[string]bool{}
	for _, path := range strings.Fields(string(raw)) {
		inDBTree := slices.ContainsFunc(trees, func(tree string) bool { return strings.HasPrefix(path, tree+"/") })
		if !inDBTree {
			continue
		}
		dir, name := filepath.Dir(path), filepath.Base(path)
		withAgent, err := agent.MatchFile(filepath.Join(root, dir), name)
		if err != nil {
			t.Fatal(err)
		}
		withoutAgent, err := untagged.MatchFile(filepath.Join(root, dir), name)
		if err != nil {
			t.Fatal(err)
		}
		if withAgent && !withoutAgent {
			packages["./"+filepath.ToSlash(dir)+"/"] = true
		}
	}
	if !packages["./app/"] {
		t.Fatal("agent-only DB coverage discovery missed the app persistence fixtures")
	}
	step := nativeRequiredStep(t, "test-agent")
	covered := map[string]bool{}
	for _, line := range strings.Split(step.Run, "\n") {
		if !strings.HasPrefix(line, "go test -tags agent -count=1 ") {
			continue
		}
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, "./") {
				covered[field] = true
			}
		}
	}
	for pkg := range packages {
		if !covered[pkg] {
			t.Errorf("agent-only DB tests in %s cannot run in untagged shards and are absent from the required tagged lane", pkg)
		}
	}
}
