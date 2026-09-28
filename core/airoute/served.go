package airoute

// Served names what ANSWERED a routed call, which is not always what the
// resolution picked.
//
// A resolution names the route's first runnable source. The router's wrapper
// then walks the rest of the route when that source fails at call time -- a
// machine that slept between the two, an app that refused -- so the source
// that served can be the second or the third. Everything that records the call
// (the work journal, the result a caller hands on) must name the one that
// served; naming the resolution's pick would record, as having answered, a
// source that failed.
//
// It is reported by the wrapper after the call, and describes the LAST
// attempt: the source that served on success, the last one tried on failure.
type Served struct {
	// RouterCallId is the v1:router:call row that records this attempt, as a
	// full row id. It is how a journal row reaches the decision record behind
	// it without copying the ledger.
	RouterCallId string

	ProviderName string
	Vendor       string
	Model        string

	// Decision is the resolution's decision, carried to the attempt that
	// served: its Door is that attempt's door, and Considered ends with the
	// line that says it was taken from the fallback route when it was.
	Decision Decision
}
