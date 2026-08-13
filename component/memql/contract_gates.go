package memql

// contract_gates.go runs the DSL CONTRACT gates at load time (memql#3629).
//
// Every one of these checks used to be a Go test walking this repo's dsl.Tree().
// A product's DSL arrives at RUNTIME from a different repo via MEMQL_DSL_PATH ->
// dsl.MountRuntimeDomainsFromEnv -> RegisterTree, and ran none of them. Under
// platform consolidation (memql#2472) that is the primary delivery path, so the
// tree that most needed the gates was the one tree with none of them -- and two
// of the gates are the pair that refuses memql#3612, where a `,` inside
// parentheses turns an ownership conjunct into a disjunct.
//
// The check reads the SAME merged source the loaders register from
// (baseloader.ReadAll -> dsl.Tree(), embedded core + every RegisterTree
// overlay), and records each violation on the LoadReport as a skip, so strict
// boot refuses it exactly as it refuses a construct that failed to parse, with
// MEMQL_DSL_ALLOW_SKIPS as the operator break-glass. cmd/memqllint drives
// Init over a mounted bundle, so a bundle author gets the same verdict offline
// before the deploy instead of a CrashLoop after it.
//
// Only CONTRACT gates live here -- authorization scoping, retired operators the
// engine still honours, the `row.` namespace that decides whether a filter
// compiles to a table column or a JSONB path. House-style gates (naming,
// redundant annotations, canonical short forms) stay in test/dslconformance:
// failing a fleet's boot over a convention would be worse than the convention
// drifting. The rules themselves live in component/memql/dslgate, which the
// conformance tests delegate to, so there is one detector rather than two that
// drift.
//
// Cost: ~85ms over the 214-file embedded tree, once per Init, on top of a load
// that already takes seconds. Measured rather than assumed, because a check
// that runs on every node's boot path earns the measurement.

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/component/memql/dslgate"
)

// contractGateOptions builds the scan options from the engine's OWN loaded
// verdicts.
//
// @serverOnly is the only thing the gates cannot read off source text, and
// deciding it by regex is fail-OPEN (memql#2875): an `@serverOnly` inside a
// multi-line annotation string or a block comment satisfies the pattern, which
// EXEMPTS the construct from the authz gate while Function.ServerOnly stays
// false and nothing is enforced at runtime. The registry entry IS the loader's
// verdict -- the same bool auth.CallOrigin is checked against at dispatch -- so
// taking it from there means the gate's exemption and the runtime's enforcement
// cannot disagree.
//
// Origin is stamped "unified:<path>" by the unified loader; the gate keys on
// the tree path, so the prefix comes off.
func contractGateOptions(functions *FunctionRegistry) dslgate.Options {
	serverOnly := map[string]bool{}
	if functions != nil {
		for _, fn := range functions.List() {
			if fn == nil || !fn.ServerOnly {
				continue
			}
			serverOnly[strings.TrimPrefix(fn.Origin, "unified:")+" "+fn.Name] = true
		}
	}
	return dslgate.Options{
		ServerOnly: func(file, name string) bool { return serverOnly[file+" "+name] },
	}
}

// recordContractGateProblems scans every file in the merged tree and records
// each violation on the load report, returning them for the caller to log.
func recordContractGateProblems(report *LoadReport, files []baseloader.RawFile, functions *FunctionRegistry) []dslgate.Violation {
	opts := contractGateOptions(functions)
	var out []dslgate.Violation
	for _, f := range files {
		out = append(out, dslgate.ScanSource(f.Path, f.Content, opts)...)
	}
	for _, v := range out {
		name := v.Construct
		if name == "" {
			name = string(v.Gate)
		}
		keyword := v.Kind
		if keyword == "" {
			keyword = "filter"
		}
		detail := v.Detail
		if v.Line > 0 {
			// baseloader.Skip carries no line field, and a file-level
			// attribution is not actionable for a gate that fires on one
			// predicate inside a 1200-line queries.memql.
			detail = fmt.Sprintf("line %d: %s", v.Line, v.Detail)
		}
		report.AddSkip(baseloader.Skip{
			Component: "memql.dslgate",
			Keyword:   keyword,
			Name:      name,
			File:      v.File,
			Phase:     "contract-gate:" + string(v.Gate),
			Err:       detail,
		})
	}
	return out
}
