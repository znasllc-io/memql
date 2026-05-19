package memql

import (
	"fmt"
	"strings"
)

// cqs_registry_validator.go runs the second pass of CQS enforcement
// (Phase 2 hoist). The per-file pass in tryParseNewFunctionSyntax
// catches violations where caller and callee live in the same file
// (rare); this pass catches the common case where a query in file A
// would call a mutation in file B.
//
// Runs after every loader (legacy + unified) has populated the
// FunctionRegistry. Reads each registered function's source for
// bare-name function calls, looks each callee up in the registry,
// and applies the violatesCQS pair test.
//
// CQS rules (mirror compiler.violatesCQS, kept in sync deliberately):
//   - Query    -> Mutation : forbidden (read path stays side-effect-free)
//   - Mutation -> Mutation : forbidden (one observable write per body)
//   - Spec     -> Mutation : forbidden (predicates are pure reads)
//
// Specs are out of scope at the registry level today -- they live in
// SpecRegistry, not FunctionRegistry, and their bodies don't reach
// other functions (specs are atomic boolean expressions, no function
// calls). The validator nevertheless runs over every registered
// function in FunctionRegistry, so any Spec routed there (the
// historic `func (Spec)` form, retired but kept for the transitional
// window) is also covered.

// ValidateCQSAcrossRegistry walks every registered function and
// enforces the cross-file CQS pair rule. Returns the first
// violation found.
//
// Caller pattern (engine startup, post-load):
//
//	if err := memql.ValidateCQSAcrossRegistry(engine.Functions()); err != nil {
//	    logger.Error("CQS violation, refusing to serve traffic", "err", err)
//	    os.Exit(1)
//	}
//
// The engine startup gate fails loud rather than logging+continuing
// because a CQS violation either silently corrupts data (mutation
// called from a query observed only via row drift) or breaks the
// runtime in production. Either way, fail at startup.
func ValidateCQSAcrossRegistry(registry *FunctionRegistry) error {
	if registry == nil {
		return nil
	}

	all := registry.List()
	if len(all) == 0 {
		return nil
	}

	// Build a name -> kind index.
	kindByName := make(map[string]string, len(all))
	sourceByName := make(map[string]string, len(all))
	for _, fn := range all {
		if fn == nil || fn.Name == "" {
			continue
		}
		kindByName[fn.Name] = strings.ToLower(strings.TrimSpace(fn.FunctionKind))
		sourceByName[fn.Name] = fn.ExprSource
	}

	for _, fn := range all {
		if fn == nil || fn.Name == "" {
			continue
		}
		callerKind := kindByName[fn.Name]
		body := sourceByName[fn.Name]
		if body == "" {
			continue
		}
		for callee := range extractBareCallNames(body) {
			// Skip self-references. The struct-form rewriter emits the
			// stored ExprSource in procedural form (`func (Mutation) NAME(ctx any)
			// error { ... }`); extractBareCallNames sees the function
			// header as a `NAME(` call site and flags every mutation as
			// calling itself. MemQL doesn't permit recursion anyway, so
			// a legitimate self-call would already be rejected upstream.
			if callee == fn.Name {
				continue
			}
			calleeKind, ok := kindByName[callee]
			if !ok {
				continue
			}
			if violatesCrossRegistryCQS(callerKind, calleeKind) {
				return fmt.Errorf(
					"CQS violation: %s %q calls %s %q -- the read path must stay side-effect-free, and a mutation must perform exactly one write. Refactor by moving the offending write into an automation triggered on the row landing (see docs/core/memql-authoring-rules.md Rule #1)",
					callerKind, fn.Name, calleeKind, callee,
				)
			}
		}
	}
	return nil
}

// violatesCrossRegistryCQS mirrors compiler.violatesCQS but uses
// string kinds since the registry already normalizes to FunctionType
// strings. Kept as a separate function so the engine-side validator
// can evolve independently of the compiler if needed.
func violatesCrossRegistryCQS(caller, callee string) bool {
	switch caller {
	case "query":
		return callee == "mutation"
	case "mutation":
		return callee == "mutation"
	case "spec":
		return callee == "mutation"
	default:
		return false
	}
}

// extractBareCallNames scans a function body for `<name>(` bare-name
// call sites and returns the set of distinct names. Skips
// identifiers preceded by `.` (so `args.foo(` does not match foo as
// a top-level callee). Conservative -- false positives only collide
// with non-function names that don't exist in the registry, so they
// silently no-op.
func extractBareCallNames(body string) map[string]struct{} {
	out := make(map[string]struct{})
	i := 0
	for i < len(body) {
		c := body[i]
		// Skip past string literals so identifiers inside strings
		// don't trigger.
		if c == '"' {
			i++
			for i < len(body) && body[i] != '"' {
				if body[i] == '\\' && i+1 < len(body) {
					i += 2
					continue
				}
				i++
			}
			i++
			continue
		}
		// Skip line comments.
		if c == '/' && i+1 < len(body) && body[i+1] == '/' {
			for i < len(body) && body[i] != '\n' {
				i++
			}
			continue
		}
		// Skip block comments.
		if c == '/' && i+1 < len(body) && body[i+1] == '*' {
			i += 2
			for i+1 < len(body) && !(body[i] == '*' && body[i+1] == '/') {
				i++
			}
			i += 2
			continue
		}
		// Identifier start: letter or underscore.
		if isCqsIdentStart(c) {
			start := i
			for i < len(body) && isCqsIdentByte(body[i]) {
				i++
			}
			// A call site is `<ident>(` with no `.` immediately before.
			if i < len(body) && body[i] == '(' {
				if start == 0 || body[start-1] != '.' {
					out[body[start:i]] = struct{}{}
				}
			}
			continue
		}
		i++
	}
	return out
}

func isCqsIdentStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_'
}

func isCqsIdentByte(b byte) bool {
	return isCqsIdentStart(b) || (b >= '0' && b <= '9')
}
