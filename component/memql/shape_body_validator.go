package memql

// shape_body_validator.go carries the DECL-LOCAL half of the shape
// validation memql#3621 adds -- everything checkable from the shape's own
// declaration, with no registry in hand. The registry-dependent half (does
// a projected payload property exist on the bound concept?) lives in
// shape_concept_validator.go and runs at bootstrap.
//
// Every check here answers the same question the issue asks: a shape body
// that says something untrue produced a projection key whose VALUE was
// silently nil, never an error. Four ways that happened:
//
//   - `include <otherShape>` -- documented composition that was never
//     implemented. The parser reads a body as a path list, so the verb and
//     its argument became two payload paths and two always-null keys.
//   - a terminal-key collision -- every path is keyed by its LAST segment,
//     and the template is a plain map, so `row.id` + `id` wrote one entry
//     and the row id was lost to whichever path came second.
//   - a kind that nothing checked -- @row / @actor were parsed and stored
//     and never compared against the body, so an @row-only shape could
//     project actor.userId (and an empty body hardcoded KindRow even under
//     @actor).
//   - an actor member outside the envelope -- `actor.displayName` loaded
//     fine and rendered nil, because extractNodeFieldValue has no actor
//     case at all. validateActorMemberPaths (#2623 / #2625) closed that set
//     for Query / Mutation / Logic / Automation and stopped short of shapes.

import (
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// shapeIncludeVerb is the composition keyword the documentation promised
// and the parser never had. It is rejected by name so an author who writes
// it gets the decision instead of two silent null keys.
const shapeIncludeVerb = "include"

// validateShapeBody runs every decl-local shape check. kindRow / kindActor
// are the annotation flags the converter already parsed off the decl, passed
// in rather than re-derived so the two cannot disagree.
func validateShapeBody(decl *languageParser.ShapeDecl, kindRow, kindActor bool, origin string) error {
	if decl == nil {
		return nil
	}

	// A shape's kind says where its fields come from. Nothing checked it
	// before, so a kind-less shape loaded and every consumer had to guess.
	if !kindRow && !kindActor {
		return fmt.Errorf("%s: shape %q declares neither @row nor @actor -- every shape must declare where its fields come from (@row = the bound concept's payload + row intrinsics, @actor = the auth envelope; declaring both is allowed for a mixed shape)", origin, decl.Name)
	}

	// An empty body is the default-projection form (memql#2035): it expands
	// to the bound concept's projectable PAYLOAD fields, which is row work.
	// Under @actor alone there is nothing for it to project.
	if len(decl.Paths) == 0 {
		if !kindRow {
			return fmt.Errorf("%s: shape %q has an empty body (the default projection over its bound concept's projectable fields) but does not declare @row -- the default projection is payload-only, so an @actor-only shape must list its actor.* paths explicitly", origin, decl.Name)
		}
		return nil
	}

	sawRowPath, sawActorPath := false, false
	for _, path := range decl.Paths {
		if path == shapeIncludeVerb {
			return fmt.Errorf("%s: shape %q uses `include` -- shape composition is NOT implemented (memql#3621). A shape body is a path list, so `include other` parsed as two payload properties named `include` and `other` and projected two always-null keys. Repeat the paths, or drop the body entirely to get the default projection over the bound concept (memql#2035)", origin, decl.Name)
		}

		// The removed `payload.` prefix has its own migration error in the
		// converter; leave it to say so rather than reporting a kind
		// mismatch about a form that is rejected outright. It still counts
		// as row-sourced, so a body made only of the removed form gets that
		// migration error rather than an unused-@row complaint.
		if strings.HasPrefix(path, "payload.") || strings.HasPrefix(path, "row.payload.") {
			sawRowPath = true
			continue
		}

		if member, ok := strings.CutPrefix(path, "actor."); ok {
			if !kindActor {
				return fmt.Errorf("%s: shape %q projects %q but does not declare @actor -- the auth envelope is a declared source, not an ambient one; add @actor or drop the path", origin, decl.Name, path)
			}
			if err := validateShapeActorMember(decl.Name, path, member, origin); err != nil {
				return err
			}
			sawActorPath = true
			continue
		}

		// Everything else is row-sourced: `row.X` (intrinsic) or a bare
		// payload property.
		if !kindRow {
			what := "payload property"
			if strings.HasPrefix(path, "row.") {
				what = "row intrinsic"
			}
			return fmt.Errorf("%s: shape %q projects %q (a %s) but does not declare @row -- add @row or drop the path", origin, decl.Name, path, what)
		}
		sawRowPath = true
	}

	// The other direction: a DECLARED kind must be used. This is the sentence
	// declared_usage_validator.go already credited to a "shape kind validator"
	// that did not exist -- "the body contains at least one matching path" --
	// and it is the same discipline the unused-@useConcept rule applies.
	if kindRow && !sawRowPath {
		return fmt.Errorf("%s: shape %q declares @row but projects no row intrinsic or payload property -- drop @row or add a path", origin, decl.Name)
	}
	if kindActor && !sawActorPath {
		return fmt.Errorf("%s: shape %q declares @actor but projects no actor.* path -- drop @actor or add a path", origin, decl.Name)
	}

	return nil
}

// validateShapeActorMember closes the actor.* set for shape bodies against
// the one canonical envelope (auth.ActorEnvelopeFields, #2623) -- the same
// table validateActorMemberPaths enforces on the four runtime surfaces.
//
// A shape was the one projection surface left open, and it is the one where
// being open is least visible: the shape renderer has no actor case, so an
// unknown member does not even fail at run time. It renders nil.
func validateShapeActorMember(shapeName, path, member, origin string) error {
	// `actor.config.<key>` reads as a member named `config`. It was dropped
	// from the envelope by #2623 (no resolver ever implemented it), so name
	// the replacement rather than only listing the valid set.
	head := member
	if idx := strings.Index(member, "."); idx > 0 {
		head = member[:idx]
	}
	if head == "config" {
		return fmt.Errorf("%s: shape %q projects %q -- `actor.config.<key>` is retired (#2623: no resolver ever implemented it). Config is read through the bare reserved `config.<key>` identifier, which shapes do not project", origin, shapeName, path)
	}
	if _, ok := auth.ActorEnvelopeCanonicalName(head); !ok {
		return fmt.Errorf("%s: shape %q projects unknown actor member `actor.%s`; the auth envelope is a closed set (#2623) -- valid: %s", origin, shapeName, head, auth.ActorEnvelopeValidNames())
	}
	return nil
}
