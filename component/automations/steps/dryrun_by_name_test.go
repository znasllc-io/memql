package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// A dry run by name runs the tree's own copy of an automation, so a name the
// tree does not hold has nothing to run: the request is refused with a
// sentence that says so and what to do, and nothing is compiled or executed.
func TestADryRunByNameOfANameTheTreeDoesNotHoldIsRefused(t *testing.T) {
	eng := sharedEmbeddedEngine(t)
	const unknown = "zzNoSuchAutomationForDryRun"
	report, err := runBundleDryRun(context.Background(), eng, memql.DryRunRequest{AutomationName: unknown})
	if err == nil {
		t.Fatalf("a dry run by a name the tree does not hold was not refused: %+v", report)
	}
	for _, want := range []string{unknown, "by name", "text"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if report.OK || len(report.Trace) != 0 || report.RanAs != "" {
		t.Errorf("a refused dry run reports a run: %+v", report)
	}
}

// A request that names no automation and carries no text asks for nothing.
func TestADryRunWithNeitherANameNorATextIsRefused(t *testing.T) {
	eng := sharedEmbeddedEngine(t)
	for _, req := range []memql.DryRunRequest{{}, {AutomationName: "  ", AutomationSource: " \n"}} {
		report, err := runBundleDryRun(context.Background(), eng, req)
		if err == nil || !strings.Contains(err.Error(), "name") || !strings.Contains(err.Error(), "text") {
			t.Errorf("a dry run of nothing was not refused with a sentence: %+v, %v", report, err)
		}
	}
}

// No run executes a @disabled automation, so a dry run by name has nothing to
// predict and refuses it as every other run by name does.
func TestADryRunByNameOfADisabledAutomationIsRefused(t *testing.T) {
	eng := sharedEmbeddedEngine(t)
	const domain, name = "zzdryrundisabled", "zzDryRunDisabledFixture"
	root := t.TempDir()
	dir := filepath.Join(root, domain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string]string{
		"memql.toml": "memql = \"1.0\"\nedition = \"2026\"\n",
		"automations.memql": "/// A fixture nothing runs.\n@disabled\n@trigger(schedule=\"0 0 3 * * *\")\n" +
			"automation " + name + " {\n  publish \"zz.dryrun.disabled\" { from: \"fixture\" }\n}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MEMQL_DSL_PATH", root)
	if got := memqldsl.MountRuntimeDomainsFromEnv(nil); len(got) != 1 || got[0] != domain {
		t.Fatalf("mounted %v, want [%s]", got, domain)
	}
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	report, err := runBundleDryRun(context.Background(), eng, memql.DryRunRequest{AutomationName: name})
	if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "@disabled") {
		t.Fatalf("a dry run by name of a @disabled automation was not refused with a sentence: %+v, %v", report, err)
	}
	if len(report.Trace) != 0 || report.RanAs != "" {
		t.Errorf("a refused dry run reports a run: %+v", report)
	}
}
