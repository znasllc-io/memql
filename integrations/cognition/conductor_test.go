package cognition

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/polyphon"
)

// =============================================================================
// Conductor test harness
// =============================================================================
//
// These tests cover the conductor's POST-LLM processing -- validation,
// id resolution, duplicate filtering, and the directive translation
// layer. The actual LLM call is not mocked here; the value of these
// fixtures is in catching regressions when the structural code paths
// change. (A live-LLM integration suite is a separate concern; the
// fixtures here protect the deterministic plumbing.)
//
// Each fixture is a (raw plan JSON, candidate roster) pair plus an
// expected post-validation shape. When validation logic changes, run
// `go test ./integrations/cognition/...` to see which fixtures move.

// -----------------------------------------------------------------------------
// Fixture helpers
// -----------------------------------------------------------------------------

// candidate builds a polyphon.AgentCandidate with the participant id +
// name + optional role. Role is the only optional field; tests can
// extend if richer fixtures are needed.
func candidate(participantId, agentTemplateId, name, role string) polyphon.AgentCandidate {
	return polyphon.AgentCandidate{
		ID:            agentTemplateId,
		ParticipantId: participantId,
		Name:          name,
		Role:          role,
	}
}

// rosterFor builds a small, common-case candidate roster used across
// fixtures. Sofia is the assistant; Atlas is technical;
// Wren is customer-facing.
func rosterFor() []polyphon.AgentCandidate {
	return []polyphon.AgentCandidate{
		candidate("p-sofia", "agent-sofia", "Sofia", "assistant"),
		candidate("p-atlas", "agent-atlas", "Atlas", "engineering_technology"),
		candidate("p-wren", "agent-wren", "Wren", "customer_success"),
	}
}

// validatePlanJSON simulates the plumbing in consultConductor that
// runs after the LLM returns: parse JSON -> validate ids -> clean
// duplicates. Returns the cleaned plan or an error matching the
// production behavior.
//
// Used by every fixture so the same code path is exercised that runs
// in production. If consultConductor's post-parse logic changes, these
// tests will pick it up.
func validatePlanJSON(t *testing.T, raw string, candidates []polyphon.AgentCandidate) ConductorPlan {
	t.Helper()
	var plan ConductorPlan
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatalf("parse: %v\nraw: %s", err, raw)
	}
	c := &CognitionIntegration{}
	lookup := buildCandidateLookup(candidates)
	names := candidateNamesForLookup(candidates)
	if resolved, ok := lookup.resolveId(plan.Primary.AgentId, plan.Primary.Instruction, "primary"); ok {
		plan.Primary.AgentId = resolved
	} else if plan.Primary.AgentId != "" {
		plan.Primary.AgentId = ""
	}
	if plan.Primary.AgentId != "" && isImpersonationInstruction(plan.Primary.Instruction, plan.Primary.AgentId, names) {
		plan.Primary.AgentId = ""
	}
	planContext := plan.Reason + " " + plan.GlobalGuidance
	plan.Sequence = c.validateAgentPlanList(plan.Sequence, lookup, plan.Primary.AgentId, "sequence", names, planContext)
	plan.ChimeIns = c.validateAgentPlanList(plan.ChimeIns, lookup, plan.Primary.AgentId, "chimein", names, planContext)
	return plan
}

// -----------------------------------------------------------------------------
// Candidate lookup tests
// -----------------------------------------------------------------------------

func TestCandidateLookup_ResolveExactParticipantId(t *testing.T) {
	lookup := buildCandidateLookup(rosterFor())
	got, ok := lookup.resolveId("p-sofia", "", "primary")
	if !ok || got != "p-sofia" {
		t.Fatalf("exact participant id should resolve to itself, got %q ok=%v", got, ok)
	}
}

func TestCandidateLookup_ResolveAgentTemplateId(t *testing.T) {
	lookup := buildCandidateLookup(rosterFor())
	// LLM emitted the agent template id instead of participant id
	got, ok := lookup.resolveId("agent-sofia", "", "primary")
	if !ok || got != "p-sofia" {
		t.Fatalf("agent template id should remap to participant id, got %q ok=%v", got, ok)
	}
}

func TestCandidateLookup_ResolveByName(t *testing.T) {
	lookup := buildCandidateLookup(rosterFor())
	// LLM emitted just the agent's name
	got, ok := lookup.resolveId("Sofia", "", "primary")
	if !ok || got != "p-sofia" {
		t.Fatalf("name should resolve to participant id, got %q ok=%v", got, ok)
	}
}

func TestCandidateLookup_ResolveCaseInsensitive(t *testing.T) {
	lookup := buildCandidateLookup(rosterFor())
	got, ok := lookup.resolveId("SOFIA", "", "primary")
	if !ok || got != "p-sofia" {
		t.Fatalf("name match should be case-insensitive, got %q ok=%v", got, ok)
	}
	got, ok = lookup.resolveId("sofia", "", "primary")
	if !ok || got != "p-sofia" {
		t.Fatalf("name match should be case-insensitive, got %q ok=%v", got, ok)
	}
}

func TestCandidateLookup_ResolveFromInstruction(t *testing.T) {
	lookup := buildCandidateLookup(rosterFor())
	// LLM left the id blank but mentioned the agent in the instruction
	got, ok := lookup.resolveId("", "Atlas should answer the OAuth question", "primary")
	if !ok || got != "p-atlas" {
		t.Fatalf("instruction-text fallback should match, got %q ok=%v", got, ok)
	}
}

func TestCandidateLookup_UnresolvedReturnsFalse(t *testing.T) {
	lookup := buildCandidateLookup(rosterFor())
	got, ok := lookup.resolveId("nonexistent-agent", "no agent name in here", "primary")
	if ok {
		t.Fatalf("unknown id with no name fallback should fail, got %q", got)
	}
}

// TestCandidateLookup_SubstringFallbackPrefixStripped covers the
// real-world case where the LLM strips a known prefix from the
// participant id. Reproduces the "primary id unresolved" bug observed
// in production where the conductor emitted
// "ga-...-default:v1:cognition:space:..." instead of the full
// "si-default:v1:agents:agent:ga-...-default:v1:cognition:space:..."
// participant id.
func TestCandidateLookup_SubstringFallbackPrefixStripped(t *testing.T) {
	candidates := []polyphon.AgentCandidate{
		{
			ID:            "agent-sofia-template",
			ParticipantId: "si-default:v1:agents:agent:ga-12345abcdef-default:v1:cognition:space:xyz",
			Name:          "Sofia",
		},
	}
	lookup := buildCandidateLookup(candidates)
	// LLM emitted a stripped form
	stripped := "ga-12345abcdef-default:v1:cognition:space:xyz"
	got, ok := lookup.resolveId(stripped, "", "primary")
	if !ok {
		t.Fatalf("substring fallback should resolve stripped form, got ok=%v", ok)
	}
	if got != "si-default:v1:agents:agent:ga-12345abcdef-default:v1:cognition:space:xyz" {
		t.Errorf("got %q want full participant id", got)
	}
}

func TestCandidateLookup_SubstringFallbackAmbiguousFails(t *testing.T) {
	// Two participants whose ids both contain "ga-shared" -> ambiguous
	candidates := []polyphon.AgentCandidate{
		{ID: "t1", ParticipantId: "si-default:v1:agents:agent:ga-shared-suffix-1", Name: "AlphaUniqueName"},
		{ID: "t2", ParticipantId: "si-default:v1:agents:agent:ga-shared-suffix-2", Name: "BetaUniqueName"},
	}
	lookup := buildCandidateLookup(candidates)
	got, ok := lookup.resolveId("ga-shared", "", "primary")
	if ok {
		t.Errorf("ambiguous substring match should fail, got %q", got)
	}
}

func TestCandidateLookup_ResolveFromReasonField(t *testing.T) {
	// When id is junk + instruction is generic, but the reason names
	// the agent, we should still resolve.
	lookup := buildCandidateLookup(rosterFor())
	got, ok := lookup.resolveId("garbage-id-xyz", "Sofia is the best fit for this UI question", "primary")
	if !ok {
		t.Fatalf("name in context text should resolve, got ok=%v", ok)
	}
	if got != "p-sofia" {
		t.Errorf("got %q want p-sofia", got)
	}
}

func TestCandidateLookup_SubstringTooShortRejected(t *testing.T) {
	// A very short id substring shouldn't match -- avoids false
	// positives on tiny fragments.
	lookup := buildCandidateLookup(rosterFor())
	got, ok := lookup.resolveId("ga", "", "primary")
	if ok {
		t.Errorf("tiny substring should not match, got %q", got)
	}
}

func TestCandidateLookup_EmptyCandidates(t *testing.T) {
	lookup := buildCandidateLookup(nil)
	got, ok := lookup.resolveId("p-sofia", "", "primary")
	if ok {
		t.Fatalf("empty roster should never resolve, got %q", got)
	}
}

// -----------------------------------------------------------------------------
// Plan validation fixtures
// -----------------------------------------------------------------------------

func TestPlanValidation_ColdOpenGreeting(t *testing.T) {
	raw := `{
		"phase": "cold_open",
		"temperature": "casual",
		"userIntent": "open the room with a hello",
		"globalGuidance": "Just opened. Keep it human.",
		"primary": {"agentId": "p-sofia", "instruction": "Greet warmly", "brevity": "short"},
		"sequence": [],
		"chimeIns": [
			{"agentId": "p-atlas", "instruction": "Just say hi back", "brevity": "short"},
			{"agentId": "p-wren", "instruction": "Just say hi back", "brevity": "short"}
		],
		"reason": "cold-open greeting"
	}`
	plan := validatePlanJSON(t, raw, rosterFor())
	if plan.Phase != "cold_open" {
		t.Errorf("phase: got %q want cold_open", plan.Phase)
	}
	if plan.Primary.AgentId != "p-sofia" {
		t.Errorf("primary: got %q want p-sofia", plan.Primary.AgentId)
	}
	if len(plan.ChimeIns) != 2 {
		t.Errorf("chime-ins: got %d want 2", len(plan.ChimeIns))
	}
	if plan.HasSequence() {
		t.Errorf("HasSequence: got true want false (cold-open uses chime-ins)")
	}
}

func TestPlanValidation_FanOutWithOrder(t *testing.T) {
	// "joke from each of you, start with Ember" -- ordered sequence
	raw := `{
		"phase": "warming",
		"temperature": "casual",
		"userIntent": "get a joke from each agent in order",
		"globalGuidance": "Three jokes, one per agent.",
		"primary": {"agentId": "p-wren", "instruction": "Tell a short joke", "brevity": "short"},
		"sequence": [
			{"agentId": "p-atlas", "instruction": "Tell your own joke", "brevity": "short"},
			{"agentId": "p-sofia", "instruction": "Tell your own joke", "brevity": "short"}
		],
		"chimeIns": [],
		"reason": "explicit fan-out with order"
	}`
	plan := validatePlanJSON(t, raw, rosterFor())
	if !plan.HasSequence() {
		t.Errorf("HasSequence should be true")
	}
	if len(plan.Sequence) != 2 {
		t.Errorf("sequence: got %d want 2", len(plan.Sequence))
	}
	if plan.Sequence[0].AgentId != "p-atlas" {
		t.Errorf("sequence[0]: got %q want p-atlas (order matters)", plan.Sequence[0].AgentId)
	}
	if plan.Sequence[1].AgentId != "p-sofia" {
		t.Errorf("sequence[1]: got %q want p-sofia", plan.Sequence[1].AgentId)
	}
}

func TestPlanValidation_FocusedQuestionStaysSolo(t *testing.T) {
	raw := `{
		"phase": "working",
		"temperature": "focused",
		"userIntent": "debug OAuth flow",
		"globalGuidance": "Focused tech question; answer directly.",
		"primary": {"agentId": "p-atlas", "instruction": "Answer the OAuth question", "brevity": "normal"},
		"sequence": [],
		"chimeIns": [],
		"reason": "single specialist with real fit"
	}`
	plan := validatePlanJSON(t, raw, rosterFor())
	if plan.PrimaryAgentId() != "p-atlas" {
		t.Errorf("primary: got %q want p-atlas", plan.PrimaryAgentId())
	}
	if len(plan.ChimeIns) != 0 || plan.HasSequence() {
		t.Errorf("focused question should leave sequence + chime-ins empty")
	}
}

func TestPlanValidation_DropsUnresolvableId(t *testing.T) {
	// Conductor invented an agent id that doesn't exist
	raw := `{
		"phase": "warming",
		"temperature": "casual",
		"userIntent": "say hi",
		"globalGuidance": "casual greeting",
		"primary": {"agentId": "p-sofia", "instruction": "Greet", "brevity": "short"},
		"sequence": [],
		"chimeIns": [
			{"agentId": "p-atlas", "instruction": "Hi back", "brevity": "short"},
			{"agentId": "p-ghost-agent", "instruction": "Should be dropped", "brevity": "short"}
		],
		"reason": "test"
	}`
	plan := validatePlanJSON(t, raw, rosterFor())
	if len(plan.ChimeIns) != 1 {
		t.Errorf("chime-ins: got %d want 1 (ghost should be dropped)", len(plan.ChimeIns))
	}
	if plan.ChimeIns[0].AgentId != "p-atlas" {
		t.Errorf("chime-ins[0]: got %q want p-atlas", plan.ChimeIns[0].AgentId)
	}
}

func TestPlanValidation_RemapsTemplateIdToParticipantId(t *testing.T) {
	// LLM emitted agent-template ids -- should remap to participant ids
	raw := `{
		"phase": "warming",
		"temperature": "casual",
		"userIntent": "test",
		"globalGuidance": "test",
		"primary": {"agentId": "agent-sofia", "instruction": "Greet", "brevity": "short"},
		"sequence": [],
		"chimeIns": [
			{"agentId": "agent-atlas", "instruction": "Hi", "brevity": "short"}
		],
		"reason": "test"
	}`
	plan := validatePlanJSON(t, raw, rosterFor())
	if plan.Primary.AgentId != "p-sofia" {
		t.Errorf("primary remap: got %q want p-sofia", plan.Primary.AgentId)
	}
	if len(plan.ChimeIns) != 1 || plan.ChimeIns[0].AgentId != "p-atlas" {
		t.Errorf("chime-in remap: got %+v want p-atlas", plan.ChimeIns)
	}
}

func TestPlanValidation_DropsDuplicatePrimaryFromChimeIns(t *testing.T) {
	// Conductor mistakenly puts primary in chime-ins too
	raw := `{
		"phase": "cold_open",
		"temperature": "casual",
		"userIntent": "test",
		"globalGuidance": "test",
		"primary": {"agentId": "p-sofia", "instruction": "Greet", "brevity": "short"},
		"sequence": [],
		"chimeIns": [
			{"agentId": "p-sofia", "instruction": "Should be dropped", "brevity": "short"},
			{"agentId": "p-atlas", "instruction": "Hi", "brevity": "short"}
		],
		"reason": "test"
	}`
	plan := validatePlanJSON(t, raw, rosterFor())
	if len(plan.ChimeIns) != 1 {
		t.Errorf("chime-ins: got %d want 1 (duplicate should be dropped)", len(plan.ChimeIns))
	}
	if plan.ChimeIns[0].AgentId != "p-atlas" {
		t.Errorf("remaining chime-in: got %q want p-atlas", plan.ChimeIns[0].AgentId)
	}
}

func TestPlanValidation_DropsDuplicateWithinChimeIns(t *testing.T) {
	// Same agent in chime-ins twice
	raw := `{
		"phase": "warming",
		"temperature": "casual",
		"userIntent": "test",
		"globalGuidance": "test",
		"primary": {"agentId": "p-sofia", "instruction": "Greet", "brevity": "short"},
		"sequence": [],
		"chimeIns": [
			{"agentId": "p-atlas", "instruction": "First hi", "brevity": "short"},
			{"agentId": "p-atlas", "instruction": "Duplicate", "brevity": "short"}
		],
		"reason": "test"
	}`
	plan := validatePlanJSON(t, raw, rosterFor())
	if len(plan.ChimeIns) != 1 {
		t.Errorf("chime-ins: got %d want 1 (duplicate should be dropped)", len(plan.ChimeIns))
	}
	if plan.ChimeIns[0].Instruction != "First hi" {
		t.Errorf("kept entry: got %q want first hi (first wins)", plan.ChimeIns[0].Instruction)
	}
}

func TestPlanValidation_AcknowledgePriorRoundtrips(t *testing.T) {
	// Explicit handoff with acknowledgment -- the acknowledgePrior
	// field should pass through validation and land on the directive.
	raw := `{
		"phase": "working",
		"temperature": "focused",
		"userIntent": "Atlas takes over for API integration",
		"globalGuidance": "Explicit handoff; brief nod is appropriate.",
		"primary": {
			"agentId": "p-atlas",
			"instruction": "Walk through the API integration steps",
			"acknowledgePrior": "Quick nod to Sofia for the handoff",
			"brevity": "normal"
		},
		"sequence": [],
		"chimeIns": [],
		"reason": "explicit handoff"
	}`
	plan := validatePlanJSON(t, raw, rosterFor())
	if got := plan.Primary.AcknowledgePrior; got != "Quick nod to Sofia for the handoff" {
		t.Errorf("acknowledgePrior round-trip: got %q", got)
	}
	d := directiveFromConductorAgentPlan(&plan, &plan.Primary, "primary")
	if d == nil {
		t.Fatalf("directive should be non-nil")
	}
	if !strings.Contains(d.AcknowledgePrior, "Sofia") {
		t.Errorf("directive AcknowledgePrior: got %q", d.AcknowledgePrior)
	}
}

func TestPlanValidation_FrustrationRecalibrates(t *testing.T) {
	// User pushed back; plan should be tense, primary only, no peers
	raw := `{
		"phase": "frustration",
		"temperature": "tense",
		"userIntent": "stop the agenda framing, address actual concern",
		"globalGuidance": "User pushed back. Drop consultant tone.",
		"primary": {"agentId": "p-sofia", "instruction": "Acknowledge briefly, ask what they want", "brevity": "short"},
		"sequence": [],
		"chimeIns": [],
		"reason": "frustration recalibration"
	}`
	plan := validatePlanJSON(t, raw, rosterFor())
	if plan.Phase != "frustration" || plan.Temperature != "tense" {
		t.Errorf("phase/temp: got %s/%s want frustration/tense", plan.Phase, plan.Temperature)
	}
	if len(plan.ChimeIns) != 0 || plan.HasSequence() {
		t.Errorf("frustration plan should not pile on")
	}
}

func TestPlanValidation_NameOnlyPlanResolves(t *testing.T) {
	// Conductor used names instead of ids in EVERY field
	raw := `{
		"phase": "warming",
		"temperature": "casual",
		"userIntent": "test",
		"globalGuidance": "test",
		"primary": {"agentId": "Sofia", "instruction": "Greet", "brevity": "short"},
		"sequence": [
			{"agentId": "Atlas", "instruction": "Tell joke", "brevity": "short"}
		],
		"chimeIns": [
			{"agentId": "Wren", "instruction": "Hi back", "brevity": "short"}
		],
		"reason": "name-only"
	}`
	plan := validatePlanJSON(t, raw, rosterFor())
	if plan.Primary.AgentId != "p-sofia" {
		t.Errorf("primary name resolve: got %q want p-sofia", plan.Primary.AgentId)
	}
	if len(plan.Sequence) != 1 || plan.Sequence[0].AgentId != "p-atlas" {
		t.Errorf("sequence name resolve: got %+v", plan.Sequence)
	}
	if len(plan.ChimeIns) != 1 || plan.ChimeIns[0].AgentId != "p-wren" {
		t.Errorf("chime-in name resolve: got %+v", plan.ChimeIns)
	}
}

// -----------------------------------------------------------------------------
// Plan helper tests
// -----------------------------------------------------------------------------

func TestPlanHelper_HasSequence(t *testing.T) {
	cases := []struct {
		name    string
		plan    *ConductorPlan
		wantHas bool
	}{
		{"nil", nil, false},
		{"empty", &ConductorPlan{}, false},
		{"with sequence", &ConductorPlan{Sequence: []ConductorAgentPlan{{AgentId: "x"}}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.plan.HasSequence(); got != tc.wantHas {
				t.Errorf("HasSequence: got %v want %v", got, tc.wantHas)
			}
		})
	}
}

func TestPlanHelper_PlanForAgentSpansBuckets(t *testing.T) {
	plan := &ConductorPlan{
		Primary:  ConductorAgentPlan{AgentId: "p-sofia", Instruction: "primary"},
		Sequence: []ConductorAgentPlan{{AgentId: "p-atlas", Instruction: "seq"}},
		ChimeIns: []ConductorAgentPlan{{AgentId: "p-wren", Instruction: "chime"}},
	}
	cases := []struct {
		agentId string
		want    string
	}{
		{"p-sofia", "primary"},
		{"p-atlas", "seq"},
		{"p-wren", "chime"},
		{"p-nope", ""},
	}
	for _, tc := range cases {
		t.Run(tc.agentId, func(t *testing.T) {
			got := plan.PlanForAgent(tc.agentId)
			if tc.want == "" {
				if got != nil {
					t.Errorf("expected nil for %q, got %+v", tc.agentId, got)
				}
				return
			}
			if got == nil || got.Instruction != tc.want {
				t.Errorf("got %+v want instruction=%q", got, tc.want)
			}
		})
	}
}

func TestPlanHelper_PrimaryAgentIdTrimmed(t *testing.T) {
	plan := &ConductorPlan{Primary: ConductorAgentPlan{AgentId: "  p-sofia  "}}
	if got := plan.PrimaryAgentId(); got != "p-sofia" {
		t.Errorf("got %q want p-sofia (trimmed)", got)
	}
}

// -----------------------------------------------------------------------------
// Directive translation tests
// -----------------------------------------------------------------------------

func TestDirective_PrimaryParticipationKeepsPrimaryMode(t *testing.T) {
	plan := &ConductorPlan{
		Phase:          "working",
		Temperature:    "focused",
		UserIntent:     "answer",
		GlobalGuidance: "answer directly",
		Primary:        ConductorAgentPlan{AgentId: "p-sofia", Instruction: "Answer", Brevity: "normal"},
	}
	d := directiveFromConductorAgentPlan(plan, &plan.Primary, "primary")
	if d.Mode != DirectivePrimary {
		t.Errorf("Mode: got %q want primary", d.Mode)
	}
	if d.SkipSelfIntro {
		t.Errorf("primary should NOT skip self-intro by default")
	}
}

func TestDirective_SequenceParticipationKeepsPrimaryMode(t *testing.T) {
	// Sequence agents are independent solo turns, NOT chime-ins.
	plan := &ConductorPlan{
		Sequence: []ConductorAgentPlan{{AgentId: "p-atlas", Instruction: "Joke", Brevity: "short"}},
	}
	d := directiveFromConductorAgentPlan(plan, &plan.Sequence[0], "sequence")
	if d.Mode != DirectivePrimary {
		t.Errorf("sequence Mode: got %q want primary (sequence != chime-in)", d.Mode)
	}
}

func TestDirective_ChimeInParticipationGetsChimeInMode(t *testing.T) {
	plan := &ConductorPlan{
		ChimeIns: []ConductorAgentPlan{{AgentId: "p-atlas", Instruction: "Hi back", Brevity: "short"}},
	}
	d := directiveFromConductorAgentPlan(plan, &plan.ChimeIns[0], "chimein")
	if d.Mode != DirectiveChimeIn {
		t.Errorf("Mode: got %q want chimein", d.Mode)
	}
	if !d.SkipSelfIntro {
		t.Errorf("chime-ins should skip self-intro")
	}
}

func TestDirective_AlwaysSkipsRoomAnnounce(t *testing.T) {
	plan := &ConductorPlan{}
	for _, p := range []string{"primary", "sequence", "chimein"} {
		ap := &ConductorAgentPlan{AgentId: "x", Instruction: "y"}
		d := directiveFromConductorAgentPlan(plan, ap, p)
		if !d.SkipRoomAnnounce {
			t.Errorf("%s: SkipRoomAnnounce should always be true", p)
		}
	}
}

func TestDirective_BrevityMapping(t *testing.T) {
	cases := map[string]Brevity{
		"short":    BrevityShort,
		"normal":   BrevityNormal,
		"detailed": BrevityDetailed,
		"":         BrevityNormal, // default
		"weird":    BrevityNormal, // unrecognized -> default
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			plan := &ConductorPlan{Primary: ConductorAgentPlan{AgentId: "x", Instruction: "y", Brevity: input}}
			d := directiveFromConductorAgentPlan(plan, &plan.Primary, "primary")
			if d.Brevity != want {
				t.Errorf("brevity %q: got %q want %q", input, d.Brevity, want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Hint round-trip tests (encode/decode through the AgentForwarder Hints map)
// -----------------------------------------------------------------------------

func TestDirective_HintRoundtrip(t *testing.T) {
	original := &AgentParticipationDirective{
		Mode:             DirectivePrimary,
		Brevity:          BrevityShort,
		Instruction:      "Greet warmly, ask what they want.",
		AcknowledgePrior: "Quick nod to Sofia",
		GlobalGuidance:   "Cold-open; keep it human",
		Phase:            "cold_open",
		Temperature:      "casual",
		UserIntent:       "open the room",
		Reason:           "cold-open greeting",
		SkipRoomAnnounce: true,
	}
	hints := make(map[string]string)
	EncodeDirectiveIntoHints(original, hints)
	decoded := DecodeDirectiveFromHints(hints)
	if decoded == nil {
		t.Fatalf("decoded should be non-nil")
	}
	if decoded.Instruction != original.Instruction {
		t.Errorf("Instruction: got %q want %q", decoded.Instruction, original.Instruction)
	}
	if decoded.AcknowledgePrior != original.AcknowledgePrior {
		t.Errorf("AcknowledgePrior: got %q want %q", decoded.AcknowledgePrior, original.AcknowledgePrior)
	}
	if decoded.Phase != original.Phase {
		t.Errorf("Phase: got %q want %q", decoded.Phase, original.Phase)
	}
	if decoded.Temperature != original.Temperature {
		t.Errorf("Temperature: got %q want %q", decoded.Temperature, original.Temperature)
	}
	if decoded.UserIntent != original.UserIntent {
		t.Errorf("UserIntent: got %q want %q", decoded.UserIntent, original.UserIntent)
	}
	if decoded.GlobalGuidance != original.GlobalGuidance {
		t.Errorf("GlobalGuidance: got %q want %q", decoded.GlobalGuidance, original.GlobalGuidance)
	}
	if !decoded.SkipRoomAnnounce {
		t.Errorf("SkipRoomAnnounce should round-trip true")
	}
}

func TestDirective_DirectiveAsMapIncludesAllConductorFields(t *testing.T) {
	d := &AgentParticipationDirective{
		Mode:             DirectivePrimary,
		Instruction:      "do thing",
		AcknowledgePrior: "nod",
		ExpectedOutput:   "joke",
		GlobalGuidance:   "global",
		Phase:            "working",
		Temperature:      "focused",
		UserIntent:       "intent",
	}
	m := directiveAsMap(d)
	wantKeys := []string{"instruction", "acknowledgePrior", "expectedOutput", "globalGuidance", "phase", "temperature", "userIntent"}
	for _, k := range wantKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("directive map missing key %q", k)
		}
	}
}

// -----------------------------------------------------------------------------
// Impersonation-pattern guard tests
// -----------------------------------------------------------------------------

func TestImpersonationGuard_TellOtherAgentsJoke(t *testing.T) {
	names := candidateNamesForLookup(rosterFor())
	cases := []struct {
		name        string
		dispatched  string
		instruction string
		want        bool
	}{
		// Clear-cut impersonation -- explicit relay verbs targeting
		// another agent.
		{"speak for atlas", "p-sofia", "Speak for Atlas while he is unavailable", true},
		{"answer on behalf of wren", "p-atlas", "Answer on behalf of Wren about customer issues", true},
		{"fill in for sofia", "p-atlas", "Fill in for Sofia and keep the chain moving", true},

		// Should NOT trip -- the Go-side guard is intentionally
		// conservative on possessive ("tell Sofia's joke") and
		// wildcard ("tell .* joke") patterns because they false-
		// positive on legit fan-out instructions like
		//   "Tell a short customer-service joke. Make it different
		//    from Moss's joke."
		// (real production failure: Nyx kept getting dropped from
		// joke-fan-out plans because "tell .* joke" + "Moss in
		// instruction" both matched.)
		// Those possessive cases are now defended at the agent-side
		// prompt rule, not Go-side.
		{"tell name's joke (now passes Go-side; agent prompt handles)", "p-atlas", "Tell Sofia's joke for her", false},
		{"present name's answer (same)", "p-atlas", "Present Sofia's answer to the joke question", false},
		{"differentiating reference (the failure-mode case)", "p-wren",
			"Tell a short customer-service joke in your own voice. Make it clearly different from Moss's joke.", false},
		{"tell own joke", "p-atlas", "Tell your own joke about engineering", false},
		{"reply to atlas", "p-sofia", "Reply to Atlas's point about OAuth", false},
		{"build on sofia", "p-atlas", "Build on Sofia's earlier comment with your own angle", false},
		{"empty", "p-atlas", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isImpersonationInstruction(tc.instruction, tc.dispatched, names)
			if got != tc.want {
				t.Errorf("got %v want %v\ninstruction: %q", got, tc.want, tc.instruction)
			}
		})
	}
}

func TestPlanValidation_DropsImpersonationPlan(t *testing.T) {
	// Conductor produces a chime-in with an UNAMBIGUOUS relay verb
	// targeting another agent ("speak for"). Go-side guard catches
	// it. Possessive ("tell X's joke") is no longer Go-side; agent-
	// prompt rule handles that case.
	raw := `{
		"phase": "warming",
		"temperature": "casual",
		"userIntent": "fan-out",
		"globalGuidance": "test",
		"primary": {"agentId": "p-sofia", "instruction": "Tell your own joke", "brevity": "short", "acknowledgePrior": "", "expectedOutput": "joke"},
		"sequence": [],
		"chimeIns": [
			{"agentId": "p-atlas", "instruction": "Speak for Sofia since she's quiet", "brevity": "short", "acknowledgePrior": "", "expectedOutput": "joke"},
			{"agentId": "p-wren", "instruction": "Tell your own joke", "brevity": "short", "acknowledgePrior": "", "expectedOutput": "joke"}
		],
		"sessionSummary": "",
		"completionCriteria": "",
		"branchPoints": [],
		"reason": "test"
	}`
	plan := validatePlanJSON(t, raw, rosterFor())
	if len(plan.ChimeIns) != 1 {
		t.Errorf("chime-ins: got %d want 1 (impersonation plan should be dropped)", len(plan.ChimeIns))
	}
	if plan.ChimeIns[0].AgentId != "p-wren" {
		t.Errorf("kept entry: got %q want p-wren (the non-impersonation plan)", plan.ChimeIns[0].AgentId)
	}
}

func TestPlanValidation_DropsImpersonationPrimary(t *testing.T) {
	// Primary itself is impersonation-shaped using an explicit relay
	// verb ("answer on behalf of").
	raw := `{
		"phase": "warming",
		"temperature": "casual",
		"userIntent": "test",
		"globalGuidance": "test",
		"primary": {"agentId": "p-atlas", "instruction": "Answer on behalf of Sofia since she's away", "brevity": "short", "acknowledgePrior": "", "expectedOutput": "joke"},
		"sequence": [],
		"chimeIns": [],
		"sessionSummary": "",
		"completionCriteria": "",
		"branchPoints": [],
		"reason": "test"
	}`
	plan := validatePlanJSON(t, raw, rosterFor())
	if plan.PrimaryAgentId() != "" {
		t.Errorf("primary: got %q want empty (impersonation primary should be dropped to silence)", plan.PrimaryAgentId())
	}
}

// Production-failure regression: the conductor produced a legitimate
// fan-out chime-in for Nyx that referenced the previous joker for
// differentiation purposes ("Make it clearly different from Moss's
// joke"). The old Go-side guard treated the wildcard "tell .* joke"
// + the candidate name "Moss" as impersonation and dropped Nyx from
// every plan. This test pins down the fix: legit fan-out instructions
// that reference other agents for differentiation must NOT be flagged.
func TestPlanValidation_KeepsDifferentiatingReference(t *testing.T) {
	raw := `{
		"phase": "warming",
		"temperature": "casual",
		"userIntent": "fan-out",
		"globalGuidance": "test",
		"primary": {"agentId": "p-sofia", "instruction": "Tell your own short joke", "brevity": "short", "acknowledgePrior": "", "expectedOutput": "joke"},
		"sequence": [
			{"agentId": "p-atlas", "instruction": "Tell a short customer-service joke in your own voice. Make it clearly different from Moss's joke.", "brevity": "short", "acknowledgePrior": "", "expectedOutput": "joke"}
		],
		"chimeIns": [],
		"sessionSummary": "",
		"completionCriteria": "",
		"branchPoints": [],
		"reason": "test"
	}`
	// Use a roster that has a "Moss" agent so the candidate-name
	// match would have triggered the old false positive.
	candidates := []polyphon.AgentCandidate{
		candidate("p-sofia", "agent-sofia", "Sofia", "assistant"),
		candidate("p-atlas", "agent-atlas", "Atlas", "engineering-technology"),
		candidate("p-moss", "agent-moss", "Moss", "legal-compliance"),
	}
	plan := validatePlanJSON(t, raw, candidates)
	if len(plan.Sequence) != 1 {
		t.Errorf("sequence: got %d want 1 (differentiating reference should NOT be dropped); plan: %+v", len(plan.Sequence), plan)
	}
}

// -----------------------------------------------------------------------------
// ExpectedOutput round-trip tests
// -----------------------------------------------------------------------------

func TestPlan_ExpectedOutputRoundtrips(t *testing.T) {
	raw := `{
		"phase": "warming",
		"temperature": "casual",
		"userIntent": "joke from each",
		"globalGuidance": "fan-out",
		"primary": {"agentId": "p-sofia", "instruction": "Tell your own joke", "brevity": "short", "acknowledgePrior": "", "expectedOutput": "joke"},
		"sequence": [],
		"chimeIns": [],
		"sessionSummary": "",
		"completionCriteria": "all three jokes told",
		"branchPoints": [{"condition": "any agent didn't joke", "evaluateAfter": "after chain settles, check"}],
		"reason": "test"
	}`
	plan := validatePlanJSON(t, raw, rosterFor())
	if plan.Primary.ExpectedOutput != "joke" {
		t.Errorf("expectedOutput: got %q want joke", plan.Primary.ExpectedOutput)
	}
	if plan.CompletionCriteria == "" {
		t.Errorf("completionCriteria should round-trip")
	}
	if len(plan.BranchPoints) != 1 {
		t.Errorf("branchPoints: got %d want 1", len(plan.BranchPoints))
	}
	d := directiveFromConductorAgentPlan(&plan, &plan.Primary, "primary")
	if d.ExpectedOutput != "joke" {
		t.Errorf("directive ExpectedOutput: got %q want joke", d.ExpectedOutput)
	}
}

func TestPlan_HintRoundtripIncludesExpectedOutput(t *testing.T) {
	original := &AgentParticipationDirective{
		Mode:           DirectivePrimary,
		Instruction:    "Tell your own joke",
		ExpectedOutput: "joke",
	}
	hints := make(map[string]string)
	EncodeDirectiveIntoHints(original, hints)
	decoded := DecodeDirectiveFromHints(hints)
	if decoded == nil || decoded.ExpectedOutput != "joke" {
		t.Errorf("expectedOutput hint round-trip failed: %+v", decoded)
	}
}

// -----------------------------------------------------------------------------
// Schema strictness regression test
// -----------------------------------------------------------------------------

// -----------------------------------------------------------------------------
// Already-spoken filter for continuation re-consults
// -----------------------------------------------------------------------------

// TestFilterAlreadySpoken_DropsRepeatedAgent verifies the Go-side
// enforcement of "don't re-dispatch agents who already produced output
// this turn cycle." When the conductor re-consults after a chain
// settles and produces a plan that includes an already-spoken agent
// (e.g., Briar told a joke in plan #N, plan #N+1 still includes Briar),
// the filter strips that agent before dispatch.
func TestFilterAlreadySpoken_DropsRepeatedAgent(t *testing.T) {
	c := &CognitionIntegration{conductors: NewConductorRegistry()}
	state := c.conductors.GetOrCreate("space-x")
	// Simulate Briar (agent template id "agent-briar") having spoken
	state.RecordAgentSpoke("agent-briar")

	// agentConfigs maps participant id -> agentPayload (with template id)
	configs := map[string]*agentPayload{
		"p-briar": {ID: "agent-briar", Name: "Briar"},
		"p-ember": {ID: "agent-ember", Name: "Ember"},
		"p-sofia": {ID: "agent-sofia", Name: "Sofia"},
	}

	plan := &ConductorPlan{
		Primary: ConductorAgentPlan{AgentId: "p-briar", Instruction: "tell joke #2"},
		Sequence: []ConductorAgentPlan{
			{AgentId: "p-ember", Instruction: "tell your joke"},
			{AgentId: "p-sofia", Instruction: "tell your joke"},
		},
	}

	filtered := c.filterAlreadySpokenFromContinuation("space-x", plan, configs)
	if filtered == nil {
		t.Fatalf("filtered plan should be non-nil (Ember and Sofia should remain)")
	}
	if filtered.Primary.AgentId != "" {
		t.Errorf("Briar should be cleared from primary, got %q", filtered.Primary.AgentId)
	}
	if len(filtered.Sequence) != 2 {
		t.Errorf("sequence should still have Ember + Sofia, got %d", len(filtered.Sequence))
	}
}

func TestFilterAlreadySpoken_AllAgentsAlreadySpoke(t *testing.T) {
	c := &CognitionIntegration{conductors: NewConductorRegistry()}
	state := c.conductors.GetOrCreate("space-x")
	state.RecordAgentSpoke("agent-briar")
	state.RecordAgentSpoke("agent-ember")
	state.RecordAgentSpoke("agent-sofia")

	configs := map[string]*agentPayload{
		"p-briar": {ID: "agent-briar"},
		"p-ember": {ID: "agent-ember"},
		"p-sofia": {ID: "agent-sofia"},
	}

	plan := &ConductorPlan{
		Primary: ConductorAgentPlan{AgentId: "p-briar"},
		Sequence: []ConductorAgentPlan{
			{AgentId: "p-ember"}, {AgentId: "p-sofia"},
		},
	}

	filtered := c.filterAlreadySpokenFromContinuation("space-x", plan, configs)
	if filtered != nil {
		t.Errorf("filtered plan should be nil (all agents already spoke), got %+v", filtered)
	}
}

func TestFilterAlreadySpoken_PreservesNewAgents(t *testing.T) {
	c := &CognitionIntegration{conductors: NewConductorRegistry()}
	state := c.conductors.GetOrCreate("space-x")
	// Only Briar has spoken
	state.RecordAgentSpoke("agent-briar")

	configs := map[string]*agentPayload{
		"p-briar": {ID: "agent-briar"},
		"p-ember": {ID: "agent-ember"},
		"p-sofia": {ID: "agent-sofia"},
	}

	plan := &ConductorPlan{
		Primary: ConductorAgentPlan{AgentId: "p-sofia", Instruction: "answer"},
		Sequence: []ConductorAgentPlan{
			{AgentId: "p-ember", Instruction: "answer"},
			{AgentId: "p-briar", Instruction: "answer again"},
		},
	}

	filtered := c.filterAlreadySpokenFromContinuation("space-x", plan, configs)
	if filtered == nil {
		t.Fatalf("filtered plan should retain Sofia + Ember")
	}
	if filtered.Primary.AgentId != "p-sofia" {
		t.Errorf("primary should remain Sofia, got %q", filtered.Primary.AgentId)
	}
	if len(filtered.Sequence) != 1 || filtered.Sequence[0].AgentId != "p-ember" {
		t.Errorf("sequence should have only Ember, got %+v", filtered.Sequence)
	}
}

func TestFilterAlreadySpoken_NilPlanOK(t *testing.T) {
	c := &CognitionIntegration{conductors: NewConductorRegistry()}
	got := c.filterAlreadySpokenFromContinuation("any", nil, nil)
	if got != nil {
		t.Errorf("nil plan should return nil")
	}
}

// -----------------------------------------------------------------------------
// routingOutcomeFromConductorPlan tests (unified-brain adapter)
// -----------------------------------------------------------------------------

func TestRoutingOutcome_BuiltFromPlanPrimary(t *testing.T) {
	plan := &ConductorPlan{
		Primary: ConductorAgentPlan{
			AgentId:     "p-sofia",
			Instruction: "Greet the user",
		},
		FitScore:    0.9,
		TurnMode:    "answer",
		Handoff:     true,
		HandoffFrom: "Atlas",
		Severity:    "",
		Reason:      "Sofia is the right fit",
	}
	candidates := rosterFor()
	outcome := routingOutcomeFromConductorPlan(plan, candidates)
	if outcome == nil || !outcome.Respond {
		t.Fatalf("outcome should respond=true, got %+v", outcome)
	}
	if outcome.Winner == nil || outcome.Winner.AgentId != "p-sofia" {
		t.Errorf("winner: got %+v want p-sofia", outcome.Winner)
	}
	if outcome.Winner.AgentName != "Sofia" {
		t.Errorf("winner name: got %q want Sofia", outcome.Winner.AgentName)
	}
	if outcome.FitScore != 0.9 {
		t.Errorf("fitScore: got %f want 0.9", outcome.FitScore)
	}
	if outcome.TurnMode != "answer" {
		t.Errorf("turnMode: got %q want answer", outcome.TurnMode)
	}
	if !outcome.Handoff || outcome.HandoffFrom != "Atlas" {
		t.Errorf("handoff: got %v %q want true Atlas", outcome.Handoff, outcome.HandoffFrom)
	}
}

func TestRoutingOutcome_SilenceWhenPrimaryEmpty(t *testing.T) {
	plan := &ConductorPlan{Primary: ConductorAgentPlan{AgentId: ""}}
	outcome := routingOutcomeFromConductorPlan(plan, rosterFor())
	if outcome.Respond {
		t.Errorf("empty primary should produce silence (Respond=false), got %+v", outcome)
	}
}

func TestRoutingOutcome_SilenceWhenPrimaryNotInCandidates(t *testing.T) {
	plan := &ConductorPlan{Primary: ConductorAgentPlan{AgentId: "p-ghost"}}
	outcome := routingOutcomeFromConductorPlan(plan, rosterFor())
	if outcome.Respond {
		t.Errorf("unknown primary should produce silence, got %+v", outcome)
	}
}

func TestRoutingOutcome_DefaultsTurnModeToAnswer(t *testing.T) {
	plan := &ConductorPlan{
		Primary:  ConductorAgentPlan{AgentId: "p-sofia", Instruction: "go"},
		TurnMode: "", // conductor left it empty
	}
	outcome := routingOutcomeFromConductorPlan(plan, rosterFor())
	if outcome.TurnMode != "answer" {
		t.Errorf("empty turnMode should default to answer, got %q", outcome.TurnMode)
	}
}

func TestRoutingOutcome_EscalationNoticeWithSeverity(t *testing.T) {
	plan := &ConductorPlan{
		Primary:  ConductorAgentPlan{AgentId: "p-sofia", Instruction: "escalate"},
		FitScore: 0.2,
		TurnMode: "escalation_notice",
		Severity: "full_gap",
	}
	outcome := routingOutcomeFromConductorPlan(plan, rosterFor())
	if outcome.TurnMode != "escalation_notice" {
		t.Errorf("turnMode: got %q want escalation_notice", outcome.TurnMode)
	}
	if outcome.Severity != "full_gap" {
		t.Errorf("severity: got %q want full_gap", outcome.Severity)
	}
}

// -----------------------------------------------------------------------------
// extractRouterToolList tests
// -----------------------------------------------------------------------------

func TestExtractRouterToolList_StringSlice(t *testing.T) {
	caps := map[string]any{"tools": []string{"uiDescribe", "uiClick", "uiNarrate"}}
	got := extractRouterToolList(caps)
	if len(got) != 3 || got[0] != "uiDescribe" || got[2] != "uiNarrate" {
		t.Errorf("got %+v want [uiDescribe uiClick uiNarrate]", got)
	}
}

func TestExtractRouterToolList_AnySlice(t *testing.T) {
	caps := map[string]any{"tools": []any{"clawSearchCode", "clawReadFile"}}
	got := extractRouterToolList(caps)
	if len(got) != 2 || got[0] != "clawSearchCode" {
		t.Errorf("got %+v", got)
	}
}

func TestExtractRouterToolList_FiltersEmpty(t *testing.T) {
	caps := map[string]any{"tools": []string{"", "uiDescribe", "  "}}
	got := extractRouterToolList(caps)
	if len(got) != 1 || got[0] != "uiDescribe" {
		t.Errorf("got %+v", got)
	}
}

func TestExtractRouterToolList_NilCapabilities(t *testing.T) {
	got := extractRouterToolList(nil)
	if got != nil {
		t.Errorf("nil capabilities should return nil")
	}
}

func TestExtractRouterToolList_NoToolsKey(t *testing.T) {
	caps := map[string]any{"domains": []string{"hr"}}
	got := extractRouterToolList(caps)
	if got != nil {
		t.Errorf("missing tools key should return nil")
	}
}

func TestFilterAlreadySpoken_NoStateNoFilter(t *testing.T) {
	c := &CognitionIntegration{conductors: NewConductorRegistry()}
	// No state for this space yet
	plan := &ConductorPlan{
		Primary: ConductorAgentPlan{AgentId: "p-briar", Instruction: "go"},
	}
	got := c.filterAlreadySpokenFromContinuation("brand-new", plan, nil)
	if got == nil {
		t.Fatalf("should pass through when state exists but no agents have spoken")
	}
	if got.Primary.AgentId != "p-briar" {
		t.Errorf("primary should pass through, got %q", got.Primary.AgentId)
	}
}

// -----------------------------------------------------------------------------
// (TestPreviousAgentAskedQuestion + TestLooksLikeCorrection were removed
// when Phase 2 of the llm-driven-decisions plan replaced the
// affirmation/follow-up guard with the messageClassifier. The semantic
// signals those helpers produced are now produced by the classifier
// (carriesAction / answersPriorAgentPrompt / intent), so testing them
// individually doesn't make sense -- the test surface is the classifier
// prompt + the dispatcher decision logic.)
// -----------------------------------------------------------------------------
// TestSchema_RequiredCoversAllProperties is a regression test for the
// strict-mode schema bug we hit. OpenAI's structured outputs require
// every property in `properties` to be listed in `required` (and same
// recursively). If a property is "optional," it must be expressed by
// allowing empty string / empty array, not by omission from required.
//
// This test parses the schema JSON and verifies every object's
// required array covers every property. If it doesn't, the conductor
// will silently fail every consult with a 400 Bad Request -- exactly
// the failure mode that hid the new conductor for hours.
func TestSchema_RequiredCoversAllProperties(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(conductorPlanSchemaJSON), &schema); err != nil {
		t.Fatalf("schema parse: %v", err)
	}
	checkRequiredCoversProps(t, "$", schema)
}

func checkRequiredCoversProps(t *testing.T, path string, node map[string]any) {
	t.Helper()
	if typeStr, _ := node["type"].(string); typeStr == "object" {
		props, _ := node["properties"].(map[string]any)
		required, _ := node["required"].([]any)
		reqSet := make(map[string]bool, len(required))
		for _, r := range required {
			if s, ok := r.(string); ok {
				reqSet[s] = true
			}
		}
		for propName := range props {
			if !reqSet[propName] {
				t.Errorf("%s.%s is in properties but not required (strict mode requires both)",
					path, propName)
			}
		}
		// Recurse into property objects
		for propName, propVal := range props {
			if pm, ok := propVal.(map[string]any); ok {
				checkRequiredCoversProps(t, path+"."+propName, pm)
			}
		}
	}
	// Recurse into array items
	if items, ok := node["items"].(map[string]any); ok {
		checkRequiredCoversProps(t, path+"[]", items)
	}
}
