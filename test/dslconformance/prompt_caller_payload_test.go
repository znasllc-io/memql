package dslconformance

// memql#3616: the cognitionPrediction prompt declared only `transcript` and
// `agents` while component/polyphon's predictive analyzer passed seven more
// keys. The schema is additionalProperties:false and is validated BEFORE the
// template renders, so every call failed -- and predictive.go swallows the
// error into patternBasedPrediction(), which is why the entire predictive-
// cognition LLM path was dead without a single error surfacing.
//
// The test drives the REAL analyzer with a capturing invokeAI, then validates
// the captured payload against the REAL prompt schema loaded from the DSL
// tree. It lives here rather than in component/polyphon because that module
// deliberately does not depend on component/memql.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"text/template"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/polyphon"
)

func loadPrompt(t *testing.T, name string) *memql.PromptTemplate {
	t.Helper()
	registry := memql.NewPromptRegistry()
	if _, err := memql.LoadUnifiedPrompts(nil, registry, template.New("partials")); err != nil {
		t.Fatalf("LoadUnifiedPrompts: %v", err)
	}
	prompt, ok := registry.Get(name)
	if !ok || prompt == nil {
		t.Fatalf("prompt %q not registered", name)
	}
	return prompt
}

// normalizeForSchema mirrors aiRuntime.normalizeAIData: the runtime
// JSON-round-trips the caller's payload before ValidateData, because the
// schema validator only recognises encoding/json-native containers.
func normalizeForSchema(t *testing.T, data map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return out
}

// capturePredictionPayload runs the analyzer far enough to build its prompt
// data and returns exactly that map. The stub invokeAI aborts the call right
// after capture, so no provider is needed.
func capturePredictionPayload(t *testing.T) map[string]any {
	t.Helper()

	var captured map[string]any
	analyzer := polyphon.NewModelPredictiveAnalyzer(
		func(_ context.Context, templateId string, data map[string]any) (any, error) {
			if templateId != "cognitionPrediction" {
				t.Errorf("analyzer invoked prompt %q, want cognitionPrediction", templateId)
			}
			captured = data
			return nil, errors.New("stop after capture")
		},
		nil, // no embedFunc -- the vector-phase keys are covered separately below
	)

	session := polyphon.NewSession("v1:cognition:space:test")
	session.AddTranscript(polyphon.TranscriptEntry{
		SpeakerId: "u1", SpeakerName: "Ada", SpeakerType: "human",
		Text: "what should we do next?", Timestamp: time.Now().UTC(),
	})
	session.AddTranscript(polyphon.TranscriptEntry{
		SpeakerId: "a1", SpeakerName: "Sofia", SpeakerType: "agent",
		Text: "let me check", Timestamp: time.Now().UTC(),
	})
	// The thread-holder branch only fires when an agent has been addressed.
	session.LastAddressedAgentId = "a1"
	session.LastAddressedAt = time.Now().UTC()

	candidates := []polyphon.AgentCandidate{{Name: "Sofia", Role: "assistant", Domains: []string{"general"}}}

	if _, err := analyzer.Analyze(context.Background(), session, candidates); err != nil {
		t.Fatalf("Analyze returned an error: %v", err)
	}
	if captured == nil {
		t.Fatal("analyzer never invoked the prompt -- the payload was not captured")
	}
	return captured
}

// TestCognitionPredictionAcceptsPolyphonPayload is the acceptance: the exact
// map the predictive analyzer builds validates clean against the prompt.
func TestCognitionPredictionAcceptsPolyphonPayload(t *testing.T) {
	prompt := loadPrompt(t, "cognitionPrediction")
	payload := capturePredictionPayload(t)

	if err := prompt.ValidateData(normalizeForSchema(t, payload)); err != nil {
		t.Fatalf("the payload component/polyphon builds must satisfy the cognitionPrediction schema "+
			"(a failure here is silently swallowed into patternBasedPrediction at runtime): %v", err)
	}
}

// TestCognitionPredictionAcceptsVectorPhaseKeys covers the two keys the
// analyzer only adds on a high-confidence embedding match, which the
// capture above cannot reach without a live embedding provider. They are
// exactly as fatal as the rest when undeclared.
func TestCognitionPredictionAcceptsVectorPhaseKeys(t *testing.T) {
	prompt := loadPrompt(t, "cognitionPrediction")
	payload := capturePredictionPayload(t)
	payload["detectedPhase"] = "task"
	payload["phaseConfidence"] = float32(0.93)

	if err := prompt.ValidateData(normalizeForSchema(t, payload)); err != nil {
		t.Fatalf("the vector-phase keys the analyzer adds on a confident match must validate: %v", err)
	}
}

// TestCognitionPredictionPayloadKeysAreDeclared states the rule directly,
// so the failure names the offending key rather than a schema URL.
func TestCognitionPredictionPayloadKeysAreDeclared(t *testing.T) {
	prompt := loadPrompt(t, "cognitionPrediction")

	declared := map[string]bool{}
	for _, arg := range prompt.Arguments() {
		name, _ := arg["name"].(string)
		declared[name] = true
	}
	if len(declared) == 0 {
		t.Fatal("prompt declares no arguments -- the check would pass vacuously")
	}

	payload := capturePredictionPayload(t)
	payload["detectedPhase"] = "task"
	payload["phaseConfidence"] = float32(0.93)

	for key := range payload {
		if !declared[key] {
			t.Errorf("payload key %q is not declared by cognitionPrediction; "+
				"additionalProperties:false rejects it before the template renders, so every call fails", key)
		}
	}
}
