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
	HomeDirectory    string
	NativePlatform   string
	ActionContracts  workerservice.ActionContracts
	RepositoryScopes workerservice.RepositoryScopes
	RegistrationId   string
	MachineId        string // Raw enrollment identity; operator labels cannot replace it.
	Name             string
	DisplayName      string
	// OwnerUserId is always stamped when known: SharedInferenceWorkers reads
	// it from the row; WorkersForOwner stamps the scoped owner so recovery in
	// PlanUserModelWithShared can still attribute a machine if a later shared
	// list row omits the field.
	OwnerUserId  string
	Capabilities []string
	// Labels is the MERGE: the cockpit's `labels` overlaid by the owner's
	// `operatorLabels`, operator side winning (design D3).
	Labels map[string]string
	// SharingMode is the enrolled owner's decision from registration.sharing.
	// Never derive it from a worker or operator label: advertisements cannot
	// volunteer a machine. The owner chooses the audience in Fleet.
	SharingMode string
	// SharedUserIds and SharedGroupIds are who the owner lent the machine to
	// under SharingModePeople (epic memql#5344), empty in every other mode.
	// ServesPerson() is the only way to ask whether they include somebody.
	SharedUserIds  []string
	SharedGroupIds []string
	PausedUserIds  []string
	PausedGroupIds []string
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

// ServesCluster admits only the owner's legacy cluster-wide share.
func (c Candidate) ServesCluster() bool {
	return workerservice.ServesTheCluster(c.SharingMode)
}

// SharingRefusal explains why the saved audience excludes cluster work.
func (c Candidate) SharingRefusal() string {
	return workerservice.SharingRefusal(c.SharingMode)
}

// Sharing is the owner's decision as component/worker reads it.
func (c Candidate) Sharing() workerservice.Sharing {
	return workerservice.Sharing{Mode: c.SharingMode, UserIds: c.SharedUserIds, GroupIds: c.SharedGroupIds, PausedUserIds: c.PausedUserIds, PausedGroupIds: c.PausedGroupIds}
}

// ServesPerson applies the saved audience on every routing path.
func (c Candidate) ServesPerson(p *workerservice.Person) bool {
	return workerservice.ServesPerson(c.Sharing(), p)
}

// PersonRefusal names why the machine does not serve this person, or "".
func (c Candidate) PersonRefusal(p *workerservice.Person) string {
	return workerservice.PersonRefusal(c.Sharing(), p)
}

// SystemRefusal names why the machine does not serve the cluster's own work,
// or "". A machine lent to named people never does (design G3).
func (c Candidate) SystemRefusal() string {
	return workerservice.SystemRefusal(c.Sharing())
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
