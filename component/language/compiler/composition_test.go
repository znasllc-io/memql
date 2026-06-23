package compiler

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

func TestValidateFileComposition_MultipleQueries(t *testing.T) {
	// Valid: Multiple queries
	source := `
@enabled
func (Query) getUsers() {
  concept==v1:user
}

@enabled
func (Query) getUserById() {
  concept==v1:user;id==args.id
}

@enabled
func (Query) getUsersByRole() {
  concept==v1:user;payload.role==args.role
}
`
	ast := mustParse(t, source)
	file, ok := ast.(*parser.File)
	if !ok {
		t.Fatalf("expected File, got %T", ast)
	}

	err := ValidateFileComposition(file)
	if err != nil {
		t.Errorf("multiple queries should be valid: %v", err)
	}

	comp := AnalyzeComposition(file)
	if comp.Queries != 3 {
		t.Errorf("expected 3 queries, got %d", comp.Queries)
	}
}

func TestValidateFileComposition_AutomationWithQueries(t *testing.T) {
	// Valid: 1 automation + helper queries
	source := `
@enabled
@trigger(event="session.opened")
func (Automation) bootstrapUser(_ any) {
  check := query { concept==v1:user }
  return check
}

func (Query) helperQuery() {
  concept==v1:config
}
`
	ast := mustParse(t, source)
	file, ok := ast.(*parser.File)
	if !ok {
		t.Fatalf("expected File, got %T", ast)
	}

	err := ValidateFileComposition(file)
	if err != nil {
		t.Errorf("automation with helper queries should be valid: %v", err)
	}

	comp := AnalyzeComposition(file)
	if comp.Automations != 1 {
		t.Errorf("expected 1 automation, got %d", comp.Automations)
	}
	if comp.Queries != 1 {
		t.Errorf("expected 1 query, got %d", comp.Queries)
	}
}

func TestValidateFileComposition_MutationWithQueries(t *testing.T) {
	// Valid: 1 mutation + validation queries
	source := `
@enabled
@audit
func (Mutation) createUser() {
  insert("v1:user", payload={"name": args.name})
}

func (Query) validateEmail() {
  concept==v1:user;payload.email==args.email
}

func (Query) checkDuplicate() {
  concept==v1:user;payload.name==args.name
}
`
	ast := mustParse(t, source)
	file, ok := ast.(*parser.File)
	if !ok {
		t.Fatalf("expected File, got %T", ast)
	}

	err := ValidateFileComposition(file)
	if err != nil {
		t.Errorf("mutation with helper queries should be valid: %v", err)
	}

	comp := AnalyzeComposition(file)
	if comp.Mutations != 1 {
		t.Errorf("expected 1 mutation, got %d", comp.Mutations)
	}
	if comp.Queries != 2 {
		t.Errorf("expected 2 queries, got %d", comp.Queries)
	}
}

func TestValidateFileComposition_MultipleAutomations(t *testing.T) {
	// Invalid: 2 automations
	source := `
@enabled
func (Automation) first(_ any) {
  step1 := query { concept==v1:a }
  return step1
}

@enabled
func (Automation) second(_ any) {
  step1 := query { concept==v1:b }
  return step1
}
`
	ast := mustParse(t, source)
	file, ok := ast.(*parser.File)
	if !ok {
		t.Fatalf("expected File, got %T", ast)
	}

	err := ValidateFileComposition(file)
	if err == nil {
		t.Error("expected error for multiple automations")
	}
	if !strings.Contains(err.Error(), "only one automation") {
		t.Errorf("expected 'only one automation' error, got: %v", err)
	}
}

func TestValidateFileComposition_MultipleMutations(t *testing.T) {
	// Invalid: 2 mutations
	source := `
@enabled
func (Mutation) createA() {
  insert("v1:a", payload={})
}

@enabled
func (Mutation) createB() {
  insert("v1:b", payload={})
}
`
	ast := mustParse(t, source)
	file, ok := ast.(*parser.File)
	if !ok {
		t.Fatalf("expected File, got %T", ast)
	}

	err := ValidateFileComposition(file)
	if err == nil {
		t.Error("expected error for multiple mutations")
	}
	if !strings.Contains(err.Error(), "only one mutation") {
		t.Errorf("expected 'only one mutation' error, got: %v", err)
	}
}

func TestValidateFileComposition_MixedAutomationMutation(t *testing.T) {
	// Invalid: automation + mutation in same file
	source := `
@enabled
func (Automation) workflow(_ any) {
  step1 := query { concept==v1:a }
  return step1
}

@enabled
func (Mutation) createRecord() {
  insert("v1:b", payload={})
}
`
	ast := mustParse(t, source)
	file, ok := ast.(*parser.File)
	if !ok {
		t.Fatalf("expected File, got %T", ast)
	}

	err := ValidateFileComposition(file)
	if err == nil {
		t.Error("expected error for mixed automation and mutation")
	}
	if !strings.Contains(err.Error(), "cannot mix automation and mutation") {
		t.Errorf("expected 'cannot mix' error, got: %v", err)
	}
}

func TestGetPrimaryType(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		expected string
	}{
		{
			name: "automation primary",
			source: `
@enabled
func (Automation) test(_ any) {
	step1 := query { concept==v1:a }
	return step1
}
func (Query) helper() { concept==v1:b }
`,
			expected: "automation",
		},
		{
			name: "mutation primary",
			source: `
@enabled
func (Mutation) test() { insert("v1:a", payload={}) }
func (Query) helper() { concept==v1:b }
`,
			expected: "mutation",
		},
		{
			name: "query only",
			source: `
@enabled
func (Query) test() { concept==v1:a }
`,
			expected: "query",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := mustParse(t, tt.source)
			file, ok := ast.(*parser.File)
			if !ok {
				t.Fatalf("expected File, got %T", ast)
			}

			primary := GetPrimaryType(file)
			if primary != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, primary)
			}
		})
	}
}

func TestCompositionSummary(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		contains string
	}{
		{
			name:     "queries only",
			source:   `func (Query) a() { concept==v1:a } func (Query) b() { concept==v1:b }`,
			contains: "2 queries",
		},
		{
			name: "automation with query",
			source: `func (Automation) a(_ any) {
				s := query { concept==v1:a }
				return s
			}
			func (Query) b() { concept==v1:b }`,
			contains: "1 automation",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ast := mustParse(t, tt.source)
			file, ok := ast.(*parser.File)
			if !ok {
				t.Fatalf("expected File, got %T", ast)
			}

			summary := CompositionSummary(file)
			if !strings.Contains(summary, tt.contains) {
				t.Errorf("expected summary to contain %q, got %q", tt.contains, summary)
			}
		})
	}
}

func TestValidateFileComposition_ArgsWithoutBlock(t *testing.T) {
	// Invalid: Function with args parameter but no `args { ... }` block.
	source := `
@enabled
@description("Search users")
func (Query) searchUsers(args any) {
  concept==v1:user; ?.payload.role==args.role
}
`
	ast := mustParse(t, source)
	file, ok := ast.(*parser.File)
	if !ok {
		t.Fatalf("expected File, got %T", ast)
	}

	err := ValidateFileComposition(file)
	if err == nil {
		t.Error("expected error for function with args param but no args block")
	}
	if !strings.Contains(err.Error(), "no file-top `args { ... }` block") {
		t.Errorf("expected error about missing args block, got: %v", err)
	}
}

func TestValidateFileComposition_ArgsWithBlock(t *testing.T) {
	// Valid: Function with args parameter AND file-top args block.
	source := `
@enabled
@description("Search users")
args {
  role    string
  status  string
}
func (Query) searchUsers(args any) {
  concept==v1:user; ?.payload.role==args.role
}
`
	ast := mustParse(t, source)
	file, ok := ast.(*parser.File)
	if !ok {
		t.Fatalf("expected File, got %T", ast)
	}

	err := ValidateFileComposition(file)
	if err != nil {
		t.Errorf("function with args block should be valid: %v", err)
	}
}

func TestValidateFileComposition_NoArgsNoBlock(t *testing.T) {
	// Valid: Function without args parameter (no args block needed).
	source := `
@enabled
@description("Get all users")
func (Query) getAllUsers() {
  concept==v1:user
}
`
	ast := mustParse(t, source)
	file, ok := ast.(*parser.File)
	if !ok {
		t.Fatalf("expected File, got %T", ast)
	}

	err := ValidateFileComposition(file)
	if err != nil {
		t.Errorf("function without args should not require an args block: %v", err)
	}
}

func TestValidateFileComposition_MutationArgsWithBlock(t *testing.T) {
	// Valid: Mutation with args parameter AND file-top args block.
	source := `
@enabled
@audit
args {
  email  string  @required
  name   string  @required
}
func (Mutation) createUser(args any) {
  insert("v1:user", {
    payload: {
      email: args.email,
      name: args.name,
    },
  })
}
`
	ast := mustParse(t, source)
	file, ok := ast.(*parser.File)
	if !ok {
		t.Fatalf("expected File, got %T", ast)
	}

	err := ValidateFileComposition(file)
	if err != nil {
		t.Errorf("mutation with args block should be valid: %v", err)
	}
}

// mustParse parses source and fails if there's an error.
func mustParse(t *testing.T, source string) parser.Node {
	t.Helper()
	lexer := parser.NewLexer(source)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("lexer error: %v", err)
	}
	p := parser.NewParser(tokens)
	ast, err := p.Parse()
	if err != nil {
		t.Fatalf("parser error: %v", err)
	}
	return ast
}
