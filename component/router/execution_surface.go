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

// machineOwnerReporter is what a provider says about WHOSE MACHINE served the
// most recent call.
//
// STRUCTURAL, beside surfaceReporter and for its reason: component/router pins
// the root module at a published version and cannot name a method that exists
// only in the working tree.
//
// IT IS THE FOURTH ACCESSOR-SHAPED GAP IN THIS FILE, and the same shape as the
// first: `v1:router:call.machineOwnerUserId` and `Decision.MachineOwnerUserId`
// both existed, both were documented as "empty until shared team machines
// land", and the machines landed without the one line between them (epic
// memql#5327, design D15). Nothing failed, because an empty string is a value
// -- so no row in any cluster said that one person's call had run on another
// person's hardware, which is the single fact a shared fleet adds to the
// ledger and the one thing the person who lent the machine is entitled to.
//
// EMPTY MEANS "NOT SOMEBODY ELSE'S", not "unknown". A vendor provider does not
// implement this at all; a fleet provider answers empty for a call on the
// caller's OWN machine. Both are the true reading, and folding them together
// is deliberate: the question is whether this call used somebody else's
// hardware, and for both of them the answer is no.
type machineOwnerReporter interface {
	// MachineOwner names the owner of the machine that served the most recent
	// call, and "" when that owner is the caller or there was no machine.
	MachineOwner() string
}

// machineOwnerOf asks a provider whose machine served the call.
func machineOwnerOf(p any) string {
	r, ok := p.(machineOwnerReporter)
	if !ok {
		return ""
	}
	return r.MachineOwner()
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

// billingReporter is what a provider says about WHO PAID for the most recent
// call.
//
// STRUCTURAL, beside the two above and for their reason. A provider that does
// not implement it says nothing, and the row's existing rule applies: an empty
// billing reads as `metered`, the conservative direction, because unattributed
// spend should count against the ceiling rather than disappear into a bucket
// the ceiling cannot see.
//
// IT WAS THE THIRD ACCESSOR WITH NO CALLER. `appProvider.LastCall()` has
// returned a billing value since the app door landed and nothing ever read it,
// so every app-served decision row said `metered` for a call that ran inside
// somebody's own subscription -- the same shape as the ExecutionSurface gap
// memql#5146 found, in the same file, one field along. It matters more now:
// a `session` door runs a whole step on a subscription, and a cost reader
// separating "what we spent" from "what ran somewhere we do not pay" would
// have had every one of them on the wrong side.
type billingReporter interface {
	// Billing is "subscription", "local", "metered" or "" when the provider
	// cannot tell. It is never inferred from anything else.
	Billing() string
}

// billingOf asks a provider who paid. The empty string is a real answer -- it
// is what every vendor provider says, and the row reads it as metered.
func billingOf(p any) string {
	r, ok := p.(billingReporter)
	if !ok {
		return ""
	}
	return r.Billing()
}
