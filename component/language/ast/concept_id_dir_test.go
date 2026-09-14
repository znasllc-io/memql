package ast

// #2614: directory-aware canonical-id assembly -- absent @namespace derives
// the domain directory; explicit must equal / colon-extend / match the pin;
// any other mismatch is the moved-file guard.

import (
	"strings"
	"testing"
)

func dirDecl(name string, attrs ...*Attribute) *ConceptDecl {
	return &ConceptDecl{Name: name, Attributes: attrs}
}

func nsAttr(v string) *Attribute { return &Attribute{Name: "namespace", Value: v} }

func TestAssembleConceptIdFromDeclInDir(t *testing.T) {
	t.Run("absent-derives-directory", func(t *testing.T) {
		id, err := AssembleConceptIdFromDeclInDir(dirDecl("space"), "cognition", "")
		if err != nil || id != "v1:cognition:space" {
			t.Fatalf("id=%q err=%v", id, err)
		}
	})
	// THE PIN IS THE DERIVATION, not a cross-check. @namespace used to WIN
	// over the directory and the pin only validated it; epic memql#5375
	// retired the annotation, so the pin is the only expression of a
	// divergence left -- and it has to be APPLIED, or dsl/shopify/generated
	// (nested, so its directory is not a legal namespace at all) and
	// dsl/deployment (pinned to "cluster") both lose their ids.
	t.Run("pin-is-the-namespace", func(t *testing.T) {
		id, err := AssembleConceptIdFromDeclInDir(dirDecl("deployment"), "deployment", "cluster")
		if err != nil || id != "v1:cluster:deployment" {
			t.Fatalf("id=%q err=%v", id, err)
		}
	})
	t.Run("colon-scoped-pin", func(t *testing.T) {
		id, err := AssembleConceptIdFromDeclInDir(dirDecl("tool"), "cognition", "cognition:client:tool")
		if err != nil || id != "v1:cognition:client:tool:tool" {
			t.Fatalf("id=%q err=%v", id, err)
		}
	})
	// A PRESENT @namespace is the RETIREMENT refusal, whatever it says.
	//
	// It used to be compared against the directory here and the #2614
	// moved-file guard fired on a mismatch -- which meant an author who moved
	// a file was told to "fix the annotation", advice pointing at something
	// that no longer exists, with a second refusal waiting behind the first.
	// The guard reconciled TWO sources of truth for a namespace and there is
	// one now, so a moved file's ids follow the move by construction and a
	// domain that must keep its ids pins them.
	t.Run("present-annotation-is-retired", func(t *testing.T) {
		_, err := AssembleConceptIdFromDeclInDir(dirDecl("space", nsAttr("identity")), "cognition", "")
		if err == nil || !strings.Contains(err.Error(), "is retired") {
			t.Fatalf("a present @namespace must refuse as retired, got %v", err)
		}
		if !strings.Contains(err.Error(), "namespace.pin") {
			t.Errorf("the error must name the pin escape hatch: %v", err)
		}
	})
}
