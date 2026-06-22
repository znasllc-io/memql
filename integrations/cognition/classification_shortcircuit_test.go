package cognition

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
)

// classifierProbeEngine wraps fakeEngine with an InvokeAIStructured
// override that counts calls and returns a canned classification, so
// tests can assert whether the LLM boundary was crossed.
type classifierProbeEngine struct {
	fakeEngine
	probeMu         sync.Mutex
	structuredCalls int
	response        string
}

func (e *classifierProbeEngine) InvokeAIStructured(
	ctx context.Context,
	templateId string,
	data map[string]any,
	schemaName string,
	schema json.RawMessage,
	strict bool,
) (string, error) {
	e.probeMu.Lock()
	defer e.probeMu.Unlock()
	e.structuredCalls++
	return e.response, nil
}

func (e *classifierProbeEngine) calls() int {
	e.probeMu.Lock()
	defer e.probeMu.Unlock()
	return e.structuredCalls
}

func newProbeClassifier(t *testing.T) (*messageClassifier, *classifierProbeEngine) {
	t.Helper()
	engine := &classifierProbeEngine{
		response: `{"intent":"question","carriesAction":true,"answersPriorAgentPrompt":false,` +
			`"addressedAgentName":"","addressedToRoom":false,"confidence":0.9,"reasoning":"llm"}`,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newMessageClassifier(logger, engine), engine
}

func TestShortCircuitClassificationPredicate(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		agentNames []string
		humanCount int
		isVoice    bool
		wantFire   bool
	}{
		{
			name: "one human one agent no mention fires",
			text: "hey", agentNames: []string{"Sofia"}, humanCount: 1, wantFire: true,
		},
		{
			name: "two agents does not fire",
			text: "hey", agentNames: []string{"Sofia", "Nova"}, humanCount: 1, wantFire: false,
		},
		{
			name: "two humans does not fire",
			text: "hey", agentNames: []string{"Sofia"}, humanCount: 2, wantFire: false,
		},
		{
			name: "zero humans does not fire",
			text: "hey", agentNames: []string{"Sofia"}, humanCount: 0, wantFire: false,
		},
		{
			name: "zero agents does not fire",
			text: "hey", agentNames: nil, humanCount: 1, wantFire: false,
		},
		{
			name: "voice does not fire",
			text: "hey", agentNames: []string{"Sofia"}, humanCount: 1, isVoice: true, wantFire: false,
		},
		{
			name: "at-mention of the single agent fires",
			text: "@Sofia please summarize this", agentNames: []string{"Sofia"}, humanCount: 1, wantFire: true,
		},
		{
			name: "at-mention case-insensitive fires",
			text: "@sofia are you there", agentNames: []string{"Sofia"}, humanCount: 1, wantFire: true,
		},
		{
			name: "at-mention first word of multiword agent name fires",
			text: "@Sofia can you help", agentNames: []string{"Sofia Reyes"}, humanCount: 1, wantFire: true,
		},
		{
			name: "at-mention of someone else does not fire",
			text: "@Bob can you help", agentNames: []string{"Sofia"}, humanCount: 1, wantFire: false,
		},
		{
			name: "mixed mentions one foreign does not fire",
			text: "@Sofia and @Bob look at this", agentNames: []string{"Sofia"}, humanCount: 1, wantFire: false,
		},
		{
			name: "email-like at-token conservatively does not fire",
			text: "send it to alice@example.com", agentNames: []string{"Sofia"}, humanCount: 1, wantFire: false,
		},
		{
			name: "bare at sign without token fires",
			text: "see you @ 5", agentNames: []string{"Sofia"}, humanCount: 1, wantFire: true,
		},
		{
			name: "empty agent name does not fire",
			text: "hey", agentNames: []string{"  "}, humanCount: 1, wantFire: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, fired := shortCircuitClassification(tc.text, tc.agentNames, tc.humanCount, tc.isVoice)
			if fired != tc.wantFire {
				t.Fatalf("shortCircuitClassification(%q, %v, %d, voice=%v) fired=%v, want %v",
					tc.text, tc.agentNames, tc.humanCount, tc.isVoice, fired, tc.wantFire)
			}
			if fired && result.AddressedAgentName == "" {
				t.Fatalf("fired short-circuit must stamp AddressedAgentName, got empty")
			}
		})
	}
}

func TestShortCircuitClassificationSynthesizedValues(t *testing.T) {
	result, fired := shortCircuitClassification("hey", []string{"Sofia"}, 1, false)
	if !fired {
		t.Fatal("expected short-circuit to fire for 1-human/1-agent text turn")
	}
	if result.AddressedAgentName != "Sofia" {
		t.Errorf("AddressedAgentName = %q, want %q", result.AddressedAgentName, "Sofia")
	}
	if result.Confidence != 1.0 {
		t.Errorf("Confidence = %v, want 1.0", result.Confidence)
	}
	if result.Reasoning != classificationShortCircuitReasoning {
		t.Errorf("Reasoning = %q, want %q", result.Reasoning, classificationShortCircuitReasoning)
	}
	if result.Intent != "unknown" || result.CarriesAction || result.AnswersPriorAgentPrompt || result.AddressedToRoom {
		t.Errorf("neutral fields drifted: intent=%q carriesAction=%v answersPriorAgentPrompt=%v addressedToRoom=%v",
			result.Intent, result.CarriesAction, result.AnswersPriorAgentPrompt, result.AddressedToRoom)
	}

	// The synthesized result must never satisfy either suppression
	// predicate in handleUtterance: both require an empty
	// AddressedAgentName, and the intent must not be an ack kind.
	if isConversationalAckIntent(result.Intent) {
		t.Errorf("synthesized intent %q is an ack intent; it could trigger suppression", result.Intent)
	}
}

func TestShortCircuitClassificationKnobDisables(t *testing.T) {
	t.Setenv(classificationShortCircuitEnvKey, "false")
	if _, fired := shortCircuitClassification("hey", []string{"Sofia"}, 1, false); fired {
		t.Fatal("short-circuit fired with MEMQL_CLASSIFICATION_SHORTCIRCUIT=false")
	}
	t.Setenv(classificationShortCircuitEnvKey, "0")
	if _, fired := shortCircuitClassification("hey", []string{"Sofia"}, 1, false); fired {
		t.Fatal("short-circuit fired with MEMQL_CLASSIFICATION_SHORTCIRCUIT=0")
	}
	t.Setenv(classificationShortCircuitEnvKey, "true")
	if _, fired := shortCircuitClassification("hey", []string{"Sofia"}, 1, false); !fired {
		t.Fatal("short-circuit did not fire with MEMQL_CLASSIFICATION_SHORTCIRCUIT=true")
	}
}

func TestClassifyWithShortCircuitSkipsLLM(t *testing.T) {
	mc, engine := newProbeClassifier(t)

	result, shortCircuited := mc.ClassifyWithShortCircuit(
		context.Background(), "hey", "", []string{"Sofia"}, 1, false)
	if !shortCircuited {
		t.Fatal("expected short-circuit for 1-human/1-agent text turn")
	}
	if got := engine.calls(); got != 0 {
		t.Fatalf("LLM provider called %d time(s) despite short-circuit; want 0", got)
	}
	if result.Reasoning != classificationShortCircuitReasoning {
		t.Errorf("Reasoning = %q, want short-circuit marker", result.Reasoning)
	}
}

func TestClassifyWithShortCircuitFallsThroughToLLM(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		roster     []string
		humanCount int
		isVoice    bool
	}{
		{name: "two agents", text: "hey", roster: []string{"Sofia", "Nova"}, humanCount: 1},
		{name: "two humans", text: "hey", roster: []string{"Sofia"}, humanCount: 2},
		{name: "voice", text: "hey", roster: []string{"Sofia"}, humanCount: 1, isVoice: true},
		{name: "foreign at-mention", text: "@Bob hi", roster: []string{"Sofia"}, humanCount: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc, engine := newProbeClassifier(t)
			result, shortCircuited := mc.ClassifyWithShortCircuit(
				context.Background(), tc.text, "", tc.roster, tc.humanCount, tc.isVoice)
			if shortCircuited {
				t.Fatal("short-circuit fired; expected fall-through to LLM classification")
			}
			if got := engine.calls(); got != 1 {
				t.Fatalf("LLM provider called %d time(s); want exactly 1", got)
			}
			if result.Intent != "question" {
				t.Errorf("Intent = %q, want %q from the LLM fixture", result.Intent, "question")
			}
		})
	}
}
