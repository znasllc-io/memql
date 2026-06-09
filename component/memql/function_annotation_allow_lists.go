package memql

import "github.com/znasllc-io/memql/component/language/annotations"

// function_annotation_allow_lists.go exposes the four per-construct
// annotation allow-lists the unified function loader's pre-parse
// validator consults. They are a load-time gate that surfaces typo'd
// or stale annotations as hard parse errors instead of letting them
// slip through to FunctionDef's generic attribute map.
//
// The lists are no longer hand-maintained here: they derive from the
// single physical registry in component/language/annotations (#991),
// the same source the editor/sense surface derives from, so the two
// can never drift. `constructAnnotationAllowLists` in
// unified_functions_loader.go is the sole consumer.
//
// The retired `@use*` family (`useConcept` / `useShape` / `useTrait` /
// `useSpec` / `useQuery` / `useMutation` / `useLogic` / `useBuiltin` /
// `usePrompt` / `useTool` / `useAutomation`) is deliberately absent:
// `ValidateConstructAnnotations` (baseparser/iface.go) hard-rejects
// anything matching the `@use*` prefix with a migration hint BEFORE the
// allow-list is consulted, so listing them was a dead no-op. The audit
// in #964 confirmed zero remaining occurrences across the tree; the
// no-op entries were pruned in #966.
//
// `@public` (on Query / Mutation) is a parse-only per-row-authz marker
// from issue #54 — no runtime semantics, just the author's explicit
// acknowledgement that the construct is intentionally callable without
// a caller-scope filter (concept catalogs, pre-auth login paths). See
// docs/public/operate/auth/per-row-authz-audit.md.

var (
	allowedQueryAnnotations      = annotations.Set("Query")
	allowedMutationAnnotations   = annotations.Set("Mutation")
	allowedLogicAnnotations      = annotations.Set("Logic")
	allowedAutomationAnnotations = annotations.Set("Automation")
)
