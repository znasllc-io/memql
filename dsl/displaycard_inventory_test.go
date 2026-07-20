package dsl

import (
	"io"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// TestDisplayCardInventory walks every `concept ... {` declaration in
// the DSL tree and logs which concepts carry the `@displayCard(...)`
// annotation shipped by memql#160 and which don't.
//
// Informational only -- does NOT hard-fail on missing annotations.
// The hard-fail variant (with `// @no-displayCard: <reason>`
// exemption comments) is the natural follow-up once every internal-
// only concept has been classified explicitly. The initial annotation
// pass under memql#160 covered the high-value user-browseable
// concepts (agent, space, plan, task, user, invitation, auditEvent,
// document, knowledgeDomain); the remaining concepts are still in
// the unannotated bucket and the test will surface them as the
// natural to-do list.
//
// Run via `go test ./dsl/ -run TestDisplayCardInventory -v`.
func TestDisplayCardInventory(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	headerRe := regexp.MustCompile(`(?m)^[ \t]*concept[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

	type row struct {
		domain string
		name   string
		file   string
		annot  bool
	}
	var rows []row

	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		src := string(raw)
		matches := headerRe.FindAllStringSubmatchIndex(src, -1)
		domain := strings.SplitN(p, "/", 2)[0]
		for _, m := range matches {
			name := src[m[2]:m[3]]
			// Walk back through the immediate preamble (comments +
			// annotations + blank lines). The "@displayCard" string
			// is what we're looking for; check the 800 chars
			// preceding the `concept` keyword since concept
			// preambles can be quite long.
			start := m[0]
			lookback := 800
			if start < lookback {
				lookback = start
			}
			preamble := src[start-lookback : start]
			hasAnnot := strings.Contains(preamble, "@displayCard")
			rows = append(rows, row{
				domain: domain,
				name:   name,
				file:   p,
				annot:  hasAnnot,
			})
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].domain != rows[j].domain {
			return rows[i].domain < rows[j].domain
		}
		return rows[i].name < rows[j].name
	})

	var annotated, missing int
	byDomain := map[string]struct{ annot, miss int }{}
	for _, r := range rows {
		entry := byDomain[r.domain]
		if r.annot {
			annotated++
			entry.annot++
		} else {
			missing++
			entry.miss++
		}
		byDomain[r.domain] = entry
	}

	t.Logf("\n=== @displayCard annotation coverage (informational) ===")
	t.Logf("Concepts with @displayCard:    %3d", annotated)
	t.Logf("Concepts without @displayCard: %3d", missing)
	t.Logf("")
	t.Logf("%-15s %6s %6s", "domain", "annot", "miss")
	domains := make([]string, 0, len(byDomain))
	for d := range byDomain {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	for _, d := range domains {
		e := byDomain[d]
		t.Logf("%-15s %6d %6d", d, e.annot, e.miss)
	}
	t.Logf("")
	if missing > 0 {
		t.Logf("Unannotated concepts (candidates for the next pass; system-internal concepts that have no user-browseable surface are expected to stay unannotated):")
		for _, r := range rows {
			if !r.annot {
				t.Logf("  %s:%s", r.domain, r.name)
			}
		}
		t.Logf("")
		t.Logf("Add @displayCard(...) on a concept to opt in; see memql#160 for the slot semantics.")
	}
}
