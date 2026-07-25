package sense

import "testing"

// stubWorkspace is a WorkspaceGraph that answers definitively, used to prove the
// Service stores and exposes the graph it is built with.
type stubWorkspace struct{}

func (stubWorkspace) ModuleResolves(string, string) Resolved         { return ResolvedYes }
func (stubWorkspace) SymbolDeclared(string, string, string) Resolved { return ResolvedYes }
func (stubWorkspace) ConceptExists(string, string) Resolved          { return ResolvedYes }
func (stubWorkspace) HasNamespace(string) bool                       { return true }
func (stubWorkspace) HasImportsOnlyFiles() bool                      { return false }
func (stubWorkspace) Namespaces() []string                           { return []string{"fylo"} }
func (stubWorkspace) Kinds(string) []string                          { return []string{"concepts"} }
func (stubWorkspace) SymbolsInModule(string, string) []string        { return []string{"order"} }
func (stubWorkspace) DeclarationSites(string) []DeclSite {
	return []DeclSite{{File: "fylo/concepts.memql", Name: "order", Kind: "concept", Line: 3, Column: 9}}
}

func TestNew_StoresNoOpWorkspaceGraph(t *testing.T) {
	s := New(nil)
	if s.Workspace() == nil {
		t.Fatal("New(nil) left a nil workspace graph; consumers must not need a nil check")
	}
	if got := s.Workspace().ModuleResolves("fylo", "concepts"); got != ResolvedUnknown {
		t.Errorf("no-op ModuleResolves = %v, want ResolvedUnknown", got)
	}
	if got := s.Workspace().SymbolDeclared("fylo", "concepts", "order"); got != ResolvedUnknown {
		t.Errorf("no-op SymbolDeclared = %v, want ResolvedUnknown", got)
	}
	if got := s.Workspace().ConceptExists("order", ""); got != ResolvedUnknown {
		t.Errorf("no-op ConceptExists = %v, want ResolvedUnknown", got)
	}
	if s.Workspace().Namespaces() != nil || s.Workspace().Kinds("fylo") != nil ||
		s.Workspace().SymbolsInModule("fylo", "concepts") != nil {
		t.Error("no-op enumerations must be empty")
	}
}

func TestNewWithWorkspace_StoresGraphAndReplacesNil(t *testing.T) {
	s := NewWithWorkspace(nil, stubWorkspace{})
	if _, ok := s.Workspace().(stubWorkspace); !ok {
		t.Fatalf("NewWithWorkspace stored %T, want stubWorkspace", s.Workspace())
	}
	if got := s.Workspace().ModuleResolves("fylo", "concepts"); got != ResolvedYes {
		t.Errorf("stub ModuleResolves = %v, want ResolvedYes", got)
	}
	// A nil graph is replaced by the no-op, never stored as nil.
	s2 := NewWithWorkspace(nil, nil)
	if _, ok := s2.Workspace().(noWorkspace); !ok {
		t.Fatalf("NewWithWorkspace(_, nil) stored %T, want noWorkspace", s2.Workspace())
	}
}
