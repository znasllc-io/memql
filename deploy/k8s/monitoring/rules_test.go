// Package monitoring holds the gates for the alert rules (memql#4468, epic
// memql#4496).
//
// An alert rule has a failure mode a manifest does not: it can be SYNTACTICALLY
// PERFECT, deploy cleanly, show up in Prometheus, and still be incapable of
// ever firing. Nothing reports that. The rule sits green and is read as
// coverage of the thing it names, which is strictly worse than having no rule
// at all -- a missing alert prompts someone to write one.
//
// Both gates here exist because that already happened once, to the single rule
// whose job was to catch a wrong backup destination on day one (memql#4460).
package monitoring

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type promRuleFile struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert string `yaml:"alert"`
				Expr  string `yaml:"expr"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	} `yaml:"spec"`
}

// loadRules reads every PrometheusRule manifest in this directory.
func loadRules(t *testing.T) map[string]promRuleFile {
	t.Helper()
	paths, err := filepath.Glob("prometheusrule-*.yaml")
	if err != nil {
		t.Fatalf("globbing rule files: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no prometheusrule-*.yaml found -- this gate is looking in the wrong place")
	}
	out := make(map[string]promRuleFile, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		var f promRuleFile
		if err := yaml.Unmarshal(b, &f); err != nil {
			t.Fatalf("parsing %s: %v", p, err)
		}
		if f.Kind != "PrometheusRule" {
			continue
		}
		out[p] = f
	}
	return out
}

// archiverTimestampVsZero matches a comparison of a CNPG archiver timestamp
// against literal 0 -- the unsatisfiable form.
var archiverTimestampVsZero = regexp.MustCompile(
	`cnpg_pg_stat_archiver_last_(?:archived|failed)_time\s*[=!<>]=?\s*0(?:\b|$)`)

// TestNoArchiverRuleComparesAgainstZero pins the SENTINEL.
//
// CNPG's built-in pg_stat_archiver query coalesces a NULL timestamp to MINUS
// ONE, not to zero:
//
//	COALESCE(EXTRACT(EPOCH FROM last_archived_time), -1) AS last_archived_time
//
// A zero would mean the archiver last succeeded at 1970-01-01T00:00:00Z, which
// no cluster reports. So `... == 0` is unsatisfiable, and that is exactly what
// MemqlDatabaseWALNeverArchived tested until memql#4468 -- the one rule written
// for the day-one wrong-destinationPath case, unable to fire on day one or any
// other day, while a real instance archived nothing for its entire life.
func TestNoArchiverRuleComparesAgainstZero(t *testing.T) {
	var checked int
	for path, f := range loadRules(t) {
		for _, g := range f.Spec.Groups {
			for _, r := range g.Rules {
				if r.Alert == "" {
					continue
				}
				checked++
				if archiverTimestampVsZero.MatchString(r.Expr) {
					t.Errorf("%s: alert %q compares a CNPG archiver timestamp against 0, which is "+
						"UNSATISFIABLE -- the never-archived sentinel is -1, not 0 "+
						"(COALESCE(EXTRACT(EPOCH FROM last_archived_time), -1)). This rule can never "+
						"fire, and its presence reads as coverage it does not provide (memql#4468). "+
						"Expression:\n%s", path, r.Alert, strings.TrimSpace(r.Expr))
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no alerts examined -- a gate that checked nothing must not pass")
	}
	t.Logf("checked %d alert expression(s)", checked)
}

// deadUnderPluginBackups are the CNPG gauges that DO NOT UPDATE at all on this
// deployment.
//
// CloudNativePG deprecated all three in v1.26, and its release note is explicit:
// they "will no longer update when using plugin-based backups (e.g. Barman
// Cloud via CNPG-I)". MemQL uses exactly that plugin, deliberately, in every
// environment (deploy/cnpg/install/kustomization.yaml pins
// plugin-barman-cloud and the component comment records why the in-tree
// stanza was rejected).
//
// So on this deployment they are pinned at zero forever, and a rule built on
// one of them fires permanently or never depending only on which way the
// comparison points. Both outcomes are worse than the absent rule, and the
// permanent-fire case is the one memql#4491 is about: health that is always red
// carries no signal.
//
// The backup-age check that these metrics LOOK like they would support lives in
// scripts/deploy/db-backup-verify.sh, which reads the object store directly.
var deadUnderPluginBackups = []string{
	"cnpg_collector_last_available_backup_timestamp",
	"cnpg_collector_first_recoverability_point",
	"cnpg_collector_last_failed_backup_timestamp",
}

func TestNoRuleUsesMetricsThatArePinnedAtZeroHere(t *testing.T) {
	var checked int
	for path, f := range loadRules(t) {
		for _, g := range f.Spec.Groups {
			for _, r := range g.Rules {
				if r.Alert == "" {
					continue
				}
				checked++
				for _, m := range deadUnderPluginBackups {
					if strings.Contains(r.Expr, m) {
						t.Errorf("%s: alert %q uses %s, which CloudNativePG deprecated in v1.26 and "+
							"which DOES NOT UPDATE under plugin-based backups -- the mode MemQL runs "+
							"in everywhere. It is pinned at zero here, so this rule fires forever or "+
							"never. Verify backups by reading the object store instead: "+
							"`make db-backup-verify ACCOUNT=<account>` (memql#4468).",
							path, r.Alert, m)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no alerts examined -- a gate that checked nothing must not pass")
	}
	t.Logf("checked %d alert expression(s) against %d dead metric(s)", checked, len(deadUnderPluginBackups))
}
