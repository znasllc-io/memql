package parser

import (
	"testing"
)

func TestLexer_SimpleQuery(t *testing.T) {
	input := `concept==v1:crm:lead;payload.active==true`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	// Find key tokens we expect
	foundConcept := false
	foundConceptName := false
	foundSemicolon := false
	foundPayloadActive := false

	for _, tok := range tokens {
		switch tok.Literal {
		case "concept":
			foundConcept = true
		case "v1:crm:lead":
			foundConceptName = true
		case ";":
			foundSemicolon = true
		case "payload.active":
			foundPayloadActive = true
		}
	}

	if !foundConcept {
		t.Error("Expected to find 'concept' token")
	}
	if !foundConceptName {
		t.Error("Expected to find 'v1:crm:lead' token")
	}
	if !foundSemicolon {
		t.Error("Expected to find ';' token")
	}
	if !foundPayloadActive {
		t.Error("Expected to find 'payload.active' token")
	}
}

func TestLexer_ConditionalFilter(t *testing.T) {
	input := `concept==v1:user;?.payload.role==args.role`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	// Find the ?. token
	found := false
	for _, tok := range tokens {
		if tok.Type == TokenQuestionDot {
			found = true
			if tok.Literal != "?." {
				t.Errorf("Expected literal '?.', got %q", tok.Literal)
			}
		}
	}

	if !found {
		t.Error("Expected to find TokenQuestionDot token")
	}
}

func TestLexer_Comments(t *testing.T) {
	input := `// This is a comment
concept==v1:test
// Another comment`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	// Comments should be skipped
	if tokens[0].Type != TokenIdentifier || tokens[0].Literal != "concept" {
		t.Errorf("Expected first token to be 'concept', got %v", tokens[0])
	}
}

func TestParser_ForRange_EnforcesItemVarName(t *testing.T) {
	input := `
@enabled
func (Automation) testAuto(_ any) {
  q := query { concept==v1:test }
  for agent := range q.result {
    _ := query { concept==v1:test }
  }
  return q
}`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	p := NewParser(tokens)
	_, err = p.Parse()
	if err == nil {
		t.Fatalf("expected parse error for non-item loop variable")
	}
}

func TestParser_ForRange_GeneratesUniqueForEachStepIds(t *testing.T) {
	input := `
@enabled
func (Automation) testAuto(_ any) {
  q := query { concept==v1:test }
  for item := range q.result {
    _ := query { concept==v1:test }
  }
  for item := range q.result {
    _ := query { concept==v1:test }
  }
  return q
}`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	p := NewParser(tokens)
	node, err := p.Parse()
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}

	file, ok := node.(*File)
	if !ok {
		t.Fatalf("expected *File, got %T", node)
	}

	var auto *AutomationDef
	for _, def := range file.Definitions {
		fn, ok := def.(*FunctionDef)
		if !ok {
			continue
		}
		a, ok := fn.Body.(*AutomationDef)
		if ok {
			auto = a
			break
		}
	}
	if auto == nil {
		t.Fatalf("expected to find AutomationDef")
	}

	seen := map[string]bool{}
	for _, step := range auto.Steps {
		if step.Type != StepTypeForEach {
			continue
		}
		if seen[step.ID] {
			t.Fatalf("duplicate forEach step id: %q", step.ID)
		}
		seen[step.ID] = true
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 forEach steps, got %d", len(seen))
	}
}

func TestLexer_BlockComments(t *testing.T) {
	input := `/* This is a block comment */
concept==v1:test
/* Another
   multiline
   comment */`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	// Comments should be skipped
	if tokens[0].Type != TokenIdentifier || tokens[0].Literal != "concept" {
		t.Errorf("Expected first token to be 'concept', got %v", tokens[0])
	}
}

func TestLexer_Keywords(t *testing.T) {
	input := `func for range if else switch case default continue break return nil retry`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	expectedTypes := []TokenType{
		TokenKeywordFunc,
		TokenKeywordFor,
		TokenKeywordRange,
		TokenKeywordIf,
		TokenKeywordElse,
		TokenKeywordSwitch,
		TokenKeywordCase,
		TokenKeywordDefault,
		TokenKeywordContinue,
		TokenKeywordBreak,
		TokenKeywordReturn,
		TokenKeywordNil,
		TokenKeywordRetry,
		TokenEOF,
	}

	for i, expected := range expectedTypes {
		if tokens[i].Type != expected {
			t.Errorf("Token %d: expected type %v, got %v (%q)", i, expected, tokens[i].Type, tokens[i].Literal)
		}
	}
}

func TestLexer_TypeReceivers(t *testing.T) {
	input := `Query Mutation Automation`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	expectedTypes := []TokenType{
		TokenKeywordQuery,
		TokenKeywordMutation,
		TokenKeywordAutomation,
		TokenEOF,
	}

	for i, expected := range expectedTypes {
		if tokens[i].Type != expected {
			t.Errorf("Token %d: expected type %v, got %v (%q)", i, expected, tokens[i].Type, tokens[i].Literal)
		}
	}
}

func TestLexer_Attributes(t *testing.T) {
	input := `@enabled
@description("Test function")
@trigger(event="test.event")`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	// Expected token pattern: @ identifier ( string/args )
	// @enabled -> @ enabled
	// @description("Test function") -> @ description ( string )
	// @trigger(event="test.event") -> @ trigger ( identifier = string )

	// First token should be @
	if tokens[0].Type != TokenAt {
		t.Errorf("Token 0: expected TokenAt, got %v (%q)", tokens[0].Type, tokens[0].Literal)
	}
	if tokens[0].Literal != "@" {
		t.Errorf("Expected '@', got %q", tokens[0].Literal)
	}

	// Second token should be identifier "enabled"
	if tokens[1].Type != TokenIdentifier {
		t.Errorf("Token 1: expected TokenIdentifier, got %v (%q)", tokens[1].Type, tokens[1].Literal)
	}
	if tokens[1].Literal != "enabled" {
		t.Errorf("Expected 'enabled', got %q", tokens[1].Literal)
	}
}

func TestLexer_StringEscapes(t *testing.T) {
	input := `"hello\nworld\t\"quoted\""`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	if tokens[0].Type != TokenString {
		t.Fatalf("Expected string token, got %v", tokens[0].Type)
	}

	expected := "hello\nworld\t\"quoted\""
	if tokens[0].Literal != expected {
		t.Errorf("Expected %q, got %q", expected, tokens[0].Literal)
	}
}

func TestLexer_Operators(t *testing.T) {
	tests := []struct {
		input        string
		expectedType TokenType
		expected     string
	}{
		{"==", TokenOperator, "=="},
		{"!=", TokenOperator, "!="},
		{">", TokenOperator, ">"},
		{">=", TokenOperator, ">="},
		{"<", TokenOperator, "<"},
		{"<=", TokenOperator, "<="},
		{"in", TokenKeywordIn, "in"},
		{"has", TokenKeywordHas, "has"},
		{"not", TokenKeywordNot, "not"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			lexer := NewLexer(tt.input)
			tokens, err := lexer.Tokenize()
			if err != nil {
				t.Fatalf("Lexer error: %v", err)
			}

			if tokens[0].Type != tt.expectedType {
				t.Errorf("Expected token type %v, got %v", tt.expectedType, tokens[0].Type)
			}
			if tokens[0].Literal != tt.expected {
				t.Errorf("Expected %q, got %q", tt.expected, tokens[0].Literal)
			}
		})
	}
}

func TestParser_SimpleQuery(t *testing.T) {
	input := `concept==v1:test`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	comp, ok := ast.(*ComparisonExpr)
	if !ok {
		t.Fatalf("Expected ComparisonExpr, got %T", ast)
	}

	if comp.Field.Raw != "concept" {
		t.Errorf("Expected field 'concept', got %q", comp.Field.Raw)
	}
	if comp.Operator != OpEq {
		t.Errorf("Expected operator '==', got %q", comp.Operator)
	}
	if comp.Value != "v1:test" {
		t.Errorf("Expected value 'v1:test', got %v", comp.Value)
	}
}

func TestParser_LogicalAnd(t *testing.T) {
	input := `concept==v1:test;payload.active==true`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	logical, ok := ast.(*LogicalExpr)
	if !ok {
		t.Fatalf("Expected LogicalExpr, got %T", ast)
	}

	if logical.Op != LogicalAnd {
		t.Errorf("Expected AND operator, got %v", logical.Op)
	}
}

func TestParser_FunctionCall(t *testing.T) {
	input := `parentOf(concept==v1:test)`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	// parentOf is a wrapper function, so it returns RelationshipExpr
	relExpr, ok := ast.(*RelationshipExpr)
	if !ok {
		t.Fatalf("Expected RelationshipExpr, got %T", ast)
	}

	// The target should be a comparison
	_, ok = relExpr.Target.(*ComparisonExpr)
	if !ok {
		t.Errorf("Expected target to be ComparisonExpr, got %T", relExpr.Target)
	}
}

func TestParser_RegularFunctionCall(t *testing.T) {
	input := `myFunc("arg1", name="value")`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	fn, ok := ast.(*FunctionCallExpr)
	if !ok {
		t.Fatalf("Expected FunctionCallExpr, got %T", ast)
	}

	if fn.Name != "myFunc" {
		t.Errorf("Expected function name 'myFunc', got %q", fn.Name)
	}
}

func TestParser_AutomationStep_FunctionCall(t *testing.T) {
	input := `
@enabled
func (Automation) testAuto(_ any) {
  checkUser := userById(userId=event.payload.userId)
  return checkUser
}`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("expected *File, got %T", ast)
	}
	if len(file.Definitions) != 1 {
		t.Fatalf("expected 1 definition, got %d", len(file.Definitions))
	}

	fn, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("expected *FunctionDef, got %T", file.Definitions[0])
	}
	auto, ok := fn.Body.(*AutomationDef)
	if !ok {
		t.Fatalf("expected *AutomationDef body, got %T", fn.Body)
	}
	if len(auto.Steps) < 1 {
		t.Fatalf("expected at least 1 step")
	}

	step := auto.Steps[0]
	if step.Type != StepTypeFunction {
		t.Fatalf("expected function step, got %s", step.Type)
	}
	cfg, ok := step.Config.(*FunctionStepConfig)
	if !ok {
		t.Fatalf("expected *FunctionStepConfig, got %T", step.Config)
	}
	if cfg.Name != "userById" {
		t.Fatalf("expected function name userById, got %q", cfg.Name)
	}
	if _, ok := cfg.Args["userId"]; !ok {
		t.Fatalf("expected userId arg to be present")
	}
}

func TestParser_AutomationStep_ConditionalFunctionCall(t *testing.T) {
	input := `
@enabled
func (Automation) testAuto(_ any) {
  createSession := if first(checkExisting).id==nil {
    mutationCreateSession(partitionId=event.payload.partitionId, participantId=event.payload.id)
  }
  return createSession
}`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("parser error: %v", err)
	}

	file := ast.(*File)
	fn := file.Definitions[0].(*FunctionDef)
	auto := fn.Body.(*AutomationDef)
	step := auto.Steps[0]

	if step.Type != StepTypeFunction {
		t.Fatalf("expected function step, got %s", step.Type)
	}
	if step.Condition == "" {
		t.Fatalf("expected conditional function step")
	}
	cfg, ok := step.Config.(*FunctionStepConfig)
	if !ok {
		t.Fatalf("expected *FunctionStepConfig, got %T", step.Config)
	}
	if cfg.Name != "mutationCreateSession" {
		t.Fatalf("expected function name mutationCreateSession, got %q", cfg.Name)
	}
}

func TestParser_AutomationDefinition(t *testing.T) {
	input := `
@enabled
func (Automation) testAutomation(_ any) {
	step1 := query {
		concept==v1:test
	}
	return step1
}`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	if len(file.Definitions) != 1 {
		t.Fatalf("Expected 1 definition, got %d", len(file.Definitions))
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	if funcDef.Name != "testAutomation" {
		t.Errorf("Expected name 'testAutomation', got %q", funcDef.Name)
	}

	// Check receiver type instead of deprecated funcDef.Type
	if funcDef.Receiver == nil {
		t.Fatal("Expected receiver to be set")
	}
	if funcDef.Receiver.Type != ReceiverAutomation {
		t.Errorf("Expected receiver type Automation, got %v", funcDef.Receiver.Type)
	}

	if len(funcDef.Args) != 1 {
		t.Errorf("Expected 1 arg, got %d", len(funcDef.Args))
	}
}

func TestParser_QueryFunction(t *testing.T) {
	input := `
func (Query) activeUsers(args any) (any, error) {
	return concept==v1:user;?.payload.role==args.role, nil
}`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	if len(file.Definitions) != 1 {
		t.Fatalf("Expected 1 definition, got %d", len(file.Definitions))
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	// Check receiver type instead of deprecated funcDef.Type
	if funcDef.Receiver == nil {
		t.Fatal("Expected receiver to be set")
	}
	if funcDef.Receiver.Type != ReceiverQuery {
		t.Errorf("Expected receiver type Query, got %v", funcDef.Receiver.Type)
	}
}

func TestParser_MutationFunction(t *testing.T) {
	input := `
func (Mutation) createUser(args any) error {
	return insert("v1:user", id="test-user", payload={"name": "Test"})
}`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	if len(file.Definitions) != 1 {
		t.Fatalf("Expected 1 definition, got %d", len(file.Definitions))
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	// Check receiver type instead of deprecated funcDef.Type
	if funcDef.Receiver == nil {
		t.Fatal("Expected receiver to be set")
	}
	if funcDef.Receiver.Type != ReceiverMutation {
		t.Errorf("Expected receiver type Mutation, got %v", funcDef.Receiver.Type)
	}
}

// ----------------------------------------------------------------------------
// Tests for New Accessor Functions
// ----------------------------------------------------------------------------

func TestParser_VarAccessor(t *testing.T) {
	input := `var("MEMQL_DEFAULT_USER_ROLE")`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	varRef, ok := ast.(*VarRefExpr)
	if !ok {
		t.Fatalf("Expected VarRefExpr, got %T", ast)
	}

	if varRef.Name != "MEMQL_DEFAULT_USER_ROLE" {
		t.Errorf("Expected var name 'MEMQL_DEFAULT_USER_ROLE', got %q", varRef.Name)
	}
}

func TestParser_StepAccessor(t *testing.T) {
	input := `step("checkUser")`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	stepRef, ok := ast.(*StepRefExpr)
	if !ok {
		t.Fatalf("Expected StepRefExpr, got %T", ast)
	}

	if stepRef.StepId != "checkUser" {
		t.Errorf("Expected step ID 'checkUser', got %q", stepRef.StepId)
	}
}

func TestParser_ArgAccessor(t *testing.T) {
	input := `args.authorizerId`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	argRef, ok := ast.(*ArgRefExpr)
	if !ok {
		t.Fatalf("Expected ArgRefExpr, got %T", ast)
	}

	if argRef.Path != "authorizerId" {
		t.Errorf("Expected arg path 'authorizerId', got %q", argRef.Path)
	}
}

func TestParser_ConcatFunction(t *testing.T) {
	input := `concat("user-", args.id)`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	concatExpr, ok := ast.(*ConcatExpr)
	if !ok {
		t.Fatalf("Expected ConcatExpr, got %T", ast)
	}

	if len(concatExpr.Args) != 2 {
		t.Errorf("Expected 2 args, got %d", len(concatExpr.Args))
	}

	// First arg should be literal string
	lit, ok := concatExpr.Args[0].(*LiteralExpr)
	if !ok {
		t.Errorf("Expected first arg to be LiteralExpr, got %T", concatExpr.Args[0])
	} else if lit.Value != "user-" {
		t.Errorf("Expected first arg value 'user-', got %v", lit.Value)
	}

	// Second arg should be ArgRefExpr
	argRef, ok := concatExpr.Args[1].(*ArgRefExpr)
	if !ok {
		t.Errorf("Expected second arg to be ArgRefExpr, got %T", concatExpr.Args[1])
	} else if argRef.Path != "id" {
		t.Errorf("Expected arg path 'id', got %q", argRef.Path)
	}
}

func TestParser_CoalesceFunction(t *testing.T) {
	input := `coalesce(step("create"), step("existing"))`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	coalesceExpr, ok := ast.(*CoalesceExpr)
	if !ok {
		t.Fatalf("Expected CoalesceExpr, got %T", ast)
	}

	if len(coalesceExpr.Args) != 2 {
		t.Errorf("Expected 2 args, got %d", len(coalesceExpr.Args))
	}
}

func TestParser_CondFunction(t *testing.T) {
	input := `cond(args.flag, "yes", "no")`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	condExpr, ok := ast.(*CondExpr)
	if !ok {
		t.Fatalf("Expected CondExpr, got %T", ast)
	}

	// Condition should be ArgRefExpr
	_, ok = condExpr.Condition.(*ArgRefExpr)
	if !ok {
		t.Errorf("Expected condition to be ArgRefExpr, got %T", condExpr.Condition)
	}

	// Then should be literal "yes"
	thenLit, ok := condExpr.Then.(*LiteralExpr)
	if !ok {
		t.Errorf("Expected Then to be LiteralExpr, got %T", condExpr.Then)
	} else if thenLit.Value != "yes" {
		t.Errorf("Expected Then value 'yes', got %v", thenLit.Value)
	}

	// Else should be literal "no"
	elseLit, ok := condExpr.Else.(*LiteralExpr)
	if !ok {
		t.Errorf("Expected Else to be LiteralExpr, got %T", condExpr.Else)
	} else if elseLit.Value != "no" {
		t.Errorf("Expected Else value 'no', got %v", elseLit.Value)
	}
}

func TestParser_FirstLastFunctions(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`first(step("users"))`, "first"},
		{`last(step("users"))`, "last"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			lexer := NewLexer(tt.input)
			tokens, err := lexer.Tokenize()
			if err != nil {
				t.Fatalf("Lexer error: %v", err)
			}

			parser := NewParser(tokens)
			ast, err := parser.Parse()
			if err != nil {
				t.Fatalf("Parser error: %v", err)
			}

			switch tt.expected {
			case "first":
				if _, ok := ast.(*FirstExpr); !ok {
					t.Fatalf("Expected FirstExpr, got %T", ast)
				}
			case "last":
				if _, ok := ast.(*LastExpr); !ok {
					t.Fatalf("Expected LastExpr, got %T", ast)
				}
			}
		})
	}
}

func TestParser_TimestampFunction(t *testing.T) {
	tests := []string{"timestamp()", "now()"}

	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			lexer := NewLexer(input)
			tokens, err := lexer.Tokenize()
			if err != nil {
				t.Fatalf("Lexer error: %v", err)
			}

			parser := NewParser(tokens)
			ast, err := parser.Parse()
			if err != nil {
				t.Fatalf("Parser error: %v", err)
			}

			_, ok := ast.(*TimestampExprFunc)
			if !ok {
				t.Fatalf("Expected TimestampExprFunc, got %T", ast)
			}
		})
	}
}

func TestParser_MemqlVersionFunction(t *testing.T) {
	lexer := NewLexer("memqlVersion()")
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	builtin, ok := ast.(*BuiltinFunctionExpr)
	if !ok {
		t.Fatalf("Expected BuiltinFunctionExpr, got %T", ast)
	}
	if builtin.Name != "memqlVersion" {
		t.Fatalf("Expected builtin name memqlVersion, got %q", builtin.Name)
	}
	if builtin.Executor != "serviceVersion" {
		t.Fatalf("Expected executor serviceVersion, got %q", builtin.Executor)
	}
}

func TestParser_NoArgAccessors(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`input()`, "input"},
		{`item()`, "item"},
		{`index()`, "index"},
		{`event()`, "event"},
		{`error()`, "error"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			lexer := NewLexer(tt.input)
			tokens, err := lexer.Tokenize()
			if err != nil {
				t.Fatalf("Lexer error: %v", err)
			}

			parser := NewParser(tokens)
			ast, err := parser.Parse()
			if err != nil {
				t.Fatalf("Parser error: %v", err)
			}

			switch tt.expected {
			case "input":
				if _, ok := ast.(*InputRefExpr); !ok {
					t.Fatalf("Expected InputRefExpr, got %T", ast)
				}
			case "item":
				if _, ok := ast.(*ItemRefExpr); !ok {
					t.Fatalf("Expected ItemRefExpr, got %T", ast)
				}
			case "index":
				if _, ok := ast.(*IndexRefExpr); !ok {
					t.Fatalf("Expected IndexRefExpr, got %T", ast)
				}
			case "event":
				if _, ok := ast.(*EventRefExpr); !ok {
					t.Fatalf("Expected EventRefExpr, got %T", ast)
				}
			case "error":
				if _, ok := ast.(*ErrorRefExpr); !ok {
					t.Fatalf("Expected ErrorRefExpr, got %T", ast)
				}
			}
		})
	}
}

func TestParser_FieldAccessor(t *testing.T) {
	input := `field(item(), "name")`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	fieldRef, ok := ast.(*FieldRefExpr)
	if !ok {
		t.Fatalf("Expected FieldRefExpr, got %T", ast)
	}

	if fieldRef.Key != "name" {
		t.Errorf("Expected key 'name', got %q", fieldRef.Key)
	}

	_, ok = fieldRef.Object.(*ItemRefExpr)
	if !ok {
		t.Errorf("Expected object to be ItemRefExpr, got %T", fieldRef.Object)
	}
}

func TestParser_StringFunctions(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`lower(args.email)`, "lower"},
		{`upper(args.code)`, "upper"},
		{`trim(args.input)`, "trim"},
		{`hash(args.email)`, "hash"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			lexer := NewLexer(tt.input)
			tokens, err := lexer.Tokenize()
			if err != nil {
				t.Fatalf("Lexer error: %v", err)
			}

			parser := NewParser(tokens)
			ast, err := parser.Parse()
			if err != nil {
				t.Fatalf("Parser error: %v", err)
			}

			switch tt.expected {
			case "lower":
				if _, ok := ast.(*LowerExpr); !ok {
					t.Fatalf("Expected LowerExpr, got %T", ast)
				}
			case "upper":
				if _, ok := ast.(*UpperExpr); !ok {
					t.Fatalf("Expected UpperExpr, got %T", ast)
				}
			case "trim":
				if _, ok := ast.(*TrimExpr); !ok {
					t.Fatalf("Expected TrimExpr, got %T", ast)
				}
			case "hash":
				if _, ok := ast.(*HashExpr); !ok {
					t.Fatalf("Expected HashExpr, got %T", ast)
				}
			}
		})
	}
}

// ----------------------------------------------------------------------------
// Tests for Ternary Operator
// ----------------------------------------------------------------------------

func TestParser_TernaryOperator(t *testing.T) {
	input := `args.flag ? "yes" : "no"`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	ternary, ok := ast.(*TernaryExpr)
	if !ok {
		t.Fatalf("Expected TernaryExpr, got %T", ast)
	}

	// Condition should be ArgRefExpr
	_, ok = ternary.Condition.(*ArgRefExpr)
	if !ok {
		t.Errorf("Expected condition to be ArgRefExpr, got %T", ternary.Condition)
	}

	// Then should be literal "yes"
	thenLit, ok := ternary.Then.(*LiteralExpr)
	if !ok {
		t.Errorf("Expected Then to be LiteralExpr, got %T", ternary.Then)
	} else if thenLit.Value != "yes" {
		t.Errorf("Expected Then value 'yes', got %v", thenLit.Value)
	}

	// Else should be literal "no"
	elseLit, ok := ternary.Else.(*LiteralExpr)
	if !ok {
		t.Errorf("Expected Else to be LiteralExpr, got %T", ternary.Else)
	} else if elseLit.Value != "no" {
		t.Errorf("Expected Else value 'no', got %v", elseLit.Value)
	}
}

func TestParser_NestedTernary(t *testing.T) {
	input := `args.a ? args.b ? "both" : "a-only" : "none"`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	ternary, ok := ast.(*TernaryExpr)
	if !ok {
		t.Fatalf("Expected TernaryExpr, got %T", ast)
	}

	// Then should be another ternary
	_, ok = ternary.Then.(*TernaryExpr)
	if !ok {
		t.Errorf("Expected Then to be TernaryExpr, got %T", ternary.Then)
	}
}

// ----------------------------------------------------------------------------
// Tests for Automation with Steps
// ----------------------------------------------------------------------------

func TestParser_AutomationWithMutationStep(t *testing.T) {
	input := `
func (Automation) bootstrap(ctx any) {
	checkUser := query {
		concept==v1:user
	}

	createUser := mutation if checkUser.metadata.itemCount == 0 {
		insert("v1:user", id=concat("user-", ctx.userId))
	}

	return createUser
}`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	// Verify receiver type
	if funcDef.Receiver == nil {
		t.Fatal("Expected receiver to be set")
	}
	if funcDef.Receiver.Type != ReceiverAutomation {
		t.Errorf("Expected receiver type Automation, got %v", funcDef.Receiver.Type)
	}

	// Verify function has one argument
	if len(funcDef.Args) != 1 {
		t.Fatalf("Expected 1 arg, got %d", len(funcDef.Args))
	}
	if funcDef.Args[0].Name != "ctx" {
		t.Errorf("Expected arg name 'ctx', got %q", funcDef.Args[0].Name)
	}

	// The body structure is an internal implementation detail.
	// Just verify the parser succeeded without errors.
	if funcDef.Body == nil {
		t.Error("Expected function body to be set")
	}
}

func TestParser_AttributeSimple(t *testing.T) {
	input := `
@enabled
func (Automation) myAutomation(_ any) {
  checkUser := query {
    concept==v1:user
  }
  return checkUser
}
`
	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	// Should have receiver
	if funcDef.Receiver == nil {
		t.Fatal("Expected receiver to be set")
	}
	if funcDef.Receiver.Type != ReceiverAutomation {
		t.Errorf("Expected receiver type Automation, got %v", funcDef.Receiver.Type)
	}

	// Should have attributes
	if len(funcDef.Attributes) != 1 {
		t.Fatalf("Expected 1 attribute, got %d", len(funcDef.Attributes))
	}
	if funcDef.Attributes[0].Name != "enabled" {
		t.Errorf("Expected attribute name 'enabled', got %q", funcDef.Attributes[0].Name)
	}

	// Should be enabled
	if !funcDef.Enabled {
		t.Error("Expected function to be enabled due to @enabled attribute")
	}
}

func TestParser_AttributeWithArgs(t *testing.T) {
	input := `
@enabled
@trigger(event="session.opened")
@description("Auto-provision user")
func (Automation) bootstrapUser(_ any) {
  checkUser := query {
    concept==v1:user
  }
  return checkUser
}
`
	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	// Should have 3 attributes
	if len(funcDef.Attributes) != 3 {
		t.Fatalf("Expected 3 attributes, got %d", len(funcDef.Attributes))
	}

	// Check enabled
	if !funcDef.Enabled {
		t.Error("Expected function to be enabled")
	}

	// Check trigger (on automation body)
	automation, ok := funcDef.Body.(*AutomationDef)
	if !ok {
		t.Fatalf("Expected AutomationDef body, got %T", funcDef.Body)
	}
	if automation.Trigger == nil {
		t.Fatal("Expected trigger to be set")
	}
	if automation.Trigger.Event != "session.opened" {
		t.Errorf("Expected trigger event 'session.opened', got %q", automation.Trigger.Event)
	}

	// Check description
	if funcDef.Description != "Auto-provision user" {
		t.Errorf("Expected description 'Auto-provision user', got %q", funcDef.Description)
	}
}

func TestParser_GoStyleQuery(t *testing.T) {
	input := `
@enabled
@description("Returns active users")
func (Query) activeUsers(args any) (any, error) {
  return concept==v1:user; payload.active==true, nil
}
`
	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	// Should have Query receiver
	if funcDef.Receiver == nil {
		t.Fatal("Expected receiver to be set")
	}
	if funcDef.Receiver.Type != ReceiverQuery {
		t.Errorf("Expected receiver type Query, got %v", funcDef.Receiver.Type)
	}

	// Should have args
	if len(funcDef.Args) != 1 {
		t.Fatalf("Expected 1 arg, got %d", len(funcDef.Args))
	}
	if funcDef.Args[0].Name != "args" {
		t.Errorf("Expected arg name 'args', got %q", funcDef.Args[0].Name)
	}
	if funcDef.Args[0].Type != "any" {
		t.Errorf("Expected arg type 'any', got %q", funcDef.Args[0].Type)
	}

	// Should have strict query returns.
	if len(funcDef.Returns) != 2 {
		t.Fatalf("Expected 2 returns, got %d", len(funcDef.Returns))
	}
}

func TestParser_GoStyleSchedule(t *testing.T) {
	input := `
@enabled
@schedule(cron="*/30 * * * *")
func (Automation) scheduledTask(_ any) {
  doWork := query {
    concept==v1:task
  }
  return doWork
}
`
	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	automation, ok := funcDef.Body.(*AutomationDef)
	if !ok {
		t.Fatalf("Expected AutomationDef body, got %T", funcDef.Body)
	}

	if automation.Schedule != "*/30 * * * *" {
		t.Errorf("Expected schedule '*/30 * * * *', got %q", automation.Schedule)
	}
}

func TestParser_GoStyleDefaultEnabled(t *testing.T) {
	// Without any annotation, a function is enabled by default. @disabled
	// is the explicit opt-out.
	input := `
func (Automation) myAutomation(_ any) {
  step1 := query {
    concept==v1:user
  }
  return step1
}
`
	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	if !funcDef.Enabled {
		t.Error("Expected function to be enabled by default (no @disabled attribute)")
	}
}

func TestParser_GoStyleDisabledAttribute(t *testing.T) {
	// @disabled is the explicit opt-out from the default-enabled behaviour.
	input := `
@disabled
func (Automation) myAutomation(_ any) {
  step1 := query {
    concept==v1:user
  }
  return step1
}
`
	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	if funcDef.Enabled {
		t.Error("Expected function to be disabled by @disabled attribute")
	}
}

func TestParser_ArgsAttribute(t *testing.T) {
	input := `
@enabled
@args({ "userId": { "type": "string", "required": true }, "limit": { "type": "number", "default": 10 } })
func (Query) searchUsers(args any) (any, error) {
  return concept==v1:user; ?.payload.userId==args.userId, nil
}
`
	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	// Should have 2 attributes
	if len(funcDef.Attributes) != 2 {
		t.Fatalf("Expected 2 attributes, got %d", len(funcDef.Attributes))
	}

	// Find args attribute
	var argsAttr *Attribute
	for _, attr := range funcDef.Attributes {
		if attr.Name == "args" {
			argsAttr = attr
			break
		}
	}
	if argsAttr == nil {
		t.Fatal("Expected to find @args attribute")
	}

	// Check that it has a Value (the object)
	if argsAttr.Value == nil {
		t.Fatal("Expected @args attribute to have a value")
	}

	// Value should be a map
	argsSchema, ok := argsAttr.Value.(map[string]any)
	if !ok {
		t.Fatalf("Expected @args value to be map[string]any, got %T", argsAttr.Value)
	}

	// Should have userId and limit
	if _, hasUserId := argsSchema["userId"]; !hasUserId {
		t.Error("Expected argsSchema to have 'userId' key")
	}
	if _, hasLimit := argsSchema["limit"]; !hasLimit {
		t.Error("Expected argsSchema to have 'limit' key")
	}
}

func TestParser_ErrorFunction(t *testing.T) {
	// Test error() function for creating errors
	input := `error("Something went wrong")`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	errorExpr, ok := ast.(*ErrorExpr)
	if !ok {
		t.Fatalf("Expected ErrorExpr, got %T", ast)
	}

	// Check message
	if errorExpr.Message == nil {
		t.Fatal("Expected error message to be set")
	}

	literal, ok := errorExpr.Message.(*LiteralExpr)
	if !ok {
		t.Fatalf("Expected LiteralExpr message, got %T", errorExpr.Message)
	}

	if literal.Value != "Something went wrong" {
		t.Errorf("Expected message 'Something went wrong', got %v", literal.Value)
	}
}

func TestParser_ErrorFunctionEmpty(t *testing.T) {
	// Test error() with no args (references current error)
	input := `error()`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	_, ok := ast.(*ErrorRefExpr)
	if !ok {
		t.Fatalf("Expected ErrorRefExpr for empty error(), got %T", ast)
	}
}

func TestParser_SpecDefinition(t *testing.T) {
	input := `
@enabled
@description("Node includes both email and phone number fields.")
func (Spec) hasUserContact() bool {
  return payload.email!=nil;payload.phoneNumber!=nil
}`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	if len(file.Definitions) != 1 {
		t.Fatalf("Expected 1 definition, got %d", len(file.Definitions))
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	// Check receiver type
	if funcDef.Receiver == nil {
		t.Fatal("Expected Receiver to be set")
	}
	if funcDef.Receiver.Type != ReceiverSpec {
		t.Errorf("Expected ReceiverSpec, got %v", funcDef.Receiver.Type)
	}

	// Check function type
	if funcDef.Type != FunctionTypeSpec {
		t.Errorf("Expected FunctionTypeSpec, got %v", funcDef.Type)
	}

	// Check name
	if funcDef.Name != "hasUserContact" {
		t.Errorf("Expected name 'hasUserContact', got %q", funcDef.Name)
	}

	// Check body is a logical expression (AND of two comparisons)
	body, ok := funcDef.Body.(*LogicalExpr)
	if !ok {
		t.Fatalf("Expected LogicalExpr, got %T", funcDef.Body)
	}
	if body.Op != LogicalAnd {
		t.Errorf("Expected LogicalAnd, got %v", body.Op)
	}
}

func TestParser_SpecWithOrCondition(t *testing.T) {
	input := `
@enabled
@description("Node has at least one contact method")
func (Spec) hasContactMethod() bool {
  return payload.email!=nil,payload.phone!=nil
}`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	// Check receiver type
	if funcDef.Receiver.Type != ReceiverSpec {
		t.Errorf("Expected ReceiverSpec, got %v", funcDef.Receiver.Type)
	}

	// Check body is an OR expression
	body, ok := funcDef.Body.(*LogicalExpr)
	if !ok {
		t.Fatalf("Expected LogicalExpr, got %T", funcDef.Body)
	}
	if body.Op != LogicalOr {
		t.Errorf("Expected LogicalOr, got %v", body.Op)
	}
}

func TestParser_SpecWithRelationship(t *testing.T) {
	input := `
@enabled
@description("Node's parent has status active")
func (Spec) hasActiveParent() bool {
  return parentOf(payload.status=="active")
}`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	// Check receiver type
	if funcDef.Receiver.Type != ReceiverSpec {
		t.Errorf("Expected ReceiverSpec, got %v", funcDef.Receiver.Type)
	}

	// Check body is a relationship expression
	body, ok := funcDef.Body.(*RelationshipExpr)
	if !ok {
		t.Fatalf("Expected RelationshipExpr, got %T", funcDef.Body)
	}
	if body.Function != RelParentOf {
		t.Errorf("Expected RelParentOf, got %v", body.Function)
	}
}

// TestParser_ConditionalFilterWithArgsFieldName verifies that:
// 1. args.fieldName syntax is converted to ArgRefExpr (not a plain string)
// 2. ConditionalFilterExpr.ArgPath is correctly extracted from the comparison value
func TestParser_ConditionalFilterWithArgsFieldName(t *testing.T) {
	input := `
@enabled
args {
  partitionId  string
  status   string
}
func (Query) spaceParticipants(args any) (any, error) {
  return concept==v1:cognition:participant;
  ?.payload.partitionId==args.partitionId;
  ?.payload.status==args.status, nil
}
`
	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	parser := NewParser(tokens)
	ast, err := parser.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}

	funcDef, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}

	// The body should be a LogicalExpr (AND of comparisons)
	logicalExpr, ok := funcDef.Body.(*LogicalExpr)
	if !ok {
		t.Fatalf("Expected LogicalExpr, got %T", funcDef.Body)
	}

	// Traverse the logical expression tree to find conditional filters
	// The structure is: (concept==... AND (?.payload.partitionId==args.partitionId AND ?.payload.status==args.status))
	var conditionalFilters []*ConditionalFilterExpr
	collectConditionalFilters(logicalExpr, &conditionalFilters)

	if len(conditionalFilters) != 2 {
		t.Fatalf("Expected 2 conditional filters, got %d", len(conditionalFilters))
	}

	// Check first conditional filter: ?.payload.partitionId==args.partitionId
	filter1 := conditionalFilters[0]
	if filter1.ArgPath != "partitionId" {
		t.Errorf("Expected ArgPath 'partitionId', got %q", filter1.ArgPath)
	}

	comp1, ok := filter1.Filter.(*ComparisonExpr)
	if !ok {
		t.Fatalf("Expected ComparisonExpr in filter, got %T", filter1.Filter)
	}
	if comp1.Field.Raw != "payload.partitionId" {
		t.Errorf("Expected field 'payload.partitionId', got %q", comp1.Field.Raw)
	}

	// Value should be ArgRefExpr, not a string
	argRef1, ok := comp1.Value.(*ArgRefExpr)
	if !ok {
		t.Fatalf("Expected ArgRefExpr, got %T (value: %v)", comp1.Value, comp1.Value)
	}
	if argRef1.Path != "partitionId" {
		t.Errorf("Expected ArgRefExpr.Path 'partitionId', got %q", argRef1.Path)
	}

	// Check second conditional filter: ?.payload.status==args.status
	filter2 := conditionalFilters[1]
	if filter2.ArgPath != "status" {
		t.Errorf("Expected ArgPath 'status', got %q", filter2.ArgPath)
	}

	comp2, ok := filter2.Filter.(*ComparisonExpr)
	if !ok {
		t.Fatalf("Expected ComparisonExpr in filter, got %T", filter2.Filter)
	}

	argRef2, ok := comp2.Value.(*ArgRefExpr)
	if !ok {
		t.Fatalf("Expected ArgRefExpr, got %T (value: %v)", comp2.Value, comp2.Value)
	}
	if argRef2.Path != "status" {
		t.Errorf("Expected ArgRefExpr.Path 'status', got %q", argRef2.Path)
	}
}

// collectConditionalFilters traverses a logical expression tree and collects all ConditionalFilterExpr nodes.
func collectConditionalFilters(expr ExpressionNode, filters *[]*ConditionalFilterExpr) {
	if expr == nil {
		return
	}
	switch node := expr.(type) {
	case *LogicalExpr:
		collectConditionalFilters(node.Left, filters)
		collectConditionalFilters(node.Right, filters)
	case *ConditionalFilterExpr:
		*filters = append(*filters, node)
	}
}

func TestParser_MutationBody_PreservesIdTemplate_ArgAccessor(t *testing.T) {
	input := `
@enabled
func (Mutation) createThing(args any) error {
  return insert("v1:thing",
    id=args.partitionId,
    payload={ name: "X" }
  )
}`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	p := NewParser(tokens)
	ast, err := p.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}
	def, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}
	m, ok := def.Body.(*MutationStmt)
	if !ok || m == nil {
		t.Fatalf("Expected MutationStmt, got %T", def.Body)
	}

	argRef, ok := m.IDTemplate.(*ArgRefExpr)
	if !ok {
		t.Fatalf("Expected IDTemplate ArgRefExpr, got %T", m.IDTemplate)
	}
	if argRef.Path != "partitionId" {
		t.Fatalf("Expected ArgRefExpr.Path 'partitionId', got %q", argRef.Path)
	}
}

func TestParser_MutationBody_PreservesIdTemplate_Concat(t *testing.T) {
	input := `
@enabled
func (Mutation) createThing(args any) error {
  return insert("v1:thing",
    id=concat("thing-", hash(concat(args.partitionId, ":", args.userId))),
    payload={ name: "X" }
  )
}`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	p := NewParser(tokens)
	ast, err := p.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}
	def, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}
	m, ok := def.Body.(*MutationStmt)
	if !ok || m == nil {
		t.Fatalf("Expected MutationStmt, got %T", def.Body)
	}

	if _, ok := m.IDTemplate.(*ConcatExpr); !ok {
		t.Fatalf("Expected IDTemplate ConcatExpr, got %T", m.IDTemplate)
	}
}

func TestParser_MutationBody_PreservesCreatedAtTemplate_ArgAccessor(t *testing.T) {
	input := `
@enabled
func (Mutation) createThing(args any) error {
  return insert("v1:thing",
    createdAt=args.createdAt,
    payload={ name: "X" }
  )
}`

	lexer := NewLexer(input)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Lexer error: %v", err)
	}

	p := NewParser(tokens)
	ast, err := p.Parse()
	if err != nil {
		t.Fatalf("Parser error: %v", err)
	}

	file, ok := ast.(*File)
	if !ok {
		t.Fatalf("Expected File, got %T", ast)
	}
	def, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("Expected FunctionDef, got %T", file.Definitions[0])
	}
	m, ok := def.Body.(*MutationStmt)
	if !ok || m == nil {
		t.Fatalf("Expected MutationStmt, got %T", def.Body)
	}

	argRef, ok := m.CreatedAtTemplate.(*ArgRefExpr)
	if !ok {
		t.Fatalf("Expected CreatedAtTemplate ArgRefExpr, got %T", m.CreatedAtTemplate)
	}
	if argRef.Path != "createdAt" {
		t.Fatalf("Expected ArgRefExpr.Path 'createdAt', got %q", argRef.Path)
	}
}

// Smoke-test the update() form: it should parse the same way as
// insert() but stamp Kind=update on the resulting MutationStmt.
// Insert-form sanity check is paired in to lock both kinds in.
func TestParser_MutationBody_KindInsertVsUpdate(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantKind MutationKind
	}{
		{
			name: "insert keyword stamps Kind=insert",
			input: `
@enabled
func (Mutation) createThing(args any) error {
  return insert("v1:thing", id=args.id, payload={ name: args.name })
}`,
			wantKind: MutationKindInsert,
		},
		{
			name: "update keyword stamps Kind=update",
			input: `
@enabled
func (Mutation) editThing(args any) error {
  return update("v1:thing", id=args.id, payload={ status: args.status })
}`,
			wantKind: MutationKindUpdate,
		},
		// (The "implicit-concept update form (use directive style)"
		// case was retired in PR C; the legacy single-segment
		// `use <name>` shape is rejected at parse time post-lockdown.
		// The procedural-form `update(...)` call now requires either
		// an explicit `concept` first argument or the canonical
		// struct-form `mutation <Concept> <name> { ... update { ... } }`
		// binding.)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lexer := NewLexer(tc.input)
			tokens, err := lexer.Tokenize()
			if err != nil {
				t.Fatalf("Lexer error: %v", err)
			}
			p := NewParser(tokens)
			astNode, err := p.Parse()
			if err != nil {
				t.Fatalf("Parser error: %v", err)
			}
			file, ok := astNode.(*File)
			if !ok {
				t.Fatalf("Expected File, got %T", astNode)
			}
			def, ok := file.Definitions[len(file.Definitions)-1].(*FunctionDef)
			if !ok {
				t.Fatalf("Expected last definition to be FunctionDef, got %T", file.Definitions[len(file.Definitions)-1])
			}
			m, ok := def.Body.(*MutationStmt)
			if !ok || m == nil {
				t.Fatalf("Expected MutationStmt, got %T", def.Body)
			}
			if m.Kind != tc.wantKind {
				t.Fatalf("Kind: got %q, want %q", m.Kind, tc.wantKind)
			}
		})
	}
}
