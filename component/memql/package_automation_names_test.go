package memql

import (
	"testing"
	"testing/fstest"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// An automation is found by its bare name across the whole tree, so a
// candidate package's automation may not take a name another domain declares --
// and above all not one of the engine's own.

func automationFile(names ...string) *fstest.MapFile {
	src := ""
	for _, name := range names {
		src += "/// An automation.\n@trigger(schedule=\"0 0 3 * * *\")\nautomation " + name + " {\n  run := logic zzNoop()\n}\n\n"
	}
	return &fstest.MapFile{Data: []byte(src)}
}

func TestAnalyzePackageDSLReportsAnAutomationTakingAnEngineName(t *testing.T) {
	const engineName = "emailStatisticsRollupSweep" // dsl/campaigns/automations.memql
	result, err := AnalyzePackageDSL(discardLogger(), withLanguageLines(fstest.MapFS{
		"zzpkg/automations.memql": automationFile(engineName, "zzPkgOwnSweep"),
	}))
	if err != nil {
		t.Fatalf("AnalyzePackageDSL: %v", err)
	}
	if len(result.AutomationNameConflicts) != 1 {
		t.Fatalf("want one conflict, got %+v", result.AutomationNameConflicts)
	}
	c := result.AutomationNameConflicts[0]
	if c.Name != engineName || c.File != "zzpkg/automations.memql" || !c.Engine || c.EngineFile != "campaigns/automations.memql" {
		t.Fatalf("conflict %+v, want %s in zzpkg/automations.memql, the engine's campaigns/automations.memql", c, engineName)
	}
	for _, d := range result.Diagnostics {
		t.Errorf("boot refuses nothing for a borrowed name, so it is no diagnostic: %+v", d)
	}
}

func TestAnalyzePackageDSLAcceptsAnAutomationWithItsOwnName(t *testing.T) {
	result, err := AnalyzePackageDSL(discardLogger(), withLanguageLines(fstest.MapFS{
		"zzpkg/automations.memql": automationFile("zzPkgOwnSweep"),
	}))
	if err != nil {
		t.Fatalf("AnalyzePackageDSL: %v", err)
	}
	if len(result.AutomationNameConflicts) != 0 {
		t.Fatalf("an automation with a name of its own conflicted: %+v", result.AutomationNameConflicts)
	}
}

// Two domains that are not the engine's collide as well: whichever is read
// later is what the name reaches. Another installed domain's file is not named
// in the conflict -- it may be another package's.
func TestAnalyzePackageDSLReportsANameAnotherDomainDeclares(t *testing.T) {
	installed := withLanguageLine(fstest.MapFS{"automations.memql": automationFile("zzInstalledSweep")})
	memqldsl.RegisterTree("zzinstalled", installed)
	t.Cleanup(func() { memqldsl.UnregisterTree("zzinstalled") })

	result, err := AnalyzePackageDSL(discardLogger(), withLanguageLines(fstest.MapFS{
		"zzpkg/automations.memql":        automationFile("zzInstalledSweep", "zzTwiceSweep"),
		"zzpkg/nested/automations.memql": automationFile("zzTwiceSweep"),
	}))
	if err != nil {
		t.Fatalf("AnalyzePackageDSL: %v", err)
	}
	want := map[string]string{
		"zzpkg/automations.memql\x00zzInstalledSweep":    "",
		"zzpkg/automations.memql\x00zzTwiceSweep":        "",
		"zzpkg/nested/automations.memql\x00zzTwiceSweep": "",
	}
	if len(result.AutomationNameConflicts) != len(want) {
		t.Fatalf("got %+v, want %d conflicts", result.AutomationNameConflicts, len(want))
	}
	for _, c := range result.AutomationNameConflicts {
		if _, ok := want[c.File+"\x00"+c.Name]; !ok || c.Engine || c.EngineFile != "" {
			t.Errorf("unexpected conflict %+v", c)
		}
	}
}
