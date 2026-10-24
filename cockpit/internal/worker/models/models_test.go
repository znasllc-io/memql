package models

import (
	"strings"
	"testing"
)

// TestWireContract pins the mirrored constants as LITERALS.
//
// They are not asserted against an import of the engine's own
// definitions, and that is a deliberate trade rather than an oversight.
// component/worker is a nested module of its own in the memql split
// (memql#3228); requiring it here for three strings would put cockpit's
// build graph behind a module that is still moving, which is the exact
// fragility .github/memql-pin exists to contain. So this repo does what
// apps.go already does with the engine's app id set: mirror, and name the
// source.
//
// Sources, read at the pinned sha 5c4f6ae9679e97609c115ec484ed1aa16edebd7a:
// memql component/worker/modelcall.go:399 (ModelCapability), :428
// (ModelLabelPrefix), :434 (RuntimeLabelPrefix); memql
// integrations/agent/worker/model_routing.go:98-103 (the attribute key
// const block: ctx, structured, embeddings, max).
//
// params, quant and tools are pinned here even though the engine at that
// pin parses NONE of them, and that inversion is the point: the cockpit
// ships the spelling FIRST (the engine half is memql#5096, unmerged when
// this landed), so for those three this test is the DEFINITION the
// engine's ModelAttributes must match rather than a transcription of it.
// The spellings come from the design record -- D5 for params=<count> and
// quant=<level>, D6 and D11 for tools. A synonym invented here is free to
// write and costs the whole feature silently: the engine's parser skips a
// key it does not know, so a mismatch raises nothing anywhere -- it is a
// fleet that never ranks by size and never routes a tool turn, with
// nothing logged on either side.
func TestWireContract(t *testing.T) {
	if Capability != "MODEL" {
		t.Errorf("Capability = %q, engine says %q", Capability, "MODEL")
	}
	if LabelPrefix != "model:" {
		t.Errorf("LabelPrefix = %q, engine says %q", LabelPrefix, "model:")
	}
	if RuntimeLabelPrefix != "runtime:" {
		t.Errorf("RuntimeLabelPrefix = %q, engine says %q", RuntimeLabelPrefix, "runtime:")
	}
	if got := Label("llama3.1:8b"); got != "model:llama3.1:8b" {
		t.Errorf("Label = %q, want %q", got, "model:llama3.1:8b")
	}
	if got := RuntimeLabel(KindOpenAICompatible); got != "runtime:openai-compatible" {
		t.Errorf("RuntimeLabel = %q, want %q", got, "runtime:openai-compatible")
	}

	for _, tc := range []struct{ got, want, source string }{
		{attrContext, "ctx", "model_routing.go:99"},
		{attrStructured, "structured", "model_routing.go:100"},
		{attrEmbeddings, "embeddings", "model_routing.go:101"},
		{attrMax, "max", "model_routing.go:102"},
		{attrParams, "params", "design D5"},
		{attrActiveParams, "activeparams", "engine model_routing.go -- memql#5235"},
		{attrQuant, "quant", "design D5"},
		{attrTools, "tools", "design D6 and D11"},
		// The four modalities. NOT transcribed from the engine, because
		// there is nothing upstream to transcribe: memql#5137 has not
		// merged, so this repository is defining these four spellings
		// and the engine's parser will have to match them. The source
		// column says so rather than pretending otherwise.
		{attrVision, "vision", "open-weight-defaults D4 -- DEFINED HERE, engine memql#5137 pending"},
		{attrAudioIn, "audioin", "open-weight-defaults D4 -- DEFINED HERE, engine memql#5137 pending"},
		{attrAudioOut, "audioout", "open-weight-defaults D4 -- DEFINED HERE, engine memql#5137 pending"},
		{attrImageGen, "imagegen", "open-weight-defaults D4 -- DEFINED HERE, engine memql#5137 pending"},
	} {
		if tc.got != tc.want {
			t.Errorf("attribute key = %q, %s says %q", tc.got, tc.source, tc.want)
		}
	}
}

// TestAttributesString pins the exact label VALUE the engine parses.
//
// The encoding lives in the engine's integrations/agent/worker/
// model_routing.go, which is behind its `agent` build tag and therefore
// out of reach of an import. These literals are that file's
// ModelAttributes.String, transcribed -- so a change there fails here
// with a diff an operator can read rather than with a machine that is in
// the catalog and never picked.
func TestAttributesString(t *testing.T) {
	tests := []struct {
		name string
		in   Attributes
		want string
	}{
		{"everything", Attributes{ContextWindow: 131072, StructuredOutput: true, Embeddings: true, MaxConcurrent: 2,
			Params: 8000000000, Quant: "Q4_K_M", Tools: true},
			"ctx=131072,structured=1,embeddings=1,max=2,params=8000000000,quant=Q4_K_M,tools=1"},
		// The plan's worked example, character for character
		// (docs/superpowers/plans/2026-09-06-fleet-inference-and-app-door.md,
		// PR 1 Task 1 "Interfaces"). The key ORDER is part of the
		// contract, not a rendering detail: the engine reads the value as
		// a set, but the cockpit re-sends it on every reconnect, and a
		// value whose order drifted would rewrite the registration row
		// for no change.
		{"the plan's example", Attributes{ContextWindow: 131072, StructuredOutput: true, MaxConcurrent: 2,
			Params: 8000000000, Quant: "Q4_K_M", Tools: true},
			"ctx=131072,structured=1,max=2,params=8000000000,quant=Q4_K_M,tools=1"},
		{"context only", Attributes{ContextWindow: 8192}, "ctx=8192"},
		{"embeddings model", Attributes{ContextWindow: 512, Embeddings: true, MaxConcurrent: 4},
			"ctx=512,embeddings=1,max=4"},
		// Size without capability. params and quant are ORDERING signals
		// (D5), not gates, so a model can be the biggest on the fleet and
		// still be ineligible for every prompt that needs a capability.
		{"size but nothing claimed", Attributes{Params: 70000000000, Quant: "Q4_0"},
			"params=70000000000,quant=Q4_0"},
		// The fail-closed direction, rendered: a model this machine can
		// say nothing about produces an EMPTY value, not a value full of
		// zeroes and falses. The engine reads both the same way; the
		// empty one is what says so honestly.
		{"nothing known", Attributes{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
			if got := ParseAttributes(tt.want); got != tt.in {
				t.Errorf("round trip = %+v, want %+v", got, tt.in)
			}
		})
	}
}

// TestAttributesRoundTrip drives the round trip from the STRUCT side, so
// the two halves are pinned without a hand-written literal in between.
//
// It is what says the cockpit and the engine can be reasoned about as one
// encoding: whatever a probe puts in Attributes is what a reader of the
// label gets back, and a field added to the struct without a key, or with
// a key String emits and ParseAttributes ignores, fails here rather than
// in a fleet that silently stopped ranking by size.
func TestAttributesRoundTrip(t *testing.T) {
	for _, in := range []Attributes{
		{},
		{ContextWindow: 131072, StructuredOutput: true, Embeddings: true, MaxConcurrent: 6,
			Params: 8030261248, Quant: "Q4_K_M", Tools: true},
		{Params: 1500000000, Quant: "F16"},
		{Params: 405000000000, Quant: "IQ2_XXS", Tools: true, MaxConcurrent: 1},
		{ContextWindow: 2048, Embeddings: true, Params: 137000000, Quant: "F16", MaxConcurrent: 4},
	} {
		rendered := in.String()
		if got := ParseAttributes(rendered); got != in {
			t.Errorf("ParseAttributes(%q) = %+v, want %+v", rendered, got, in)
		}
		if again := ParseAttributes(rendered).String(); again != rendered {
			t.Errorf("re-rendering %q produced %q; the label must be byte-stable", rendered, again)
		}
	}
}

// TestAttributesString_QuantCannotForgeAnAttribute.
//
// quant is the ONLY free-text attribute in a delimiter-separated list, so
// it is the only one that can forge a second attribute out of a runtime's
// string. A value carrying a comma or an '=' is dropped WHOLE rather than
// escaped or truncated: quant gates nothing, so losing it costs an
// ordering tiebreak, while emitting it costs the integrity of every
// attribute standing beside it.
func TestAttributesString_QuantCannotForgeAnAttribute(t *testing.T) {
	for _, q := range []string{"Q4_K_M,max=999", "Q4=K", "Q4\nK", "Q4\tK", "", "   "} {
		if got := (Attributes{ContextWindow: 4096, Quant: q}).String(); got != "ctx=4096" {
			t.Errorf("quant %q rendered as %q; an unsafe quant must be dropped whole", q, got)
		}
	}
	// Interior whitespace survives, because it cannot forge anything and
	// dropping it would lose a level an operator can read.
	if got := (Attributes{Quant: "Q4 K M"}).String(); got != "quant=Q4 K M" {
		t.Errorf("String() = %q, want the level kept", got)
	}
}

// TestParseAttributes_FailsClosed. Every unreadable input costs
// eligibility rather than granting it -- the direction that turns a
// garbled label into "this machine is not picked for structured prompts"
// instead of "this machine answers prose to a conductor turn".
func TestParseAttributes_FailsClosed(t *testing.T) {
	for _, in := range []string{
		"ctx=notanumber,structured=maybe,embeddings=perhaps,max=lots",
		"params=notanumber,quant=,tools=maybe",
		"params=8e9",  // the label carries a COUNT, never a human string
		"params=8.0B", // likewise: the conversion happens on this side
		"ctx=-1,max=0", "params=-1", "params=0",
		"garbage",
		"",
		"structured", // no '=' at all
	} {
		if got := ParseAttributes(in); got != (Attributes{}) {
			t.Errorf("ParseAttributes(%q) = %+v, want the zero value", in, got)
		}
	}
	// The permissive spellings the engine accepts, and only those.
	for _, yes := range []string{"1", "true", "TRUE", "yes", "y", "Y"} {
		if !ParseAttributes("structured=" + yes).StructuredOutput {
			t.Errorf("structured=%s should parse true", yes)
		}
		if !ParseAttributes("tools=" + yes).Tools {
			t.Errorf("tools=%s should parse true", yes)
		}
	}
	for _, no := range []string{"0", "false", "no", "on", "enabled", "sure"} {
		if ParseAttributes("structured=" + no).StructuredOutput {
			t.Errorf("structured=%s must NOT parse true", no)
		}
		if ParseAttributes("tools=" + no).Tools {
			t.Errorf("tools=%s must NOT parse true", no)
		}
	}
	// A quant that String would never emit is not accepted back either,
	// so the set of levels that survive a round trip is closed.
	if got := ParseAttributes("quant=Q4=K").Quant; got != "" {
		t.Errorf("quant = %q, want it refused: String cannot have emitted it", got)
	}
}

func offered(id string, attrs Attributes) Info {
	return Info{ID: id, Kind: KindOllama, Runtime: KindOllama, BaseURL: "http://x", Allowed: true, Attributes: attrs}
}

// TestAdvertised_FloorGatesEverything. Below the floor a machine offers
// nothing, however much it has installed and however much of it the owner
// allowed. The floor gates the inventory; it does not annotate it.
func TestAdvertised_FloorGatesEverything(t *testing.T) {
	inv := Inventory{
		Floor:  FloorVerdict{Met: false, Reason: "an Intel Mac is not supported as an inference machine."},
		Models: []Info{offered("llama3.1:8b", Attributes{ContextWindow: 8192})},
	}
	if got := inv.Advertised(); len(got) != 0 {
		t.Errorf("Advertised() = %v below the floor, want none", got)
	}
	if got := inv.Labels(); got != nil {
		t.Errorf("Labels() = %v below the floor, want nil", got)
	}
	if _, ok := inv.Find("llama3.1:8b"); ok {
		t.Error("Find() resolved a model below the floor")
	}
}

// TestAdvertised_BlockedIsReportedButNotOffered. models.allow is
// default-deny, and a blocked model stays VISIBLE -- that is what lets
// the portal say "present, blocked" rather than rendering it identically
// to "not installed".
func TestAdvertised_BlockedIsReportedButNotOffered(t *testing.T) {
	blocked := offered("qwen2.5:7b", Attributes{ContextWindow: 32768})
	blocked.Allowed = false
	inv := Inventory{
		Floor:  FloorVerdict{Met: true},
		Models: []Info{offered("llama3.1:8b", Attributes{ContextWindow: 8192, MaxConcurrent: 1}), blocked},
	}
	if len(inv.Models) != 2 {
		t.Fatalf("the blocked model must still be reported: %v", inv.Models)
	}
	adv := inv.Advertised()
	if len(adv) != 1 || adv[0].ID != "llama3.1:8b" {
		t.Fatalf("Advertised() = %v, want only the allowed model", adv)
	}
	if _, ok := inv.Find("qwen2.5:7b"); ok {
		t.Error("Find() resolved a blocked model; a call for it must be refused")
	}
}

// TestLabels_ShapeAndRuntimes.
func TestLabels_ShapeAndRuntimes(t *testing.T) {
	inv := Inventory{
		Floor: FloorVerdict{Met: true},
		Models: []Info{
			offered("llama3.1:8b", Attributes{ContextWindow: 131072, StructuredOutput: true, MaxConcurrent: 1,
				Params: 8000000000, Quant: "Q4_K_M", Tools: true}),
			{ID: "qwen2.5-7b", Kind: KindOpenAICompatible, Runtime: "lmstudio", Allowed: true,
				Attributes: Attributes{ContextWindow: 32768, StructuredOutput: true, MaxConcurrent: 1}},
		},
	}
	labels := inv.Labels()
	if got := labels["model:llama3.1:8b"]; got != "ctx=131072,structured=1,max=1,params=8000000000,quant=Q4_K_M,tools=1" {
		t.Errorf("model label = %q", got)
	}
	// The declared entry claimed no size, and says so by omission rather
	// than by zeroes: D5 sorts a model that does not state its size LAST,
	// and params=0 would be a claim that it has none.
	if got := labels["model:qwen2.5-7b"]; got != "ctx=32768,structured=1,max=1" {
		t.Errorf("declared model label = %q", got)
	}
	if _, ok := labels["runtime:ollama"]; !ok {
		t.Error("missing runtime:ollama")
	}
	if _, ok := labels["runtime:openai-compatible"]; !ok {
		t.Error("missing runtime:openai-compatible")
	}
	if got := inv.RuntimeKinds(); len(got) != 2 || got[0] != KindOllama || got[1] != KindOpenAICompatible {
		t.Errorf("RuntimeKinds() = %v", got)
	}
}

// -----------------------------------------------------------------------------
// The four modalities (engine memql#5137, record D4)
// -----------------------------------------------------------------------------

// The full label, every key, in emission order. This is the string the
// engine's parser has to read, and it is asserted whole rather than key
// by key: the ORDER is part of the contract, because an inventory that
// rendered differently between two runs would rewrite every machine's
// registration row on every reconnect for no change.
func TestModalityLabelRendersInEmissionOrder(t *testing.T) {
	full := Attributes{
		ContextWindow: 8192, StructuredOutput: true, Embeddings: true,
		MaxConcurrent: 4, Params: 9_000_000_000, Quant: "Q4_K_M", Tools: true,
		Vision: true, AudioIn: true, AudioOut: true, ImageGen: true,
	}
	want := "ctx=8192,structured=1,embeddings=1,max=4,params=9000000000,quant=Q4_K_M," +
		"tools=1,vision=1,audioin=1,audioout=1,imagegen=1"
	if got := full.String(); got != want {
		t.Fatalf("label =\n  %q\nwant\n  %q", got, want)
	}
	if got := ParseAttributes(want); got != full {
		t.Fatalf("round trip lost data:\n  %+v\nwant\n  %+v", got, full)
	}
}

// FALSE IS ABSENT. There is no vision=0: the engine reads a missing key
// as absent, so emitting a zero would make "this machine says no" and
// "this machine did not say" the same string on the wire.
func TestModalityFlagsAreOmittedWhenFalse(t *testing.T) {
	got := Attributes{ContextWindow: 8192, Tools: true}.String()
	for _, key := range []string{"vision", "audioin", "audioout", "imagegen"} {
		if strings.Contains(got, key) {
			t.Fatalf("%q appeared in a label that claims none of them: %q", key, got)
		}
	}
}

// Each flag is INDEPENDENT. A model that sees is not thereby a model
// that speaks, and a bug that set them together would advertise three
// modalities on the strength of one probe.
func TestModalityFlagsAreIndependent(t *testing.T) {
	for _, tc := range []struct {
		attrs Attributes
		want  string
	}{
		{Attributes{Vision: true}, "vision=1"},
		{Attributes{AudioIn: true}, "audioin=1"},
		{Attributes{AudioOut: true}, "audioout=1"},
		{Attributes{ImageGen: true}, "imagegen=1"},
	} {
		if got := tc.attrs.String(); got != tc.want {
			t.Fatalf("%+v rendered %q, want exactly %q", tc.attrs, got, tc.want)
		}
	}
}

// An unrecognised spelling costs eligibility rather than granting it,
// the same one-way permissiveness every other flag has.
func TestModalityFlagsParseFailClosed(t *testing.T) {
	got := ParseAttributes("vision=maybe,audioin=0,audioout=no,imagegen=off")
	if got.Vision || got.AudioIn || got.AudioOut || got.ImageGen {
		t.Fatalf("a modality was granted by an unrecognised value: %+v", got)
	}
	// And the spellings that DO count, matching parseAdvertisedBool.
	on := ParseAttributes("vision=1,audioin=true,audioout=yes,imagegen=y")
	if !on.Vision || !on.AudioIn || !on.AudioOut || !on.ImageGen {
		t.Fatalf("a documented truthy spelling was rejected: %+v", on)
	}
}

// THE FINGERPRINT MOVES. Every machine reconnects once on rollout --
// the cost record D8 accepts, and the thing to expect on deploy rather
// than read as an incident.
func TestModalityFlagsChangeTheLabel(t *testing.T) {
	before := Attributes{ContextWindow: 8192, Tools: true}.String()
	for _, after := range []Attributes{
		{ContextWindow: 8192, Tools: true, Vision: true},
		{ContextWindow: 8192, Tools: true, AudioIn: true},
		{ContextWindow: 8192, Tools: true, AudioOut: true},
		{ContextWindow: 8192, Tools: true, ImageGen: true},
	} {
		if after.String() == before {
			t.Fatalf("%+v renders identically to a model claiming no modality", after)
		}
	}
}
