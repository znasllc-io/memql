package memql

import (
	"fmt"
	"regexp"
	"strings"

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

// Spec represents a named boolean predicate.
//
// Row-specs compile to SQL WHERE filters (Kind == SpecKindRow).
// Context-specs evaluate in-process against the caller's auth
// context at call time (Kind == SpecKindContext).
type Spec struct {
	Name        string
	Description string
	ExprSource  string
	Expr        ExpressionNode
	Kind        SpecKind
	UsesSI      bool
	Origin      string

	// UseConceptName carries the bare name from @useConcept(N) when
	// the spec uses that binding form. Empty when the spec binds via
	// @useShape or when the entry is a trait.
	UseConceptName string

	// UseShapeName carries the bare name from @useShape(N) when the
	// spec uses that binding form. Empty when the spec binds via
	// @useConcept or when the entry is a trait. Validated at post-load
	// time against the shape registry.
	UseShapeName string

	// IsTrait flags this entry as a trait rather than a spec.
	// Traits share the runtime contract (atomic boolean predicate)
	// but are concept-agnostic: at load time, trait sources are
	// validated to FORBID @useConcept / @useShape, allowing only
	// @row / @caller kind annotations.
	IsTrait bool
}

func (s *Spec) clone() *Spec {
	if s == nil {
		return nil
	}
	return &Spec{
		Name:           s.Name,
		Description:    s.Description,
		ExprSource:     s.ExprSource,
		Expr:           cloneExpressionNode(s.Expr),
		Kind:           s.Kind,
		UsesSI:         s.UsesSI,
		Origin:         s.Origin,
		UseConceptName: s.UseConceptName,
		UseShapeName:   s.UseShapeName,
		IsTrait:        s.IsTrait,
	}
}

// SpecRegistry stores globally registered specifications.
type SpecRegistry struct {
	*baseregistry.Registry[Spec]
}

func newSpecRegistry() *SpecRegistry {
	return &SpecRegistry{
		Registry: baseregistry.New[Spec]("spec",
			func(s *Spec) *Spec { return s.clone() },
			validateSpecName),
	}
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
