package dslconformance

// belongs_to_inventory_test.go -- every concept the engine ships says which
// account its rows belong to, or that it is cluster-wide (design record
// docs/superpowers/specs/2026-11-12-account-row-ownership-design.md).
//
// WHY A TEST AND NOT A LOAD REFUSAL. An undeclared concept loads: tests define
// several hundred concepts inline and register them the way a pack does, and a
// load that refused them would refuse every one of those fixtures. So the
// engine's own tree is held here instead, where one failure names every
// offender at once -- the shape of the display-card inventory beside this file.
//
// WHAT AN UNDECLARED CONCEPT WOULD COST. The engine's stamp, the assignment of
// existing rows, the account inventory, and later export and sign-in all ask
// the declaration whose row this is. A concept that says nothing is treated as
// cluster-wide: its rows are never stamped and would never leave in an export
// of the account they were made for. That is safe in the direction that
// matters (nothing leaks) and silently incomplete in the other, which is the
// kind of gap a build gate exists to close.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/repowalk"
)

// belongsToRe matches the declaration on a line of a concept's preamble. A
// commented-out line does not count.
var belongsToRe = regexp.MustCompile(`^[ \t]*@belongsTo\(`)

// shippedConceptRoots are the trees whose concepts the engine ships: its own
// DSL, every storefront pack, and the fleet bundle this repository deploys.
// Paths are relative to this package's directory.
var shippedConceptRoots = []string{
	filepath.Join("..", "..", "dsl"),
	filepath.Join("..", "..", "packs"),
	filepath.Join("..", "..", "deploy", "fleet", "dsl"),
}

type belongsToEntry struct {
	file     string
	name     string
	line     int
	declared bool
}

func scanBelongsTo(t *testing.T) []belongsToEntry {
	t.Helper()
	var out []belongsToEntry
	for _, root := range shippedConceptRoots {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				// The shared skip list first (nested checkouts, vendored trees).
				// Then two of this walk's own: `_reference` holds authoring
				// skeletons that are not loaded, and `testdata` holds stand-ins
				// for somebody else's bundle.
				if repowalk.SkipDir(d.Name()) || d.Name() == "_reference" || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".memql") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			lines := strings.Split(string(raw), "\n")
			for i, line := range lines {
				m := conceptHeaderRe.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				declared := false
				for _, pre := range preambleFor(lines, i) {
					if belongsToRe.MatchString(pre) {
						declared = true
						break
					}
				}
				out = append(out, belongsToEntry{file: path, name: m[1], line: i + 1, declared: declared})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	return out
}

// TestEveryShippedConceptDeclaresBelongsTo is the gate.
func TestEveryShippedConceptDeclaresBelongsTo(t *testing.T) {
	entries := scanBelongsTo(t)
	if len(entries) < 200 {
		t.Fatalf("scanned %d concepts -- the walk stopped finding the tree and every assertion below is vacuous", len(entries))
	}
	var missing []string
	for _, e := range entries {
		if !e.declared {
			missing = append(missing, e.file+":"+e.name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these concepts declare no @belongsTo. Say whose rows they are:\n"+
			"  @belongsTo(account)                   the rows belong to an account (add `accountId string`)\n"+
			"  @belongsTo(account, via=\"<field>\")    the rows follow the row that field names\n"+
			"  @belongsTo(account, shared)           and a row with no account is the cluster's, offered to every account\n"+
			"  @belongsTo(account, stated)           the concept's own mutations state the account\n"+
			"  @belongsTo(cluster)                   cluster-wide: people, sign-in, nodes, configuration\n"+
			"(docs/public/language/memql.md, \"Which account a row belongs to\"):\n  %s",
			strings.Join(missing, "\n  "))
	}
}

// TestNoConceptSharesWithAnAccountWhileClusterWide holds the two declarations
// together across the loaded tree: a concept that opens its rows to an
// account's members through @rowAuthz(account=...) has rows that belong to
// accounts, so it cannot also say it is cluster-wide. The concept translator
// refuses the pair on one concept; this is the same rule read off the registry,
// so a future path that builds a concept some other way is still held to it.
func TestNoConceptSharesWithAnAccountWhileClusterWide(t *testing.T) {
	for _, c := range loadedConcepts(t) {
		if c.RowAuthz == nil || strings.TrimSpace(c.RowAuthz.Account) == "" {
			continue
		}
		if c.BelongsTo != nil && c.BelongsTo.Scope == langparser.BelongsToCluster {
			t.Errorf("%s shares its rows with an account (@rowAuthz(account=%q)) and declares @belongsTo(cluster)",
				c.Name, c.RowAuthz.Account)
		}
	}
}
