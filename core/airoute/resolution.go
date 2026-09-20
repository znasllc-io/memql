package airoute

// Door names where inference came from. The three values mirror the router's
// own door classification and v1:platform:inferenceStatus.doorsOpen, so one
// vocabulary spans the readiness line, the log, the park card and the decision
// record. They are declared here so a decision can be READ without importing
// the router.
const (
	DoorLocal      = "local"
	DoorApp        = "app"
	DoorFederation = "federation"
	// DoorSession is an app door that took the whole STEP rather than serving
	// one turn (epic memql#5391, design D7).
	//
	// IT IS A FOURTH DOOR RATHER THAN A FLAG ON DoorApp, because
	// v1:router:call.door is what a reader filters on and the two are
	// different answers: `app` is "a signed-in subscription app answered this
	// turn", `session` is "a signed-in subscription app was handed this step
	// and drove its own loop". Folding them would make the whole history of a
	// cluster whose only door is a signed-in Claude Code read as ordinary chat
	// turns.
	DoorSession = "session"
)

// ConsideredEntry is one line of the door report: an entry the chain walk
// passed over, or the one it took, and why.
//
// It is kept on SUCCESS as well as on refusal (design D10). A rule is
// falsifiable only if the decisions it made can be read, and what a chain did
// NOT pick is half of that -- the pre-rules router accumulated exactly this
// report and dropped it with the stack frame the moment an entry won.
type ConsideredEntry struct {
	Entry  string `json:"entry"`
	Door   string `json:"door"`
	Reason string `json:"reason"`
}

// Decision is one resolution in the words the record uses. Every field is
// filled on success AND on refusal, so a park is as legible as a hit.
type Decision struct {
	// Level is what the call asked for, after any rule override.
	Level Level
	// ServedLevel is what actually served. It differs from Level only when
	// Degraded is true. No degradation is silent (design D9).
	ServedLevel Level
	Degraded    bool

	// RequestedLevel is what the CALL declared before a rule overrode it.
	// Equal to Level when no rule raised or lowered it.
	RequestedLevel Level

	// Rule is the name of the rule that matched; Policy the policy it named,
	// after policy: expansion; Door the door the winning entry belongs to.
	Rule   string
	Policy string
	Door   string

	// Considered is the door report, kept on success too.
	Considered []ConsideredEntry

	// Touches is the request's footprint, copied through so a decision can be
	// filtered by what the call was about.
	Touches []string

	// MinContextTokens is the floor this resolution was made against. It sits
	// beside the winning model's window on the record, which is what makes an
	// under-counted estimate diagnosable rather than merely wrong.
	MinContextTokens int

	// MachineOwnerUserId names whose machine served a local call. Empty until
	// epic 4 (shared machines) fills it.
	MachineOwnerUserId string

	// Outcome is "ok" on a resolution and the refusal code on a park.
	Outcome string
}

// OutcomeOK is the Decision.Outcome of a resolution that produced a provider.
const OutcomeOK = "ok"

// Resolution is what the router returns: the entry it picked, and the whole
// decision that led there.
type Resolution struct {
	ProviderName string
	Vendor       string
	Model        string

	// Decision is what gets written to v1:router:call.
	Decision Decision
}
