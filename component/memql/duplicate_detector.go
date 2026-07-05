package memql

// duplicate_detector.go is the load-time per-kind uniqueness gate
// (epic #2351 / S5, memql#2360). It kills the silent last-wins
// registration class: queries + mutations + logic + builtins share ONE
// flat FunctionRegistry keyed by bare name, and every unified loader
// registers via Upsert (baseregistry.Registry.Upsert never checks for an
// existing entry). Loading walks core + every RegisterTree'd pack
// alphabetically, so two constructs of the same kind with the same name
// -- in different files, or a core file vs a pack file -- silently
// overwrite one another by load order. The same shape recurs for
// shapes / specs / tools / prompts / providers / policies / seeds /
// automations.
//
// The detector walks the SAME merged DSL tree the loaders register from
// (embedded core + pack overlays, each file surfaced exactly once by
// dsl.Tree()'s shadowing layeredFS) and reports every (group, name) pair
// declared more than once. That construction is what makes it
// LOAD-CYCLE-AWARE rather than a naive Upsert interceptor:
//
//   - Legacy + unified double-load is a non-issue: the legacy loaders
//     (loadEmbeddedFunctions / loadEmbeddedShapes / loadToolRegistry /
//     ...) are inert stubs returning empty registries; only the unified
//     pass populates anything. The detector reads FILES, not
//     registrations, so it counts each declaration once regardless.
//   - The authoring-session overlay (authoring_session.go) writes to a
//     SEPARATE overlay registry, not the boot registries, and is not
//     part of the tree walk -- so an operator re-authoring a single
//     construct never trips the gate.
//   - Engine re-Init in tests re-runs this as a PURE function over the
//     tree; there is no package-global accumulated state to leak a false
//     positive across Init calls.
//   - The MEMQL_DSL_PATH per-type disk overlay (component/memql/dslfs)
//     feeds the retired per-type loaders, not dsl.Tree(); the unified
//     loaders and this detector both read dsl.Tree(), so they always
//     agree on the loaded set.
//
// Constructs are grouped by the runtime registry they land in, NOT by
// raw keyword: a `query x` and a `builtin x` genuinely collide because
// both Upsert into the FunctionRegistry, so both live in the "functions"
// group. An `automation x` and a `logic x` do NOT collide (different
// registries) and are correctly kept in different groups -- this is a
// real, intentional pattern in the product pack (an automation that
// invokes a same-named logic).
//
// This is detection + ERROR visibility only. Strict fail-boot rides in
// with S2's LoadReport (#2357); the conformance tests
// (duplicate_detector_test.go here + the carrier repo's pack-load gate) are
// what keep the tree at zero duplicates in CI today.

import (
	"fmt"
	"sort"
	"strings"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/baseloader"
)

// DuplicateConstruct is one (group, name) collision: the same construct
// name declared by two or more authored constructs that land in the same
// runtime registry. Origins names every declaring file (with its
// keyword), sorted, so the ERROR log and the failing test point at both
// sides of the collision.
type DuplicateConstruct struct {
	Group   string   // runtime-registry group ("functions", "shapes", ...)
	Name    string   // the colliding construct name
	Origins []string // "<keyword> <path>" per declaration, sorted + deduped
}

// String renders a one-line summary for logs + test failures.
func (d DuplicateConstruct) String() string {
	return fmt.Sprintf("[%s] %q declared in %d places: %s",
		d.Group, d.Name, len(d.Origins), strings.Join(d.Origins, " | "))
}

// structFormKeywordGroups maps each struct-form construct keyword to the
// runtime registry it lands in. These are the kinds extracted via
// ExtractKeywordSlices (the same call the unified loaders make), so the
// detector sees exactly the declarations that get registered.
//
// The function-registry kinds (query / mutation / logic) are handled
// separately via ExtractFunctionSlices because that extractor applies a
// brace-depth-0 guard: it must NOT mistake a nested `logic name { ... }`
// step invocation inside an automation body for a top-level logic
// declaration. Builtins share the FunctionRegistry too, but they are
// top-level-only, so ExtractKeywordSlices("builtin") is safe for them.
var structFormKeywordGroups = map[string]string{
	"builtin":    "functions", // shares FunctionRegistry with query/mutation/logic
	"shape":      "shapes",
	"spec":       "specs", // spec + trait share the SpecRegistry
	"trait":      "specs",
	"tool":       "tools",
	"prompt":     "prompts",
	"provider":   "providers",
	"policy":     "policies",
	"seed":       "seeds",
	"automation": "automations",
	"action":     "actions",
}

// DetectDuplicateConstructsInTree is the zero-argument convenience for
// the live merged tree: it reads dsl.Tree() (embedded core + every
// RegisterTree'd pack) and runs the detector. Callers in consumer repos
// (e.g. the carrier repo's pack-load gate) use this so they don't have to
// import the baseloader package to get the file list.
func DetectDuplicateConstructsInTree() []DuplicateConstruct {
	return DetectDuplicateConstructs(baseloader.ReadAll(nil))
}

// DetectDuplicateConstructs walks the supplied DSL files and returns
// every (group, name) pair declared more than once, sorted by group then
// name. An empty slice means the tree is clean.
//
// Pass baseloader.ReadAll(logger) for the live merged tree (embedded core
// + every RegisterTree'd pack). Tests pass a fixture slice to exercise
// the detector in isolation.
func DetectDuplicateConstructs(files []baseloader.RawFile) []DuplicateConstruct {
	// key = group + "\x00" + name -> set of "<keyword> <path>" origins.
	origins := map[string]map[string]struct{}{}

	record := func(group, name, keyword, path string) {
		if name == "" {
			return
		}
		key := group + "\x00" + name
		set := origins[key]
		if set == nil {
			set = map[string]struct{}{}
			origins[key] = set
		}
		set[keyword+" "+path] = struct{}{}
	}

	for _, f := range files {
		// Function-registry kinds via the brace-depth-aware extractor so
		// nested logic step-invocations inside automations are excluded.
		for _, s := range ExtractFunctionSlices(f.Content) {
			switch s.Kind {
			case languageParser.FunctionTypeQuery,
				languageParser.FunctionTypeMutation,
				languageParser.FunctionTypeLogic:
				record("functions", s.Name, string(s.Kind), f.Path)
			}
		}
		// Struct-form kinds via the same generic extractor the loaders use.
		for keyword, group := range structFormKeywordGroups {
			for _, s := range ExtractKeywordSlices(f.Content, keyword) {
				record(group, s.Name, keyword, f.Path)
			}
		}
	}

	var out []DuplicateConstruct
	for key, set := range origins {
		if len(set) < 2 {
			continue
		}
		parts := strings.SplitN(key, "\x00", 2)
		list := make([]string, 0, len(set))
		for o := range set {
			list = append(list, o)
		}
		sort.Strings(list)
		out = append(out, DuplicateConstruct{
			Group:   parts[0],
			Name:    parts[1],
			Origins: list,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Name < out[j].Name
	})
	return out
}
