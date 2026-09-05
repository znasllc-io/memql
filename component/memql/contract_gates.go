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
// # The core tree's late-bound calls are not violations (memql#4882)
//
// The merged tree is what made the cross-namespace gate honest, and it is also
// what made it fire against the engine itself: dsl/cognition/logic.memql calls
// `mutationCreateCanvasState`, which the engine documents as "supplied by a
// product bundle at runtime". On a node mounting a bundle that declares it,
// pass 1 finds the declaration in the product namespace and the gate asks the
// CORE file to import it -- an import the engine cannot write, because it does
// not know the product namespace exists. Every conforming bundle therefore
// refused strict boot. contractGateOptions now passes the embedded tree's own
// domain list, and dslgate exempts a core file's reference to a name only a
// runtime domain declares -- that direction and no other.
//
// Only CONTRACT gates live here -- authorization scoping, retired operators the
// engine still honours, the `row.` namespace that decides whether a filter
// compiles to a table column or a JSONB path, and the cross-namespace import
// rule. House-style gates (naming, redundant annotations, canonical short forms)
// stay in test/dslconformance: failing a fleet's boot over a convention would be
// worse than the convention drifting. The rules themselves live in
// component/memql/dslgate, which the conformance tests delegate to, so there is
// one detector rather than two that drift.
//
// # The whole corpus goes in one call (memql#4051)
//
// This used to loop dslgate.ScanSource per file, which can only express per-file
// rules -- and one gate is not one. GateCrossNamespaceImport (memql#3803) asks
// whether a reference crosses a namespace boundary, which depends on where the
// referenced name is DECLARED, so no single file answers it. It therefore lived
// on dslgate.ScanTree, whose only caller was a conformance test, which meant the
// rule was enforced over THIS repo's dsl/ at PR time and over a MEMQL_DSL_PATH
// product bundle nowhere at all -- reproducing, for one gate, the exact
// asymmetry this file was written to abolish.
//
// memql#4051 ruled that a gap rather than a deliberate exemption. The cost
// argument for leaving it out ("strict boot may not want to pay for the merged
// tree") did not survive being checked: boot ALREADY holds the merged tree --
// `files` is baseloader.ReadAll's output, read for the duplicate detector on the
// next line of Init -- so the corpus pass adds no walk, no open and no read.
//
// Cost, measured over the 206-file embedded tree on the dev machine, once per
// Init, on top of a load that already takes seconds:
//
//	per-file gates   ~43ms
//	cross-namespace  ~79ms   (memql#4051)
//	total           ~121ms
//
// The corpus gate is the more expensive half because it makes two passes over
// every file -- one to find declarations, one to find references -- where the
// per-file gates make one. Stated rather than rounded away, because a check on
// every node's boot path earns the measurement, and because the next person
// weighing a gate against boot time should see what this one actually cost.

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/memql/baseloader"
	"github.com/znasllc-io/memql/component/memql/dslgate"
	"github.com/znasllc-io/memql/dsl"
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
	// The core-domain verdict (memql#4882). Read once per Init off the embedded
	// tree's own directory list, the same set MountRuntimeDomainsFromEnv refuses
	// a runtime domain for colliding with -- so "core" here means exactly what
	// it means at mount time.
	core := map[string]bool{}
	for _, d := range dsl.CoreDomains() {
		core[d] = true
	}
	return dslgate.Options{
		ServerOnly:         func(file, name string) bool { return serverOnly[file+" "+name] },
		CoreDomain:         func(domain string) bool { return core[domain] },
		BuiltinStepRefusal: builtinStepRefusal(functions),
	}
}

// builtinStepRefusal answers the builtin-step gate with THE PARSER'S OWN
// verdict rather than a second reading of it (memql#4927).
//
// # What it can decide, and what it deliberately cannot
//
// An automation step's argument VALUES are not literals -- `inboundRequestId:
// id` names one of the automation's own args, resolved at fire time -- so this
// cannot hand the source text to the parser and ask. What it can decide, with
// no value knowledge at all, is ARITY: whether the profile accepts a call with
// nothing in it, and whether it accepts one with something in it. Both are the
// real parser's answer to a probe, so the gate and the runtime cannot disagree
// about the rule even if the rule changes.
//
// Empty is the case memql#4927 was filed for. A profile-`object` builtin
// declared with an empty body is a shape somebody writes on purpose --
// "@args(profile=object)" reads like a house style rather than a claim about
// the call -- and the automation beside it is then dead for as long as it is
// deployed.
func builtinStepRefusal(functions *FunctionRegistry) func(string, string) (string, bool) {
	return func(name, args string) (string, bool) {
		if functions == nil {
			return "", false
		}
		fn, err := functions.Get(strings.TrimSpace(name))
		if err != nil || fn == nil || !fn.IsBuiltin() {
			// Not a builtin this build knows. Fail OPEN: a product bundle at
			// MEMQL_DSL_PATH may call one this tree cannot see.
			return "", false
		}
		if strings.TrimSpace(args) == "" {
			if _, perr := parseMetaCommandArgs(name, "", fn.BuiltinArgs); perr != nil {
				return fmt.Sprintf(
					"calling it with no arguments is refused: %v. "+
						"Either declare it @args(profile=\"optionalObject\") -- the profile that admits an empty call, "+
						"and which nothing in the tree uses today -- or give the step the arguments it asks for.",
					perr), true
			}
			return "", true
		}
		// A NON-empty list, probed with a shape rather than the real values.
		// The probe only has to settle whether the profile takes arguments at
		// all; a profile that does will reject THIS probe for its own field
		// rules, which is not what the gate is asking about.
		if _, perr := parseMetaCommandArgs(name, "", fn.BuiltinArgs); perr == nil {
			// It accepts an empty call, so it accepts this one's arity.
			if _, perr := parseMetaCommandArgs(name, args, fn.BuiltinArgs); perr != nil &&
				strings.Contains(perr.Error(), "does not accept arguments") {
				return fmt.Sprintf("it takes no arguments: %v", perr), true
			}
		}
		return "", true
	}
}

// recordContractGateProblems scans every file in the merged tree and records
// each violation on the load report, returning them for the caller to log.
//
// It hands dslgate the WHOLE set in one call rather than looping ScanSource
// per file, and that is the load-bearing part (memql#4051). A per-file loop can
// only run per-file rules, which left the cross-namespace-import gate
// (memql#3803) enforced by a conformance test over this repo's own dsl/ and by
// nothing at all over a MEMQL_DSL_PATH product bundle -- the exact asymmetry
// this file was written to abolish. `files` is already the merged set the
// loaders register from, so passing it whole costs no extra read.
func recordContractGateProblems(report *LoadReport, files []baseloader.RawFile, functions *FunctionRegistry) []dslgate.Violation {
	corpus := make([]dslgate.SourceFile, 0, len(files))
	for _, f := range files {
		corpus = append(corpus, dslgate.SourceFile{Path: f.Path, Content: f.Content})
	}
	out := dslgate.ScanFiles(corpus, contractGateOptions(functions))
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
