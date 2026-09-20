package router

// Where a call actually RAN, recorded on the decision row (epic memql#5146).
//
// ===========================================================================
// THE FIELD EXISTED, THE ROW WROTE IT, AND NOTHING EVER PUT A VALUE IN IT
// ===========================================================================
// `CallRecord.ExecutionSurface` and `v1:router:call.executionSurface` both
// predate this file. `buildRecord` never assigned the field, so every decision
// row carried an empty string -- and `fleetProvider.LastCall()`, the accessor
// written for exactly this handoff, had NO CALLER anywhere in the tree.
//
// It read as finished from either end. The row has the column, the provider has
// the getter, and the one line between them was missing; nothing failed,
// because an empty string is a value.
//
// It surfaced because the sharing ledger needs it. "Served 41 calls for 3
// people this week" is a fold over the calls that ran on ONE machine, and the
// only thing on a decision row that says which machine is this field. Filtering
// on an always-empty column would have folded to "No calls have run on this
// machine this week." -- a false answer, told to the person who lent the
// hardware, indistinguishable from the true zero.
//
// ===========================================================================
// A STRUCTURAL INTERFACE, DECLARED HERE, ON PURPOSE
// ===========================================================================
// component/router is its own module and pins the root module at a PUBLISHED
// version, so it cannot name a method that only exists in the working tree --
// that is the direction violation the module-boundaries lane catches, and the
// reason the routing-rule renderer is injected from app/ rather than imported.
//
// A structurally-satisfied interface has neither problem. It is declared HERE,
// so nothing is imported; the assertion compiles whatever the published
// component/memql contains; and a provider that does not report a surface
// simply does not satisfy it, which is the correct answer for every vendor
// provider -- a call to Anthropic ran on nobody's machine, and the empty
// string is the true reading rather than a missing one.
type surfaceReporter interface {
	// ExecutionSurface names the machine that served the most recent call,
	// as `fleet:<registrationId>` or `cockpit-app:<appId>`, and "" when the
	// call did not run on anybody's machine.
	ExecutionSurface() string
}

// surfaceOf asks a provider where the call ran.
//
// The empty string is a real answer here and not a failure: it is what every
// vendor provider says, and what a fleet provider says before it has served
// anything. The caller records it verbatim rather than treating it as absent,
// because "ran on MemQL's own path" and "we do not know" would otherwise be
// the same row.
func surfaceOf(p any) string {
	r, ok := p.(surfaceReporter)
	if !ok {
		return ""
	}
	return r.ExecutionSurface()
}

// servedModelReporter is what a provider says about the model that ACTUALLY
// served the most recent call, and at what reasoning effort.
//
// STRUCTURAL, for the reason surfaceReporter above is: component/router pins
// the root module at a PUBLISHED version and cannot name a method that exists
// only in the working tree. A provider that does not implement it simply does
// not answer, which is the correct reading for every vendor provider -- what
// was asked for is what ran, and `model` on the row already says it.
//
// IT IS A REPORT, NOT A REQUEST (design D9). An app may reroute mid-session,
// and an app that ignores an effort flag states none. Copying the request's
// pin here would record as MEASURED something nobody measured, and the gap
// between what was pinned and what ran is the only way to see either.
type servedModelReporter interface {
	// ServedModel names the model the surface reported serving the most
	// recent call with, and the effort it reported running at. Both are ""
	// when it said nothing, which the row records as unknown.
	ServedModel() (model, effort string)
}

// servedModelOf asks a provider what served the call.
//
// Two empty strings is a REAL ANSWER and not a failure: it is what every
// vendor provider says, and what an app says when its harness reported no
// model. The caller records it verbatim rather than filling it in, because
// "the surface did not say" and "it served what we asked for" are different
// facts and only one of them is knowable here.
func servedModelOf(p any) (string, string) {
	r, ok := p.(servedModelReporter)
	if !ok {
		return "", ""
	}
	return r.ServedModel()
}
