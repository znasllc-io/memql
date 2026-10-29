package fleetcatalog

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

// MaxQuantLen bounds the quantization string a machine may report. It is
// operator-facing text from another process and nothing parses it, so the only
// job here is to stop a malformed cockpit from growing the registration row.
const MaxQuantLen = 32

// ModelNeeds is what a particular prompt requires of a model. A zero value
// needs nothing beyond the model existing.
type ModelNeeds struct {
	// StructuredOutput is set for conductor / planner / suggest prompts,
	// which parse the answer rather than reading it.
	StructuredOutput bool
	// Embeddings is set for embedding calls.
	Embeddings bool
	// Tools is set for a turn that offers the model functions to call.
	Tools bool
	// The four MODALITY needs (epic memql#5137, D4). Each is set by the call
	// KIND rather than by the prompt: a kind="vision" call needs Vision, a
	// kind="transcribe" call needs AudioIn, and so on.
	//
	// They are needs rather than a `Kind` field because Satisfies answers ONE
	// question -- can this machine serve this turn -- and a kind would make it
	// answer that question by switching on a second vocabulary. Every other
	// capability here is already a need, and the reasons an entry is skipped
	// read the same way whatever ruled it out.
	Vision   bool
	AudioIn  bool
	AudioOut bool
	ImageGen bool
	// MinContextWindow is the floor in tokens. Zero means no floor.
	MinContextWindow int
}

// NeedsForKind is what a model call of a given KIND requires of a machine,
// beyond whatever the prompt itself needs.
//
// It exists so the mapping from kind to capability lives in ONE place. The
// alternative -- each dispatch site setting the flag for its own kind -- is
// four places to forget one, and forgetting sends a vision turn to a machine
// that cannot see, which fails on somebody else's laptop.
func NeedsForKind(kind string) ModelNeeds {
	switch kind {
	case "vision":
		return ModelNeeds{Vision: true}
	case "transcribe":
		return ModelNeeds{AudioIn: true}
	case "speak":
		return ModelNeeds{AudioOut: true}
	case "image":
		return ModelNeeds{ImageGen: true}
	case "embedding":
		return ModelNeeds{Embeddings: true}
	}
	return ModelNeeds{}
}

// ModelAttributes is what a machine advertised about ONE model, carried as the
// value of its `model:<id>` label.
//
// The encoding is a flat `k=v` list rather than JSON because labels are
// `map[string]string` end to end -- the concept, the wire, the Fleet page --
// and a JSON blob inside one of them would be unreadable in every surface that
// renders labels as text.
//
// EVERY CAPABILITY DEFAULTS TO FALSE, and that direction is deliberate. A
// machine that says nothing about structured output is not selected for a
// structured prompt: a model that silently answers prose to a conductor turn
// produces a parse failure three layers away, naming nothing. Absent is
// "not advertised", which is a fact; assuming yes would be a guess that fails
// late.
type ModelAttributes struct {
	// ContextWindow in tokens. Zero means the machine did not say, which
	// does not meet any floor -- for the same fail-closed reason.
	ContextWindow int
	// StructuredOutput reports that the runtime can honour a response
	// schema for this model.
	StructuredOutput bool
	// Embeddings reports that the model produces vectors.
	Embeddings bool
	// Tools reports that the runtime can carry a tool-calling turn for this
	// model -- pass tool schemas in and surface tool calls back out.
	//
	// It is a RUNTIME capability as much as a model one: the same weights
	// behind an endpoint that does not implement the tools field cannot do
	// this, which is why it is advertised per machine rather than inferred
	// from the model id.
	Tools bool
	// Params is the parameter count the runtime reported, expanded to a
	// number (Ollama's `details.parameter_size`: "8B" -> 8000000000).
	//
	// AN ORDERING SIGNAL, NOT A CAPABILITY GATE. Satisfies does not read it
	// and must not start to: a model that never said how big it is stays
	// eligible for everything it advertised, it simply does not WIN by
	// silence (design D5).
	Params int64
	// ActiveParams is the per-token parameter count for a mixture; zero means unreported.
	ActiveParams int64
	// Quant is the quantization level the runtime reported (Q4_K_M, F16).
	// Carried for the operator; nothing selects on it.
	Quant string
	// MaxConcurrent is the per-model ceiling. Zero means the machine
	// declared none, which the load ordering reads as unlimited -- the
	// convention loadRatio already uses.
	MaxConcurrent uint32

	// The four MODALITY flags (epic memql#5137, D4). Each says the machine can
	// serve one of the new call kinds on WorkerService.Stream: vision (chat
	// messages carrying images), transcribe (audio in, text out), speak (text
	// in, audio out) and image (a prompt in, image bytes out).
	//
	// LIKE Tools AND StructuredOutput, THESE ARE RUNTIME CAPABILITIES AS MUCH
	// AS MODEL ONES, which is why they are advertised per machine rather than
	// inferred from the model id: the same weights behind an endpoint that does
	// not implement image parts cannot see, and a catalog entry saying the model
	// has vision does not make that endpoint able to serve it.
	//
	// False is the ZERO VALUE and absence is how a machine says false. An
	// unadvertised modality costs eligibility rather than granting it -- the
	// alternative is a vision turn routed to a machine that cannot see, failing
	// on somebody else's laptop with an error naming nothing.
	Vision   bool
	AudioIn  bool
	AudioOut bool
	ImageGen bool
}

// Attribute keys in the label value.
const (
	attrContext      = "ctx"
	attrStructured   = "structured"
	attrEmbeddings   = "embeddings"
	attrTools        = "tools"
	attrParams       = "params"
	attrActiveParams = "activeparams"
	attrQuant        = "quant"
	attrMax          = "max"

	// The four modality keys (epic memql#5137, D4). Lowercase with no
	// separator, matching what the cockpit emits -- `audioin`, not `audioIn`
	// and not `audio_in`. The spelling is the contract; a mismatch is a machine
	// that advertises a door the engine never sees.
	attrVision   = "vision"
	attrAudioIn  = "audioin"
	attrAudioOut = "audioout"
	attrImageGen = "imagegen"
)

// ParseModelAttributes reads the value of a `model:<id>` label.
//
// An unparseable value yields the zero attributes rather than an error: the
// machine is still offering the model, and refusing to route to it over a
// malformed number would take a working machine out of the fleet for a
// cosmetic reason. The zero value is fail-closed on every capability, so the
// worst a garbled value can do is make a machine ineligible for the prompts
// that need a capability it never legibly claimed.
func ParseModelAttributes(value string) ModelAttributes {
	var a ModelAttributes
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
		case attrTools:
			a.Tools = parseAdvertisedBool(v)
		case attrParams:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				a.Params = n
			}
		case attrActiveParams:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				a.ActiveParams = n
			}
		case attrQuant:
			// Carried verbatim, and bounded: it is an operator-facing
			// string from another process, so it is never parsed and never
			// unbounded.
			if len(v) > MaxQuantLen {
				v = v[:MaxQuantLen]
			}
			a.Quant = v
		case attrMax:
			// Parse at the advertised width: oversized values are malformed,
			// just like negative or nonnumeric attributes, and are ignored.
			if n, err := strconv.ParseUint(v, 10, 32); err == nil && n > 0 {
				a.MaxConcurrent = uint32(n)
			}
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

// parseAdvertisedBool accepts the spellings a machine might send. It is
// permissive in exactly one direction: anything it does not recognise is
// FALSE, so a novel spelling costs eligibility rather than granting it.
func parseAdvertisedBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y":
		return true
	}
	return false
}

// String renders attributes back to the label value, so the cockpit contract
// and the engine's reading of it have exactly one definition.
func (a ModelAttributes) String() string {
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
	if a.Tools {
		parts = append(parts, attrTools+"=1")
	}
	if a.Params > 0 {
		parts = append(parts, fmt.Sprintf("%s=%d", attrParams, a.Params))
	}
	if a.ActiveParams > 0 {
		parts = append(parts, fmt.Sprintf("%s=%d", attrActiveParams, a.ActiveParams))
	}
	if a.Quant != "" {
		parts = append(parts, attrQuant+"="+a.Quant)
	}
	if a.MaxConcurrent > 0 {
		parts = append(parts, fmt.Sprintf("%s=%d", attrMax, a.MaxConcurrent))
	}
	// The four modality flags, last and in this order (epic memql#5137, D4).
	//
	// FALSE IS ABSENCE, never `=0` -- the rule the four flags above already
	// follow, and the one the cockpit implements against. The ORDER is not
	// load-bearing for parsing (ParseModelAttributes is order-independent, and
	// has to be: the cockpit requires byte-identical labels for an unchanged
	// inventory or every reconnect rewrites the registration row) but it is
	// agreed with the cockpit session anyway, so the two renderings can be
	// reconciled later without a fleet-wide relabel.
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

// Satisfies reports whether these attributes meet the prompt's needs, and
// names the miss when they do not. The reason is for the refusal report
// (memql#4682), which lists every machine considered and why each was ruled
// out; it is not a distinct error class.
func (a ModelAttributes) Satisfies(n ModelNeeds) (bool, string) {
	// An embedding model answers nothing but embeddings (memql#6238), whatever
	// else the label claims: the same rule FleetMachine.serves applies to the
	// catalog, and the reason it is "never" is written there.
	if !n.Embeddings && a.Embeddings {
		return false, "model is an embedding model and answers embedding calls only"
	}
	if n.StructuredOutput && !a.StructuredOutput {
		return false, "model does not advertise structured output"
	}
	if n.Embeddings && !a.Embeddings {
		return false, "model does not advertise embeddings"
	}
	if n.Tools && !a.Tools {
		return false, "model does not advertise tool calling"
	}
	// The four modality gates (epic memql#5137, D4). Same shape and same
	// direction as the three above: an unadvertised capability is a REFUSAL to
	// select, not a downgrade. A machine that cannot see is skipped for a vision
	// turn rather than handed one and left to fail with an error that names
	// nothing about capability.
	if n.Vision && !a.Vision {
		return false, "model does not advertise vision"
	}
	if n.AudioIn && !a.AudioIn {
		return false, "model does not advertise audio input (transcription)"
	}
	if n.AudioOut && !a.AudioOut {
		return false, "model does not advertise audio output (speech)"
	}
	if n.ImageGen && !a.ImageGen {
		return false, "model does not advertise image generation"
	}
	if n.MinContextWindow > 0 && a.ContextWindow < n.MinContextWindow {
		if a.ContextWindow == 0 {
			return false, fmt.Sprintf("model advertises no context window (floor %d)", n.MinContextWindow)
		}
		return false, fmt.Sprintf("context window %d is under the floor %d", a.ContextWindow, n.MinContextWindow)
	}
	return true, ""
}

// ModelAttributesFor returns what this machine advertised about a model, and
// whether it advertised it at all.
func (c Candidate) ModelAttributesFor(modelId string) (ModelAttributes, bool) {
	v, ok := c.Labels[workerservice.ModelLabel(modelId)]
	if !ok {
		return ModelAttributes{}, false
	}
	return ParseModelAttributes(v), true
}

// ModelsOffered lists the model ids this machine advertises, sorted so a
// catalog built from several machines is stable.
func (c Candidate) ModelsOffered() []string {
	var out []string
	for k, _ := range c.Labels {
		if id, ok := workerservice.ModelIdFromLabel(k); ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Runtimes lists the runtime names this machine reports (ollama,
// openai-compatible). Reported for the operator's benefit; it steers no
// selection, because two machines serving the same model through different
// runtimes are interchangeable to a caller.
func (c Candidate) Runtimes() []string {
	var out []string
	for k := range c.Labels {
		if strings.HasPrefix(k, workerservice.RuntimeLabelPrefix) {
			if name := strings.TrimPrefix(k, workerservice.RuntimeLabelPrefix); name != "" {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}
