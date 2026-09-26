package airoute

// Modality is DERIVED from the call, never declared (design D1). It says which
// provider interface must be satisfied, not which model.
type Modality string

const (
	ModalityChat           Modality = "chat"
	ModalityStreamingChat  Modality = "streamingChat"
	ModalityTools          Modality = "tools"
	ModalityStreamingTools Modality = "streamingTools"
	ModalityStructured     Modality = "structured"
	ModalityVision         Modality = "vision"
	ModalityEmbedding      Modality = "embedding"
	ModalitySpeech         Modality = "speech"
	ModalityTranscribe     Modality = "transcribe"
)

// Modalities is the closed set, for validation and for an error message.
func Modalities() []Modality {
	return []Modality{
		ModalityChat, ModalityStreamingChat, ModalityTools, ModalityStreamingTools,
		ModalityStructured, ModalityVision, ModalityEmbedding, ModalitySpeech,
		ModalityTranscribe,
	}
}

// Valid reports whether m is one of the closed set.
func (m Modality) Valid() bool {
	for _, k := range Modalities() {
		if k == m {
			return true
		}
	}
	return false
}

func (m Modality) String() string { return string(m) }

// NeedsTools reports whether this modality is MemQL DRIVING a tool loop.
//
// It is the predicate the app door turns on (design D7). An app is an agent
// that drives itself, so it cannot serve a turn in somebody else's loop -- two
// agents fighting over one conversation. What it CAN do is take the whole
// step, which is what the `session` door is.
//
// EMBEDDINGS ARE NOT HERE and never will be (D10): an embedding must come from
// the embedder of the index it is written into, and no app exposes one.
func (m Modality) NeedsTools() bool {
	return m == ModalityTools || m == ModalityStreamingTools
}

// Needs are the capability floors a provider must clear to serve the call.
//
// Vision, AudioIn, AudioOut and Image are declared here and are consumed from
// epic 3, which gives every modality a local door. This epic sets them only
// where the call plainly has them, and an unset flag is "not required" rather
// than "not measured" -- a floor nobody set must not narrow the chain.
type Needs struct {
	Structured bool
	Tools      bool
	Vision     bool
	AudioIn    bool
	AudioOut   bool
	Image      bool

	// MinContextTokens is the floor an entry's context window must clear.
	//
	// It is NEVER zero on a request that reaches the router. A zero floor
	// admits every entry, and on the decision record it reads exactly like a
	// floor that was measured and cleared -- so "not measured" and "anything
	// will do" would be the same value.
	MinContextTokens int
}

// Call tags a rule may branch on. Tags are OPEN -- an author may set any
// string -- but these are the ones the shipped rules name, so they are
// constants rather than literals at the call sites that set them.
const (
	// TagBackground is a turn no human is waiting on.
	TagBackground = "background"
	// TagBackgroundEscalation is the one stronger continuation the background
	// lane swaps to when the cheap tier is stuck on a turn.
	TagBackgroundEscalation = "backgroundEscalation"
)

// ResolveRequest carries everything a rule may branch on and everything the
// decision record must be able to say. Every call site that reaches a model
// builds one.
type ResolveRequest struct {
	// Level is what the call declares it needs. Required: a request with no
	// level is refused rather than defaulted, because a guessed level is a
	// policy decision nobody wrote.
	Level Level

	// Modality is derived from the call site, not declared by an author.
	Modality Modality

	// Needs are the capability floors. Audio calls have no chat context floor;
	// text-model calls always carry a positive MinContextTokens.
	Needs Needs

	// PromptName is the DSL prompt this call renders, empty for a Go call
	// site that has no prompt. A rule may branch on it.
	PromptName string

	// Tags are call tags a rule may branch on (TagBackground and friends).
	Tags []string

	// Role is the AGENT's role slug. ActorRole is the calling human's cluster
	// role. They are different questions -- an operator watching a
	// non-operator agent is not an operator turn -- and a rule names them
	// separately so it cannot route on who is watching.
	Role      string
	ActorRole string

	// Touches is the call's footprint: a work step's footprint union, or an
	// agent turn's knowledge-domain concept ids. Empty otherwise. A rule
	// matches it with startsWith semantics.
	Touches []string

	// ExplicitProvider pins one registry entry and wins over every rule
	// (design D2). A prompt's @defaultProvider rides this field, and is still
	// refused at load when it names a policy.
	ExplicitProvider string

	// Attribution, unchanged in meaning from the pre-rules router.
	RequestId string
	UserId    string
	AgentId   string
	Partition string

	// CallerKind says WHAT KIND of caller made this call, so that an empty
	// UserId is an answer rather than a gap (memql#5581).
	//
	// It is one of component/auth's CallerKinds -- user / system / connector /
	// anonymous / unattributed -- carried as a plain string because this
	// package imports nothing but the standard library and must stay nameable
	// from every module in the workspace. It is DERIVED from the context where
	// the request is built and never supplied by a caller; the router fills it
	// from the same derivation when a call site left it empty.
	//
	// NOTHING ROUTES ON IT. No rule key branches on the caller kind and none
	// may: it exists so a decision record can say who caused the call, and a
	// rule that read it would turn a piece of evidence into an input.
	CallerKind string

	// RunId and StepId name the WORK STEP this call serves, when it serves
	// one.
	//
	// They are the hinge of design D7. An app door resolved for a tool-needing
	// call takes the whole step as a session subrun, and a session is opened
	// FROM a step -- so a call that carries none is refused at resolution
	// rather than resolved to a door with nothing to hand over. A bare Go
	// model call with tools has no step, and that is a fact about the call
	// site rather than a failure of anybody's fleet.
	RunId  string
	StepId string

	// CloudConsent is one person's explicit yes for THIS call, after they
	// were shown the refusal. Nothing in the router can set it.
	CloudConsent bool
}

// HasTag reports whether the request carries tag.
func (r ResolveRequest) HasTag(tag string) bool {
	for _, t := range r.Tags {
		if t == tag {
			return true
		}
	}
	return false
}
