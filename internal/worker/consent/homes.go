package consent

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Homes is the consent state of every cluster this machine serves: ONE
// MANAGER PER HOME (memql-cockpit#433).
//
// A consent window is an operator saying "tools may run on this machine
// now" -- and on a machine paired with two clusters, a single window
// answered that for both. A window opened because the operator was
// working with cluster A admitted `exec` and `fs_write` dispatched by
// cluster B, which is exactly the crossing a second pairing is supposed
// to rule out: each cluster gets this machine on the terms its owner set
// for THAT cluster. So each home's dispatcher asks its own Manager, and a
// grant names the home it is for.
//
// The kill switch is the exception, and deliberately: `consent revoke`
// with no home closes every window at once. Revoking is the safe
// direction, and a person reaching for it should not first have to
// remember which cluster they opened a window for.
type Homes struct {
	mu       sync.Mutex
	ids      []string
	byID     map[string]*Manager
	now      func() time.Time
	stateDir string
}

// NewHomes builds an empty registry on the live clock.
func NewHomes() *Homes { return NewHomesWithClock(nil) }

// NewPersistentHomes retains explicit local stops across worker restarts.
// Time-limited windows and directory grants are never persisted here:
// the engine freshly checks the owner's grant on every scoped dispatch.
func NewPersistentHomes(stateDir string) *Homes {
	h := NewHomes()
	h.stateDir = stateDir
	return h
}

// NewHomesWithClock is the test-friendly constructor; nil is the live
// clock.
func NewHomesWithClock(clock func() time.Time) *Homes {
	return &Homes{byID: make(map[string]*Manager), now: clock}
}

// For returns the Manager that gates home id, creating it on first use.
// The worker calls it once per home it runs, when it builds that home's
// dispatcher.
func (h *Homes) For(id string) *Manager {
	id = strings.TrimSpace(id)
	h.mu.Lock()
	defer h.mu.Unlock()
	if m, ok := h.byID[id]; ok {
		return m
	}
	m := NewManagerWithClock(h.now)
	m.home = id
	m.peers = h.count
	if h.stateDir != "" {
		m.loadDirectoryPause(h.stateDir, id)
	}
	h.byID[id] = m
	h.ids = append(h.ids, id)
	return m
}

// IDs lists the homes in the order they were registered.
func (h *Homes) IDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.ids...)
}

// count is how many homes the registry holds. A Manager asks it when it
// writes a refusal, to decide whether the command it suggests has to name
// its home.
func (h *Homes) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.ids)
}

// Managers returns every home's Manager, in registration order.
func (h *Homes) Managers() []*Manager {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*Manager, 0, len(h.ids))
	for _, id := range h.ids {
		out = append(out, h.byID[id])
	}
	return out
}

// Error codes a Pick can answer with, so a client can render its own
// sentence -- the CLI knows the window the person typed, and the worker
// does not.
const (
	// CodeHomeRequired: the machine serves several clusters and the
	// request named none.
	CodeHomeRequired = "home_required"
	// CodeUnknownHome: the request named a cluster this worker does not
	// serve.
	CodeUnknownHome = "unknown_home"
	// CodeNoHomes: this worker serves no cluster at all right now.
	CodeNoHomes = "no_homes"
)

// PickError is a Pick that could not name one home.
type PickError struct {
	Code string
	// Homes is every home this worker serves, for the sentence.
	Homes []string
	// Asked is the home the request named, when it named one.
	Asked string
}

func (e *PickError) Error() string {
	switch e.Code {
	case CodeHomeRequired:
		return fmt.Sprintf("this machine serves %d clusters (%s), and consent is per cluster: name the one this is for with --cluster <id>",
			len(e.Homes), strings.Join(e.Homes, ", "))
	case CodeUnknownHome:
		return fmt.Sprintf("this worker does not serve a cluster called %q; it serves: %s", e.Asked, strings.Join(e.Homes, ", "))
	default:
		return "this worker is not serving any cluster right now, so there is no consent to set"
	}
}

// Pick resolves the home a request is for: the one it names, or the only
// one there is. It never guesses between two.
func (h *Homes) Pick(id string) (string, *Manager, error) {
	id = strings.TrimSpace(id)
	h.mu.Lock()
	defer h.mu.Unlock()
	homes := append([]string(nil), h.ids...)
	if id != "" {
		if m, ok := h.byID[id]; ok {
			return id, m, nil
		}
		return "", nil, &PickError{Code: CodeUnknownHome, Homes: homes, Asked: id}
	}
	switch len(homes) {
	case 0:
		return "", nil, &PickError{Code: CodeNoHomes}
	case 1:
		return homes[0], h.byID[homes[0]], nil
	default:
		return "", nil, &PickError{Code: CodeHomeRequired, Homes: homes}
	}
}

// HomeStatus is one home's consent state.
type HomeStatus struct {
	Home   string `json:"home"`
	Status Status `json:"status"`
}

// Statuses snapshots every home, in registration order.
func (h *Homes) Statuses() []HomeStatus {
	h.mu.Lock()
	ids := append([]string(nil), h.ids...)
	byID := make(map[string]*Manager, len(h.byID))
	for k, v := range h.byID {
		byID[k] = v
	}
	h.mu.Unlock()
	out := make([]HomeStatus, 0, len(ids))
	for _, id := range ids {
		out = append(out, HomeStatus{Home: id, Status: byID[id].Snapshot()})
	}
	return out
}

// PendingApprovals collects every home's pending strict-mode approvals.
func (h *Homes) PendingApprovals() []PendingApprovalInfo {
	var out []PendingApprovalInfo
	for _, m := range h.Managers() {
		out = append(out, m.PendingApprovals()...)
	}
	return out
}
