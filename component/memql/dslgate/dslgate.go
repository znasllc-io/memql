// Package dslgate holds the DSL CONTRACT gates -- the checks that must hold
// for any .memql tree the engine loads, wherever that tree came from.
//
// # Why this package exists (memql#3629)
//
// Every DSL correctness gate in this repo was a Go test walking dsl.Tree():
// TestNoRetiredOperatorForms, TestPerRowAuthzClassification,
// TestFilterIntrinsicsUseRowNamespace, TestSortKeysUseRowNamespace,
// TestAdminGateIsATopLevelConjunct. A product's DSL arrives at RUNTIME, from a
// different repo, via MEMQL_DSL_PATH -> dsl.MountRuntimeDomainsFromEnv, and
// runs none of them. Under platform consolidation (memql#2472) that is the
// PRIMARY delivery path -- the engine ships product-agnostic and a product IS a
// DSL bundle plus a client -- so the tree that most needs the gates had none,
// while the tree every gate did cover was the one written by the people who
// wrote the gates.
//
// Two of those gates are the pair that would have caught memql#3612, where a
// `,` inside parentheses is read by the engine as OR and turns an ownership
// conjunct into a disjunct. That is an authorization bypass, and both gates
// that refuse it were exactly the ones a product bundle did not run.
//
// # The split
//
// A CONTRACT gate protects something the engine's behaviour depends on:
// authorization scoping, an operator the engine still honours but authors must
// not write, the namespace that decides whether a filter compiles to a table
// column or a JSONB path. Those live here and run at LOAD time, so they cover
// the embedded core tree, every RegisterTree overlay, and a MEMQL_DSL_PATH
// bundle identically.
//
// A HOUSE-STYLE gate protects consistency of authoring -- naming conventions,
// redundant annotations, longhand spellings with a canonical short form. Those
// stay as tests in test/dslconformance. Failing a fleet's boot over a
// convention would be a worse outcome than the convention drifting.
//
// # How load-time enforcement works
//
// MemQLEngine.Init runs ScanTree over the merged tree and records every
// violation on the LoadReport as a skip, so strict boot REFUSES rather than
// warns, with MEMQL_DSL_ALLOW_SKIPS as the operator break-glass -- the same
// mechanism a construct that fails to parse already goes through. cmd/memqllint
// runs the engine's Init over a mounted bundle, so a bundle author sees these
// offline before the deploy rather than as a CrashLoop after it.
//
// # One detector
//
// The test package delegates to this code rather than keeping its own copy.
// The repeated failure in this area is two detectors that drift, and the drift
// is reliably fail-open: the CI gate split filter clauses on `&&` only while
// the editor rule did not (memql#2779), the classifier could not see the `,`
// operator the engine honours (memql#3612), the audit decided @serverOnly by a
// regex the loader did not use (memql#2875). One implementation cannot
// disagree with itself.
package dslgate

import (
	"fmt"
	"io"
	"io/fs"
	"sort"

	"github.com/znasllc-io/memql/core/dslfs"
)

// Gate names the rule a violation broke. Stable strings: they appear in boot
// logs, in the strict-boot refusal, and in memqllint output.
type Gate string

const (
	// GateRetiredOperator -- a filter clause uses a retired operator form.
	GateRetiredOperator Gate = "retired-operator"
	// GateFilterRowIntrinsic -- a filter names a row intrinsic without `row.`.
	GateFilterRowIntrinsic Gate = "filter-row-intrinsic"
	// GateSortRowIntrinsic -- a sort key names a row intrinsic without `row.`.
	GateSortRowIntrinsic Gate = "sort-row-intrinsic"
	// GateUserScopeSelection -- a construct selects rows by a user-scope column
	// with no caller check, admin gate, @public or @serverOnly.
	GateUserScopeSelection Gate = "authz-user-scope"
	// GateAdminGateComposition -- an admin gate that a false value would not
	// zero the result set with.
	GateAdminGateComposition Gate = "authz-admin-gate"
)

// Violation is one contract-gate failure, attributed as precisely as the gate
// can attribute it.
type Violation struct {
	Gate      Gate
	File      string // path within the scanned tree
	Line      int    // 1-based; 0 when the gate cannot place it
	Kind      string // construct keyword (query / mutate / seed), when known
	Construct string // construct name, when known
	Detail    string // operator-facing sentence, including the remedy
}

// String renders a violation for a log line or a test failure.
func (v Violation) String() string {
	loc := v.File
	if v.Line > 0 {
		loc = fmt.Sprintf("%s:%d", v.File, v.Line)
	}
	if v.Construct != "" {
		return fmt.Sprintf("%s [%s] %s %s: %s", loc, v.Gate, v.Kind, v.Construct, v.Detail)
	}
	return fmt.Sprintf("%s [%s] %s", loc, v.Gate, v.Detail)
}

// Options carries what the gates cannot read off the source text alone.
type Options struct {
	// ServerOnly reports whether the construct named by (file, name) carries a
	// real @serverOnly annotation. @serverOnly answers the same question
	// @public does from the other side (memql#2800): the construct is not
	// callable by a client at all, so a caller-scope filter is neither present
	// nor meaningful.
	//
	// It is an OPTION rather than a source-text scan on purpose. Deciding it
	// by regex is fail-OPEN (memql#2875): a line beginning `@serverOnly`
	// inside a multi-line annotation string, or inside a block comment opened
	// on an `@`-line, satisfies the regex, which EXEMPTS the construct here
	// while Function.ServerOnly stays false and nothing is enforced at
	// runtime. The engine passes its own loaded verdict; a caller with no
	// loader (a pure source scan) passes the same rule applied to a parse.
	//
	// nil means "no construct is @serverOnly", which is the fail-CLOSED
	// direction: a genuinely server-only construct is reported rather than
	// silently exempted. Callers that have the verdict should supply it.
	ServerOnly func(file, name string) bool
}

func (o Options) serverOnly(file, name string) bool {
	if o.ServerOnly == nil {
		return false
	}
	return o.ServerOnly(file, name)
}

// UserScopeSelectionExemptions records constructs that select rows by a
// user-scope column without a caller check, and are known-outstanding rather
// than accepted. Keyed "<file> <constructName>".
//
// An entry here is DEBT, not a blessing: it keeps the gate green for a finding
// already tracked elsewhere while still failing on anything new. Removing the
// entry is part of closing the issue it names.
//
// It lives in this package rather than in the test so the load-time gate and
// the test-time gate cannot hold different tables -- an entry that silenced
// the test while the engine refused to boot would be a trap laid for the next
// author.
var UserScopeSelectionExemptions = map[string]string{
	// Empty on purpose. runningPlansForUser was the last entry; #2800 resolved
	// it by marking the construct @serverOnly rather than by exempting it, so
	// the debt is paid rather than deferred.
}

// ScanTree runs every contract gate over every .memql file in tree and returns
// the violations, ordered by file then line so boot logs and test output are
// deterministic.
//
// tree is the MERGED tree -- dsl.Tree() returns the embedded core domains plus
// every RegisterTree overlay, and MEMQL_DSL_PATH product domains are mounted
// through exactly that call -- so one scan covers every source a node loads
// from.
func ScanTree(tree fs.FS, opts Options) ([]Violation, error) {
	if tree == nil {
		return nil, nil
	}
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		return nil, fmt.Errorf("walking DSL tree: %w", err)
	}
	var out []Violation
	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			return nil, fmt.Errorf("open %s: %w", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", p, readErr)
		}
		out = append(out, ScanSource(p, string(raw), opts)...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Gate < out[j].Gate
	})
	return out, nil
}

// ScanSource runs every contract gate over one file's source.
func ScanSource(path, src string, opts Options) []Violation {
	var out []Violation
	out = append(out, scanRetiredOperators(path, src)...)
	out = append(out, scanRowIntrinsics(path, src)...)
	out = append(out, scanConstructAuthz(path, src, opts)...)
	return out
}
