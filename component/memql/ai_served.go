package memql

// ai_served.go -- reading back which source ANSWERED a routed call.
//
// The router hands back a wrapped client that walks the rest of the route
// when the resolution's pick fails at call time (component/router's
// fallbackWalk), so the source that served can be the second or the third.
// The wrapper reports it after the call; these helpers are how the two
// journaled seams -- the ai() runtime and CallAIStructured -- read it, so the
// journal row, the cache key and the `local` mark all name the source that
// served rather than the one that was picked.

import (
	"strings"

	"github.com/znasllc-io/memql/core/airoute"
)

// servedReporter is what the router's wrapped clients answer after a call.
// Structural, like every other read-back seam between the two modules: a
// client that reports nothing -- a test stand-in, a bare registry client --
// simply does not satisfy it, and the resolution's own pick stands.
type servedReporter interface {
	LastServed() (airoute.Served, bool)
}

// servedBy asks a routed client which source its most recent call ended on.
func servedBy(client any) (airoute.Served, bool) {
	reporter, ok := client.(servedReporter)
	if !ok || reporter == nil {
		return airoute.Served{}, false
	}
	served, ok := reporter.LastServed()
	if !ok || strings.TrimSpace(served.ProviderName) == "" {
		return airoute.Served{}, false
	}
	return served, true
}

// servedLocally reports whether a source is one of the user's own models,
// which the journal records as `served: "local"`: MemQL was not billed for it.
func servedLocally(door, providerName string) bool {
	if door == airoute.DoorLocal {
		return true
	}
	_, isFleet := IsFleetReference(providerName)
	return isFleet
}

// resolutionServed is the resolution a caller hands on once the routed client
// has said which source served: that source, with the decision behind it.
func resolutionServed(served airoute.Served) airoute.Resolution {
	return airoute.Resolution{
		ProviderName: served.ProviderName,
		Vendor:       served.Vendor,
		Model:        served.Model,
		Decision:     served.Decision,
	}
}
