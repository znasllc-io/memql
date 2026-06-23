package automations_test

import (
	"context"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/events"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// parseEmitLogicBody parses a Logic source string and returns the parsed
// *AutomationDef body RunLogic receives in production. Reimplemented here
// (external test package) since the internal parseLogicBody helper isn't
// reachable from package automations_test.
func parseEmitLogicBody(t *testing.T, src string) *languageParser.AutomationDef {
	t.Helper()
	normalised, err := languageParser.NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll: %v", err)
	}
	lexer := languageParser.NewLexer(normalised)
	tokens, err := lexer.Tokenize()
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	ast, err := languageParser.NewParser(tokens).Parse()
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	file, ok := ast.(*languageParser.File)
	if !ok {
		t.Fatalf("expected *File, got %T", ast)
	}
	for _, def := range file.Definitions {
		fd, ok := def.(*languageParser.FunctionDef)
		if !ok || fd.Type != languageParser.FunctionTypeLogic {
			continue
		}
		body, ok := fd.Body.(*languageParser.AutomationDef)
		if !ok {
			t.Fatalf("expected *AutomationDef body, got %T", fd.Body)
		}
		return body
	}
	t.Fatalf("no logic function found in source")
	return nil
}

// TestLogicRunner_EmitStepPublishesToEngineBus is the regression for
// memql#572: an `emit` (publishEvent) step INSIDE a logic body must publish
// to the engine's event bus.
//
// Before the fix, RunLogic built its StepContext with a nil EventBus, so any
// logic that emits (logicAutoJoinAI's emitAutoJoinComplete,
// bootstrapSession's session.created) failed its emit step with "event
// bus not configured". Because the compiler topologically orders steps with
// no inter-dependency arbitrarily, that abort could land BEFORE the
// load-bearing mutation step (the AI join / session insert), so the side
// effect never ran -- the assistant never auto-joined the space and voice
// could never engage.
func TestLogicRunner_EmitStepPublishesToEngineBus(t *testing.T) {
	bus := events.NewBus()
	defer bus.Close()

	received := make(chan events.Event, 1)
	unsub := bus.Subscribe("si.auto-joined", func(e events.Event) { received <- e })
	defer unsub()

	// A bare engine (no DB) is enough: the emit path touches only the
	// evaluator + event bus, and the DB-backed step-record write is a
	// no-op for a nil engine db. The bus is wired exactly as app bootstrap
	// does it (SetEventBus before the runner is built).
	engine, err := memql.New(nil)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	engine.SetEventBus(bus)

	runner := automations.NewLogicRunner(engine, steps.NewRegistry(), nil)

	src := `logic emitOnly {
  body {
    emitDone := publishEvent({
      topic: "si.auto-joined",
      payload: { ok: "yes" }
    })
    return emitDone
  }
}`
	body := parseEmitLogicBody(t, src)

	if _, err := runner.RunLogic(context.Background(), "emitOnly", body, map[string]any{}); err != nil {
		t.Fatalf("RunLogic returned error (emit step likely aborted on a nil EventBus): %v", err)
	}

	select {
	case e := <-received:
		if e.Topic != "si.auto-joined" {
			t.Errorf("published topic = %q, want si.auto-joined", e.Topic)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("emit step did not publish to the engine bus -- EventBus not wired into the logic StepContext")
	}
}
