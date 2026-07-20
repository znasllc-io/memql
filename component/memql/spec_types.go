package memql

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/memql/baseregistry"
)

var (
	specNamePattern = regexp.MustCompile(`^[a-z]+[A-Za-z0-9]*$`)
)

// SpecKind discriminates how a spec is evaluated.
//
//   - Row: body references payload.* / intrinsics only. Compiles to
//     a SQL WHERE clause and runs per-row in the database. The
//     foundational atomic-predicate flavour; used in query filters,
//     embedded in `concept==X;<spec>` expressions.
//
//   - Context: body references caller.* / ctx.* only (no row
//     fields). Evaluated in-process at call time against the
//     auth-context envelope. Callable from policy bodies via the
//     `spec("name")` builtin. Replaces "pure boolean policies" that
//     only check caller role / partition / scope — those collapse
//     into context-specs.
//
// Mixed-mode bodies (both row + caller references) are rejected at
// registration. If a real use case needs both, write a policy that
// composes a row-spec + context-spec.
type SpecKind string

const (
	SpecKindRow     SpecKind = "row"
	SpecKindContext SpecKind = "context"
)

// SpecRegistry stores globally registered specifications, plus the names
// the unified loader skipped as @disabled. A disabled name stays OCCUPIED:
// specs are authorization predicates, and without the reservation an
// authored spec could be promoted over a deliberately-retired core spec
// name (the ToolRegistry resurrection precedent, #2606/#2607).
type SpecRegistry struct {
	*baseregistry.Registry[Spec]

	disabledMu sync.RWMutex
	disabled   map[string]bool
}

func newSpecRegistry() *SpecRegistry {
	return &SpecRegistry{
		Registry: baseregistry.New[Spec]("spec",
			func(s *Spec) *Spec { return s.clone() },
			validateSpecName),
		disabled: make(map[string]bool),
	}
}

// MarkDisabled records a spec/trait name skipped as @disabled at load,
// keeping the name occupied for promotion guards and diagnostics.
func (r *SpecRegistry) MarkDisabled(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	r.disabledMu.Lock()
	if r.disabled == nil {
		r.disabled = make(map[string]bool)
	}
	r.disabled[name] = true
	r.disabledMu.Unlock()
}

// UnmarkDisabled releases a @disabled name reservation. Only the authored
// lifecycle calls it (memql#2643): a reservation placed by a stored @disabled
// AUTHORED row is lifted by promoting the corrected (enabled) source or by
// demoting the name. Core-loader reservations are never released.
func (r *SpecRegistry) UnmarkDisabled(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	r.disabledMu.Lock()
	delete(r.disabled, name)
	r.disabledMu.Unlock()
}

// IsDisabled reports whether the name was skipped as @disabled at load.
func (r *SpecRegistry) IsDisabled(name string) bool {
	r.disabledMu.RLock()
	defer r.disabledMu.RUnlock()
	return r.disabled[strings.TrimSpace(name)]
}

// add inserts a spec into the registry. Errors when the name is
// already taken.
func (r *SpecRegistry) add(spec *Spec) error {
	if spec == nil {
		return fmt.Errorf("spec is nil")
	}
	return r.Registry.Add(spec.Name, spec)
}

func validateSpecName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("spec name is required")
	}
	if !specNamePattern.MatchString(trimmed) {
		return fmt.Errorf("spec name %q must be camelCase (letters/digits, starting lowercase)", name)
	}
	return nil
}

func cloneSpecMap(src map[string]*Spec) map[string]*Spec {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]*Spec, len(src))
	for name, spec := range src {
		if spec == nil {
			continue
		}
		dst[name] = spec.clone()
	}
	return dst
}
