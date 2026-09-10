// Package fleetcatalog reads the shared graph projection of worker models.
// It has no worker streams, dispatchers or cross-replica call state.
package fleetcatalog

import (
	"strings"
	"time"

	workerservice "github.com/znasllc-io/memql/component/worker"
)

// Candidate is one of the owner's machines as the router sees it: the row's
// fields, plus the label merge. It is deliberately NOT the registry's *Worker
// -- a candidate may be held by another replica, in which case this node has
// no handle for it at all and dispatch goes over the forward.
type Candidate struct {
	RegistrationId string
	Name           string
	DisplayName    string
	// OwnerUserId is always stamped when known: SharedInferenceWorkers reads
	// it from the row; WorkersForOwner stamps the scoped owner so recovery in
	// PlanUserModelWithShared can still attribute a machine if a later shared
	// list row omits the field.
	OwnerUserId  string
	Capabilities []string
	// Labels is the MERGE: the cockpit's `labels` overlaid by the owner's
	// `operatorLabels`, operator side winning (design D3).
	Labels map[string]string
	// The TWO CONSENTS that let this machine serve somebody other than its
	// owner (epic memql#5146, D6). They are separate fields, and keeping them
	// separate is the design rather than an accident of parsing.
	//
	// SharingMode is the OWNER's, from registration.sharing.mode, set by an act
	// on the Fleet page. InferenceServe is the COCKPIT's, from that machine's
	// own policy.yaml, arriving on the capability descriptor.
	//
	// They replace the `sharedInference` OPERATOR LABEL this field used to be
	// projected from (memql#4676). The label carried one consent where two are
	// needed -- a person may own a machine they are not entitled to volunteer,
	// and a machine may sit somewhere its policy forbids serving strangers --
	// and it carried the owner's half in a map whose sibling is rewritten from
	// the Register message on every reconnect, so the prohibition against
	// reading the MERGE had to be maintained by hand at every reader. A
	// structured field on the row cannot be spoofed by a machine reporting a
	// label of that name, so the prohibition became unnecessary rather than
	// merely documented.
	//
	// ServesCluster() is the only way to ask; neither half alone is consent.
	SharingMode    string
	InferenceServe string
	// Apps is the local-app inventory the cockpit reported, verbatim -- ids
	// this engine cannot drive included, so an operator surface can show an
	// app the engine will never select.
	Apps []workerservice.AppInfo
	// AppDescriptors say HOW each reported app is driven. Only entries whose
	// app id AND harness word this engine knows are stored, so an entry here
	// is one the engine can act on. An app with NO entry is not an app whose
	// harness does neither -- see workerservice.DescriptorFor.
	AppDescriptors []workerservice.AppDescriptor
	// Hardware is what the machine IS, as its cockpit reported it (epic
	// memql#5146, D1). The ZERO VALUE is the absent case and Hardware.Present()
	// is how to ask -- a machine whose cockpit predates the field is not a
	// machine with no memory, and every reader must take it that way.
	//
	// It steers no dispatch. It decides the machine's CLASS, which decides what
	// the machine is RECOMMENDED to pull; eligibility for a call is still the
	// advertised label, which is a fact about a running process where this is a
	// fact about hardware.
	Hardware        workerservice.Inventory
	Concurrency     map[string]uint32
	ActiveCount     int
	ConnectedNodeId string
	LastSelectedAt  time.Time
	LastSeenAt      time.Time
	RevokedAt       time.Time
}

// ServesCluster reports whether BOTH consents say cluster.
//
// It delegates to component/worker rather than comparing here, so there is one
// implementation of "is this machine shared" in the tree. Two would drift, and
// the drift would be a machine serving a stranger's prompt on one code path and
// not on another.
func (c Candidate) ServesCluster() bool {
	return workerservice.ServesTheCluster(c.SharingMode, c.InferenceServe)
}

// SharingRefusal names WHICH consent is missing, for the routing plan's
// rejected map and for the machine page. The two repairs are in different
// places, so one sentence for both would send half the operators to the wrong
// machine.
func (c Candidate) SharingRefusal() string {
	return workerservice.SharingRefusal(c.SharingMode, c.InferenceServe)
}

// Label returns the machine's display label for a card or a log line.
func (c Candidate) Label() string {
	if s := strings.TrimSpace(c.DisplayName); s != "" {
		return s
	}
	return c.Name
}

// SupportsCapability reports whether the machine advertised the capability.
func (c Candidate) SupportsCapability(name string) bool {
	if name == "" {
		return true
	}
	for _, have := range c.Capabilities {
		if have == name {
			return true
		}
	}
	return false
}

func MergeLabels(cockpit, operator map[string]string) map[string]string {
	out := make(map[string]string, len(cockpit)+len(operator))
	for k, v := range cockpit {
		out[k] = v
	}
	for k, v := range operator {
		out[k] = v
	}
	return out
}
