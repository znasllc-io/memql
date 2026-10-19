// Package models knows which local model runtimes this machine has, which
// models they can actually serve, and what to claim about each one.
//
// It exists for the same reason the apps package does: the engine cannot
// discover any of this. The cockpit dials out from behind NAT, and the
// engine reads what the cockpit reports on Register -- deriving the
// `model:<id>` routing labels the fleet router selects on (memql#4678).
// Nothing here decides whether a call happens; everything here decides
// whether this machine is ELIGIBLE to be picked.
//
// Three rules run through the whole package:
//
//   - EVERY CAPABILITY DEFAULTS TO ABSENT. The engine's attribute parser
//     is fail-closed: a model that says nothing about structured output is
//     never selected for a structured prompt. So a probe that cannot
//     establish a capability must say nothing rather than guess. The
//     failure a guess produces is not local -- a model that quietly answers
//     prose to a conductor turn produces a parse error three layers away,
//     naming nothing here.
//
//   - THE FLOOR GATES THE INVENTORY, IT DOES NOT ANNOTATE IT. A machine
//     below the hardware floor advertises no models at all. It stays a full
//     worker for everything else and nothing about it is degraded; it
//     simply is not an inference machine, and the reason is a sentence
//     rather than an absence.
//
//   - `models.allow` IS DEFAULT-DENY. Serving a call spends this machine's
//     own GPU on somebody else's prompt, so it gets the posture app consent
//     has: nothing is offered until the owner says which model may be. A
//     model that is present but unlisted is REPORTED as blocked rather than
//     omitted -- "present, blocked" is a state an operator can fix, and
//     rendering it identically to "not installed" sends them hunting the
//     wrong problem.
package models

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The wire contract, mirrored from the engine.
//
// These are MIRRORED rather than imported for the reason apps.go states
// about the app id set: cockpit resolves four memql packages, none of them
// `component/worker`, and the attribute encoding itself lives behind the
// engine's `agent` build tag where this module cannot reach it at all.
// The definitions are in memql `component/worker/modelcall.go` and
// `integrations/agent/worker/model_routing.go`; TestWireContract pins this
// copy against the exact strings those files parse.
const (
	// Capability is what a machine must advertise before the router will
	// consider it for a model call at all.
	Capability = "MODEL"
	// LabelPrefix is how a machine advertises a model it will serve.
	LabelPrefix = "model:"
	// RuntimeLabelPrefix names a runtime present on the machine. It
	// steers no selection -- two machines serving the same model through
	// different runtimes are interchangeable to a caller -- and exists
	// for the operator reading the Fleet page.
	RuntimeLabelPrefix = "runtime:"
)

// The runtime kinds this cockpit serves. Closed set (spec D1): Ollama
// discovered natively, and any OpenAI-compatible endpoint the machine's
// policy.yaml declares -- which covers LM Studio, vLLM and llamafile
// without a per-vendor integration for each.
const (
	KindOllama           = "ollama"
	KindOpenAICompatible = "openai-compatible"
)

// Attribute keys inside a `model:<id>` label value.
//
// The last three arrived with the cockpit AHEAD of the engine (design D5
// and D11; the engine half is memql#5096), which is the one direction in
// which a spelling mistake here is free of consequences until it is very
// expensive: the engine's parser skips a key it does not know, so a
// misspelled attribute is not rejected, not logged and not visible --
// it is a fleet that quietly never ranks by size and never routes a tool
// turn. TestWireContract is what holds the two halves to one spelling.
const (
	attrContext      = "ctx"
	attrStructured   = "structured"
	attrEmbeddings   = "embeddings"
	attrMax          = "max"
	attrParams       = "params"
	attrActiveParams = "activeparams"
	attrQuant        = "quant"
	attrTools        = "tools"

	// The four MODALITY keys (engine memql#5137, record D4). Same
	// situation as the three above and one step further out: the engine
	// half has not merged, so this repository is DEFINING these
	// spellings rather than transcribing them. A misspelling costs a
	// fleet that never routes a vision turn and raises nothing
	// anywhere, which is why TestWireContract pins all four.
	attrVision   = "vision"
	attrAudioIn  = "audioin"
	attrAudioOut = "audioout"
	attrImageGen = "imagegen"
)

// Attributes is what this machine claims about ONE model.
//
// The encoding is a flat `k=v` list rather than JSON, and that is the
// engine's choice rather than ours to improve on: labels are
// map[string]string end to end -- the concept, the wire, the Fleet page --
// and a JSON blob inside one of them is unreadable in every surface that
// renders labels as text.
type Attributes struct {
	// ContextWindow in tokens. Zero means this machine did not say, which
	// meets no floor -- the fail-closed direction.
	ContextWindow int
	// StructuredOutput reports that the runtime can honour a response
	// schema for this model.
	StructuredOutput bool
	// Embeddings reports that the model produces vectors.
	Embeddings bool
	// MaxConcurrent is the per-model ceiling. Zero means none declared,
	// which the engine's load ordering reads as unlimited.
	MaxConcurrent int
	// Params is the parameter COUNT, not the human string a runtime
	// prints. The engine ranks on it numerically -- "strongest first" is
	// parameters descending, then context window (D5) -- and "70B" sorts
	// before "8.0B" as text, so the conversion happens on this side where
	// the runtime's own spelling is still in hand.
	//
	// It GATES nothing: Satisfies does not read it, and a model that
	// states no size is still served for every prompt it is eligible for.
	// Zero means this machine did not say, and an unstated size sorts
	// LAST -- a model does not become the fleet's strongest by silence.
	Params int64
	// ActiveParams counts parameters used per token by a mixture, when established by the runtime.
	ActiveParams int64
	// Quant is the quantization level exactly as the runtime spelled it
	// ("Q4_K_M", "F16"). Verbatim rather than normalised, because an
	// operator compares it against what `ollama list` prints, and a
	// spelling this side invented is one they cannot find.
	//
	// It is the only FREE-TEXT attribute in the label, which makes it the
	// only one that can forge a second attribute -- see quantSafe.
	Quant string
	// Tools reports that the runtime can carry a tool-calling turn for
	// this model. The engine skips a machine whose tools is false when
	// the turn has tools (D6, D11), so the fail-closed direction here is
	// the same as everywhere else in this package.
	//
	// It is a SEPARATE field from StructuredOutput even though the Ollama
	// probe answers both from one capability, and it has to be: a
	// declared runtime's operator states the two independently, and
	// collapsing them would make "this model holds a schema" and "this
	// runtime can carry a tool call" one claim that no single source
	// actually establishes.
	Tools bool

	// The four modalities (engine memql#5137, record D4). Each is
	// false unless a RUNTIME PROBE confirmed it, and the package's
	// fail-closed rule bites hardest here: three of the four have no
	// capability any runtime reports, so the honest answer for them on
	// a bare Ollama is silence, and the operator's declaration under a
	// declared runtime is the only source that can say otherwise.
	//
	// A modality inferred from a model NAME would be the worst version
	// of this. "kokoro" in an id is not a runtime that answered, and a
	// machine advertising audioout on that basis takes a speech call it
	// cannot serve -- with the failure landing on somebody else's
	// prompt, three layers from here.

	// Vision reports that the model accepts image parts. The one
	// modality Ollama answers directly: `vision` is a capability in
	// /api/show, beside `tools` and `embedding`.
	Vision bool
	// AudioIn reports that the model accepts audio input (speech to
	// text). No Ollama capability corresponds to it, so it is declared
	// or absent.
	AudioIn bool
	// AudioOut reports that the model produces audio (text to speech).
	// Served by a Kokoro runtime rather than by Ollama, so it arrives
	// with the runtime that answers for it.
	AudioOut bool
	// ImageGen reports that the model generates images.
	ImageGen bool
}

// String renders the label value the engine parses. Keys are emitted in a
// fixed order and a zero-valued attribute is omitted entirely, so the same
// inventory always produces byte-identical labels -- an unstable rendering
// would rewrite the registration row on every reconnect for no change.
func (a Attributes) String() string {
	parts := make([]string, 0, 11)
	if a.ContextWindow > 0 {
		parts = append(parts, fmt.Sprintf("%s=%d", attrContext, a.ContextWindow))
	}
	if a.StructuredOutput {
		parts = append(parts, attrStructured+"=1")
	}
	if a.Embeddings {
		parts = append(parts, attrEmbeddings+"=1")
	}
	if a.MaxConcurrent > 0 {
		parts = append(parts, fmt.Sprintf("%s=%d", attrMax, a.MaxConcurrent))
	}
	if a.Params > 0 {
		parts = append(parts, fmt.Sprintf("%s=%d", attrParams, a.Params))
	}
	if a.ActiveParams > 0 {
		parts = append(parts, fmt.Sprintf("%s=%d", attrActiveParams, a.ActiveParams))
	}
	if q := strings.TrimSpace(a.Quant); quantSafe(q) {
		parts = append(parts, attrQuant+"="+q)
	}
	if a.Tools {
		parts = append(parts, attrTools+"=1")
	}
	// The modalities go LAST and in this order, and both matter: the
	// same inventory has to render byte-identically every time or the
	// registration row is rewritten on every reconnect for no change.
	//
	// FALSE IS ABSENT, not `vision=0`. The engine reads a missing key
	// as absent, so emitting a zero would make "this machine says no"
	// and "this machine did not say" the same string on the wire --
	// which is the one distinction the whole fail-closed design rests
	// on.
	if a.Vision {
		parts = append(parts, attrVision+"=1")
	}
	if a.AudioIn {
		parts = append(parts, attrAudioIn+"=1")
	}
	if a.AudioOut {
		parts = append(parts, attrAudioOut+"=1")
	}
	if a.ImageGen {
		parts = append(parts, attrImageGen+"=1")
	}
	return strings.Join(parts, ",")
}

// quantSafe reports whether a quantization level can travel inside a
// label value verbatim, and it is the SINGLE gate on that -- the probes
// store what the runtime said, and this is where an unusable one stops.
//
// Quant is the only free-text attribute in a delimiter-separated list, so
// it is the only one that can forge a second attribute: a level carrying
// a comma or an '=' would be re-read on the far side as some other key,
// and the engine would then act on an attribute this machine never
// claimed. Such a value is dropped WHOLE rather than escaped or
// truncated, because quant gates nothing (D5 makes it an ordering signal)
// -- losing it costs a tiebreak, while emitting it costs the integrity of
// every attribute standing beside it. Control bytes go the same way: this
// string is rendered on the Fleet page and into log lines.
func quantSafe(q string) bool {
	if q == "" {
		return false
	}
	for _, r := range q {
		if r < 0x20 || r > 0x7e || r == ',' || r == '=' {
			return false
		}
	}
	return true
}

// ParseAttributes reads a label value back. It exists so the round-trip
// against the engine's reading is assertable here rather than discovered
// in production, and it is deliberately the same shape as the engine's
// parser: unrecognised input costs eligibility rather than granting it.
func ParseAttributes(value string) Attributes {
	var a Attributes
	for _, part := range strings.Split(value, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case attrContext:
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				a.ContextWindow = n
			}
		case attrStructured:
			a.StructuredOutput = parseAdvertisedBool(v)
		case attrEmbeddings:
			a.Embeddings = parseAdvertisedBool(v)
		case attrMax:
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				a.MaxConcurrent = n
			}
		case attrParams:
			// A COUNT, base ten, and nothing else. The human string a
			// runtime prints ("8.0B") is converted where it is read, so a
			// label carrying one is a label some other writer produced --
			// and reading it as 8 would put a machine at the bottom of a
			// ranking it belongs at the top of.
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				a.Params = n
			}
		case attrActiveParams:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				a.ActiveParams = n
			}
		case attrQuant:
			// Refused unless String could have emitted it, so the set of
			// levels that survive a round trip is closed rather than
			// "whatever arrived".
			if quantSafe(v) {
				a.Quant = v
			}
		case attrTools:
			a.Tools = parseAdvertisedBool(v)
		case attrVision:
			a.Vision = parseAdvertisedBool(v)
		case attrAudioIn:
			a.AudioIn = parseAdvertisedBool(v)
		case attrAudioOut:
			a.AudioOut = parseAdvertisedBool(v)
		case attrImageGen:
			a.ImageGen = parseAdvertisedBool(v)
		}
	}
	return a
}

// parseAdvertisedBool mirrors the engine's permissiveness, which runs in
// exactly one direction: anything unrecognised is false, so a novel
// spelling costs eligibility rather than granting it.
func parseAdvertisedBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y":
		return true
	}
	return false
}

// Label renders the advertisement label for a model id.
func Label(modelId string) string { return LabelPrefix + strings.TrimSpace(modelId) }

// RuntimeLabel renders the advertisement label for a runtime kind.
func RuntimeLabel(kind string) string { return RuntimeLabelPrefix + strings.TrimSpace(kind) }

// Info is one model this machine found, and everything needed to serve a
// call against it.
//
// Allowed is this machine's own policy.yaml verdict and is separate from
// the attributes on purpose: a blocked model is still reported, so the
// portal can distinguish it from one that is not installed.
type Info struct {
	Transcription string
	Voices        map[string]string
	// ID is the runtime-facing model id -- the exact string the router
	// will select on and send back in ModelCallStart.model, so it is
	// never normalised or prettified here.
	ID string
	// Kind is the runtime family: KindOllama or KindOpenAICompatible.
	Kind string
	// Runtime is the operator-facing runtime name. "ollama" for the
	// native probe; whatever policy.yaml called it for a declared one.
	// It is NOT what goes in the runtime label -- the engine's set is
	// the kinds -- and exists so `memql worker models` can tell two
	// declared endpoints apart.
	Runtime string
	// BaseURL is where a call to this model goes.
	BaseURL string
	// APIKeyEnv names the environment variable holding this runtime's
	// bearer, when it needs one. The VALUE is never stored on this
	// struct: Info is printed by the CLI and logged, and a struct that
	// holds a secret eventually prints one.
	APIKeyEnv string
	// Allowed mirrors policy.yaml models.allow.
	Allowed bool

	// KeepAlive and WorkingContext are this machine's instructions to its
	// own Ollama for THIS model (policy.yaml models.resident). They are not
	// Attributes, and the difference is the point: Attributes are what the
	// engine is told, and the engine has no use for either of these. A key
	// on the label that the engine does not parse is skipped without a
	// word, which is a setting nobody can see fail.
	//
	// Both are the zero value unless the owner wrote an entry, and only an
	// Ollama model ever carries them -- an OpenAI-compatible runtime has no
	// keep_alive to send it.
	KeepAlive      KeepAlive
	WorkingContext int

	// CeilingSource is where Attributes.MaxConcurrent came from, as the tail of
	// a sentence ("the memql-ollama.service unit"). Discovery fills it when it
	// fills the ceiling, so the worker's log says what discovery USED. It is
	// not a label attribute: the engine is told the number, never why.
	CeilingSource string

	Attributes
}

// Inventory is the whole answer to "what does this machine serve".
type Inventory struct {
	// Floor is the hardware verdict. When it is not met, Advertised is
	// empty no matter what Models holds.
	Floor FloorVerdict
	// Models is everything found, allowed or not, sorted by id. It is
	// what the diagnostic prints.
	Models []Info
	// ProbeNotes records why a runtime contributed nothing -- "no
	// response from Ollama at ...", "declared runtime lmstudio refused
	// the connection". Absence of a runtime is not an error and is not
	// logged as one; it is a sentence an operator can read when they ask.
	ProbeNotes []string
	// RuntimeVersions is the version each runtime kind reported, keyed
	// by kind (memql-cockpit#400, engine record D7). It becomes the
	// VALUE of the `runtime:<kind>` label, which was previously empty.
	//
	// A runtime present with no readable version keeps an empty value,
	// exactly as today: the LABEL is the advertisement that the runtime
	// is here, and the version is extra. Dropping the label because a
	// version could not be read would hide a runtime that is running.
	RuntimeVersions map[string]string
}

// Advertised is the set this machine actually offers: allowed models, and
// only when the hardware floor is met.
func (inv Inventory) Advertised() []Info {
	if !inv.Floor.Met {
		return nil
	}
	out := make([]Info, 0, len(inv.Models))
	for _, m := range inv.Models {
		if m.Allowed {
			out = append(out, m)
		}
	}
	return out
}

// Labels renders the registration labels for the advertised set: one
// `model:<id>` per model and one `runtime:<kind>` per kind present.
//
// Returns nil when nothing is advertised, so a machine that offers no
// models sends no model labels at all rather than an empty marker.
func (inv Inventory) Labels() map[string]string {
	advertised := inv.Advertised()
	if len(advertised) == 0 {
		return nil
	}
	out := make(map[string]string, len(advertised)+2)
	for _, m := range advertised {
		out[Label(m.ID)] = m.Attributes.String()
		// The runtime label now carries its VERSION as the value
		// (record D7: "advertise runtime:<name> with a version once
		// present"). It was empty before, so this changes every
		// machine's label fingerprint once on rollout -- the cost D8
		// accepts, and the same one params and quant already charged.
		out[RuntimeLabel(m.Kind)] = strings.TrimSpace(inv.RuntimeVersions[m.Kind])
	}
	return out
}

// RuntimeKinds lists the kinds behind the advertised set, sorted.
func (inv Inventory) RuntimeKinds() []string {
	seen := map[string]bool{}
	for _, m := range inv.Advertised() {
		seen[m.Kind] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Find returns the advertised model with this id. A call for a model this
// machine does not currently offer is refused rather than served: the
// advertisement is the promise, and honouring a call outside it would let
// a model removed from models.allow keep serving until the next reconnect.
func (inv Inventory) Find(modelId string) (Info, bool) {
	for _, m := range inv.Advertised() {
		if m.ID == modelId {
			return m, true
		}
	}
	return Info{}, false
}
