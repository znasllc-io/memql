package parser

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestBodyClausesMatchTheRewriterCaseArms pins the query and mutation rows of
// the clause table to the `case` arms of the struct-body parsers that accept
// them -- the arms the grammar-surface digest reads -- so a clause added to
// or removed from either parser without the table fails here by name.
func TestBodyClausesMatchTheRewriterCaseArms(t *testing.T) {
	raw, err := os.ReadFile("rewriter.go")
	if err != nil {
		t.Fatalf("read rewriter.go: %v", err)
	}
	src := string(raw)
	litRE := regexp.MustCompile(`"([^"\\]*)"`)
	caseLiterals := func(fn string) map[string]bool {
		out := map[string]bool{}
		for _, line := range strings.Split(functionSource(t, src, fn), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "case ") {
				continue
			}
			for _, m := range litRE.FindAllStringSubmatch(line, -1) {
				if m[1] != "" {
					out[m[1]] = true
				}
			}
		}
		return out
	}

	for _, tc := range []struct {
		fn, keyword string
		// adjust maps the table onto the arms: a clause the parser reads
		// before its switch (true = add to the table side), or an arm that
		// only exists to REFUSE a retired clause (false = add to the arm side).
		beforeSwitch, refusalArms []string
	}{
		// The query's `args { }` block is cut out by argsBlockHeader before
		// the line switch runs; its `concept` arm refuses the retired inline
		// concept line.
		{"parseStructQueryBody", "query", []string{"args"}, []string{"concept"}},
		{"parseStructMutationBody", "mutate", nil, nil},
	} {
		arms := caseLiterals(tc.fn)
		for _, c := range tc.beforeSwitch {
			arms[c] = true
		}
		for _, c := range tc.refusalArms {
			delete(arms, c)
		}
		table := map[string]bool{}
		for _, c := range BodyClauses(tc.keyword) {
			table[c] = true
		}
		if got, want := sortedKeySet(table), sortedKeySet(arms); got != want {
			t.Errorf("%s: BodyClauses(%q) = %s, but %s's case arms accept %s -- update bodyClauseTable (body_clauses.go) in the same change as the parser",
				tc.keyword, tc.keyword, got, tc.fn, want)
		}
	}
}

// bodyClauseFixtures is, per construct and clause, the smallest construct
// that uses the clause. The automation `precondition` block has no fixture:
// component/automations extracts it before the rewriter runs, so the parser
// alone never sees one.
var bodyClauseFixtures = map[string]map[string]string{
	"query": {
		"args":     "query thing probe {\n  args {\n    id string\n  }\n  filter row.id == args.id\n}",
		"filter":   "query thing probe {\n  filter row.id != \"\"\n}",
		"shape":    "query thing probe {\n  filter row.id != \"\"\n  shape probeCard\n}",
		"sort":     "query thing probe {\n  filter row.id != \"\"\n  sort \"row.createdAt\", \"desc\"\n}",
		"paginate": "query thing probe {\n  filter row.id != \"\"\n  paginate 25\n}",
		"asOf":     "query thing probe {\n  filter row.id != \"\"\n  asOf latest\n}",
		"count":    "query thing probe {\n  filter row.id != \"\"\n  count\n}",
	},
	"mutate": {
		"args":   "mutate thing probe {\n  args {\n    id string!\n  }\n  insert {\n    id: args.id\n  }\n}",
		"insert": "mutate thing probe {\n  insert {\n    id: \"x\"\n  }\n}",
		"update": "mutate thing probe {\n  update {\n    id: \"x\"\n  }\n}",
		"accept": "mutate thing probe {\n  args {\n    name string!\n  }\n  accept { name }\n}",
		"stamp":  "mutate thing probe {\n  args {\n    name string!\n  }\n  accept { name }\n  stamp { createdAt: now }\n}",
	},
	"logic": {
		"args": "logic probe {\n  args {\n    x string\n  }\n  body {\n    return args.x\n  }\n}",
		"body": "logic probe {\n  body {\n    return 1\n  }\n}",
	},
	"automation": {
		"args": "automation probe {\n  args {\n    x any\n  }\n  step run {\n    mutation createThing (id: \"x\")\n  }\n}",
		"step": "automation probe {\n  step run {\n    mutation createThing (id: \"x\")\n  }\n}",
	},
	"action": {
		"args": "action probe {\n  args {\n    x string\n  }\n  capability script(script: args.x)\n}",
	},
	"capability": {
		"args": "capability integration.probe.run {\n  args {\n    x string\n  }\n}",
	},
	"provider": {
		"params": "provider probe {\n  params {\n    contextWindow 1\n  }\n}",
		"auth":   "provider probe {\n  auth {\n    key \"x\"\n  }\n}",
	},
}

// bodyClauseRefusals is, per construct, a body clause the construct does NOT
// accept, to prove the table is not merely a subset of what parses.
var bodyClauseRefusals = map[string]string{
	"query":      "query thing probe {\n  filter row.id != \"\"\n  project name\n}",
	"mutate":     "mutate thing probe {\n  delete {\n    id: \"x\"\n  }\n}",
	"automation": "automation probe {\n  body {\n    return 1\n  }\n}",
	"action":     "action probe {\n  params {\n    x string\n  }\n  capability script(script: \"x\")\n}",
	"capability": "capability integration.probe.run {\n  body {\n  }\n}",
	"provider":   "provider probe {\n  models {\n  }\n}",
}

// TestBodyClausesMatchWhatTheParsersAccept: every clause in the table parses
// on the authored path, and a clause the table does not list is refused.
func TestBodyClausesMatchWhatTheParsersAccept(t *testing.T) {
	for keyword, clauses := range bodyClauseTable {
		for _, clause := range clauses {
			if keyword == "automation" && clause == "precondition" {
				continue // stripped by component/automations before the rewriter runs
			}
			src, ok := bodyClauseFixtures[keyword][clause]
			if !ok {
				t.Errorf("%s: no fixture for the %q clause -- add one to bodyClauseFixtures", keyword, clause)
				continue
			}
			if !grammarSurfaceOutcome(src) {
				normalised, nerr := NormaliseAll(src)
				_, perr := ParseFile(normalised)
				t.Errorf("%s: the %q clause the table lists is refused:\n%s\nrewrite: %v\nparse: %v", keyword, clause, src, nerr, perr)
			}
		}
		if bad, ok := bodyClauseRefusals[keyword]; ok && grammarSurfaceOutcome(bad) {
			t.Errorf("%s: a clause the table does not list parses:\n%s", keyword, bad)
		}
	}
	for keyword := range bodyClauseFixtures {
		if _, ok := bodyClauseTable[keyword]; !ok {
			t.Errorf("fixture for %q, which the table does not list", keyword)
		}
	}
}

// TestLineClausesAreBodyClauses: every line clause is a clause some body
// accepts, so the editor never inserts a clause no construct takes.
func TestLineClausesAreBodyClauses(t *testing.T) {
	all := map[string]bool{}
	for _, clauses := range bodyClauseTable {
		for _, c := range clauses {
			all[c] = true
		}
	}
	for c := range lineClauses {
		if !all[c] {
			t.Errorf("line clause %q is in no construct's body", c)
		}
	}
	if IsLineClause("args") || !IsLineClause("filter") {
		t.Error("args opens a block and filter takes its line")
	}
}

func sortedKeySet(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
