package packages

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// The first install has no registered product domain. Every later install
// does: analysis must inspect the replacement, not silently reuse live DSL.
func TestAnalysisValidatesAnInstalledDomainReplacement(t *testing.T) {
	live := validPackage()
	root := t.TempDir()
	for name, file := range live {
		if !strings.HasPrefix(name, "dsl/") {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(name, "dsl/")))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, file.Data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MEMQL_DSL_PATH", root)
	if got := memqldsl.MountRuntimeDomainsFromEnv(nil); len(got) != 1 || got[0] != "acme" {
		t.Fatalf("runtime mount: %v", got)
	}
	t.Cleanup(func() { memqldsl.UnregisterTree("acme") })

	for _, invalid := range []bool{false, true} {
		name := "valid replacement"
		if invalid {
			name = "invalid replacement"
		}
		t.Run(name, func(t *testing.T) {
			candidate := validPackage()
			candidate["dsl/acme/concepts.memql"] = file(validConcepts + strings.Replace(validConcepts, "concept widget", "concept addedWidget", 1))
			if invalid {
				candidate["dsl/acme/concepts.memql"] = file(bootRefusingConcepts)
			}
			report, err := Analyze(candidate, Options{})
			if len(report.DslDomains) != 1 || report.DslDomains[0].Domain != "acme" {
				t.Fatalf("installed domain disappeared from candidate report: %+v", report)
			}
			if invalid {
				if RefusalCode(err) != CodeDslRefusesBoot || report.OK {
					t.Fatalf("invalid replacement passed using live definitions: report=%+v err=%v", report, err)
				}
			} else if err != nil || !report.OK || report.DslDomains[0].Constructs["concept"] != 2 {
				t.Fatalf("valid replacement was not analyzed: report=%+v err=%v", report, err)
			}
			got, readErr := fs.ReadFile(memqldsl.Tree(), "acme/concepts.memql")
			if readErr != nil || string(got) != validConcepts {
				t.Fatalf("analysis changed the installed tree: %q, %v", got, readErr)
			}
		})
	}
}
