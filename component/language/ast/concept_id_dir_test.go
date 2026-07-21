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
	t.Run("explicit-equal-wins", func(t *testing.T) {
		id, err := AssembleConceptIdFromDeclInDir(dirDecl("space", nsAttr("cognition")), "cognition", "")
		if err != nil || id != "v1:cognition:space" {
			t.Fatalf("id=%q err=%v", id, err)
		}
	})
	t.Run("colon-extension-wins", func(t *testing.T) {
		id, err := AssembleConceptIdFromDeclInDir(dirDecl("tool", nsAttr("cognition:client:tool")), "cognition", "")
		if err != nil || id != "v1:cognition:client:tool:tool" {
			t.Fatalf("id=%q err=%v", id, err)
		}
	})
	t.Run("pinned-divergence-wins", func(t *testing.T) {
		id, err := AssembleConceptIdFromDeclInDir(dirDecl("deployment", nsAttr("cluster")), "deployment", "cluster")
		if err != nil || id != "v1:cluster:deployment" {
			t.Fatalf("id=%q err=%v", id, err)
		}
	})
	t.Run("mismatch-is-the-moved-file-guard", func(t *testing.T) {
		_, err := AssembleConceptIdFromDeclInDir(dirDecl("space", nsAttr("identity")), "cognition", "")
		if err == nil || !strings.Contains(err.Error(), "does not match its domain directory") {
			t.Fatalf("mismatch must error with the guard message, got %v", err)
		}
		if !strings.Contains(err.Error(), "namespace.pin") {
			t.Errorf("the error must name the pin escape hatch: %v", err)
		}
	})
}
