package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// scriptedAnalyzeEngine returns a queued sequence of agentFactoryAnalyze
// answers and records the priorError each call was given, so a test can assert
// BOTH that the retry happened and that the model was told why.
type scriptedAnalyzeEngine struct {
	memql.IntegrationEngineAccess
	answers    []string
	invokeErr  error
	priorSeen  []string
	invokeCall int
}

func (e *scriptedAnalyzeEngine) Execute(_ context.Context, _ string) (*memql.ExecuteResult, error) {
	return memql.NewResultWithOutput(nil), nil
}

func (e *scriptedAnalyzeEngine) InvokeAIStructured(
	_ context.Context, _ string, data map[string]any, _ string, _ json.RawMessage, _ bool,
) (string, error) {
	prior, _ := data["priorError"].(string)
	e.priorSeen = append(e.priorSeen, prior)
	if e.invokeErr != nil {
		return "", e.invokeErr
	}
	i := e.invokeCall
	e.invokeCall++
	if i < len(e.answers) {
		return e.answers[i], nil
	}
	return e.answers[len(e.answers)-1], nil
}

func testRoles() []roleSnapshot {
	return []roleSnapshot{
		{Slug: "creative-companion", Name: "Creative Companion"},
		{Slug: "fiction-writer", Name: "Fiction Writer"},
	}
}

func decisionJSON(action, roleSlug, targetId string) string {
	return fmt.Sprintf(`{"action":%q,"roleSlug":%q,"targetAgentId":%q,"skillIds":[],"reasoning":"r"}`,
		action, roleSlug, targetId)
}

// TestInvalidRoleSlugIsRetriedWithTheReason is the memql#4690 regression: one
// bad guess used to fail the whole goal. Now the model is re-asked, and told
// what was wrong.
func TestInvalidRoleSlugIsRetriedWithTheReason(t *testing.T) {
	eng := &scriptedAnalyzeEngine{answers: []string{
		decisionJSON("create", "content-creator", ""), // not in catalog -- the live failure
		decisionJSON("create", "fiction-writer", ""),  // corrected
	}}
	i := New(memql.NewAgentRegistry(), eng)

	decision, err := i.decideForGoal(context.Background(), "write me a story", nil, testRoles(), nil)
	if err != nil {
		t.Fatalf("decideForGoal failed after a correctable first answer: %v", err)
	}
	if decision.RoleSlug != "fiction-writer" {
		t.Errorf("roleSlug = %q, want the corrected %q", decision.RoleSlug, "fiction-writer")
	}
	if len(eng.priorSeen) != 2 {
		t.Fatalf("agentFactoryAnalyze called %d times, want 2 (one bad answer, one retry)", len(eng.priorSeen))
	}
	if eng.priorSeen[0] != "" {
		t.Errorf("first attempt was given priorError %q, want empty", eng.priorSeen[0])
	}
	// The feedback must NAME the bad value and LIST the real options, or the
	// retry is just a second roll of the same dice.
	if !strings.Contains(eng.priorSeen[1], "content-creator") {
		t.Errorf("retry feedback does not name the rejected slug: %q", eng.priorSeen[1])
	}
	if !strings.Contains(eng.priorSeen[1], "fiction-writer") ||
		!strings.Contains(eng.priorSeen[1], "creative-companion") {
		t.Errorf("retry feedback does not list the catalog slugs: %q", eng.priorSeen[1])
	}
}

// TestRetriesAreBounded: a model that never corrects must not loop. The cap is
// a cost control, so this asserts the CALL COUNT, not just that it terminates.
func TestRetriesAreBounded(t *testing.T) {
	eng := &scriptedAnalyzeEngine{answers: []string{decisionJSON("create", "nope-not-real", "")}}
	i := New(memql.NewAgentRegistry(), eng)

	_, err := i.decideForGoal(context.Background(), "goal", nil, testRoles(), nil)
	if err == nil {
		t.Fatal("expected failure after the attempt cap, got nil")
	}
	if len(eng.priorSeen) != maxFactoryAnalyzeAttempts {
		t.Errorf("made %d attempts, want exactly %d", len(eng.priorSeen), maxFactoryAnalyzeAttempts)
	}
	if !strings.Contains(err.Error(), "nope-not-real") {
		t.Errorf("give-up error should carry the last rejection, got: %v", err)
	}
}

// TestProviderFailureIsNotRetried is the other half of the design. Re-asking a
// provider that is down cannot help, and each attempt spends the plan's budget.
func TestProviderFailureIsNotRetried(t *testing.T) {
	sentinel := errors.New(`provider "streamClaudeSonnet" not available`)
	eng := &scriptedAnalyzeEngine{invokeErr: sentinel}
	i := New(memql.NewAgentRegistry(), eng)

	_, err := i.decideForGoal(context.Background(), "goal", nil, testRoles(), nil)
	if err == nil {
		t.Fatal("expected the provider error to propagate")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("provider error should propagate unwrapped-through, got: %v", err)
	}
	if len(eng.priorSeen) != 1 {
		t.Errorf("provider outage caused %d LLM attempts, want exactly 1 -- retrying a down provider "+
			"burns the plan's token budget to reach the same wall", len(eng.priorSeen))
	}
}

// TestUnknownActionAndMissingTargetAreCorrectable pins the other two rejection
// classes that used to be one-shot terminal.
func TestUnknownActionAndMissingTargetAreCorrectable(t *testing.T) {
	for name, bad := range map[string]string{
		"unknown action":       decisionJSON("invent", "fiction-writer", ""),
		"match with no target": decisionJSON("match", "", ""),
		"target not owned":     decisionJSON("match", "", "v1:agents:agent:someone-elses"),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateFactoryDecision(parseDecision(t, bad), nil, testRoles()); err == nil {
				t.Fatal("expected a rejection")
			} else if !correctable(err) {
				t.Errorf("rejection should be correctable so the model gets a retry, got %T", err)
			}
		})
	}
}

func parseDecision(t *testing.T, raw string) factoryDecision {
	t.Helper()
	var d factoryDecision
	if err := jsonUnmarshalForTest(raw, &d); err != nil {
		t.Fatalf("fixture is not a decision: %v", err)
	}
	return d
}

func jsonUnmarshalForTest(raw string, v any) error { return json.Unmarshal([]byte(raw), v) }
