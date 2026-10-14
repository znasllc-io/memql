// Package consent implements the per-(user, scope) consent state
// machine that gates every workerHost / workerComputer tool
// dispatch. Lives next to the worker because the gate sits inside
// the worker's dispatcher hot path -- moving it elsewhere would
// either require a network round-trip or expose the predicate to
// every cockpit caller.
//
// The state machine is intentionally tiny: one active window at a
// time, ManagerSnapshot for read-only inspection, single-writer
// transitions via Grant / Revoke. The IPC layer (socket.go) marshals
// external requests onto these primitives; the dispatcher
// (worker/tools/dispatcher.go) calls Allows() in the hot path.
//
// Reference: memql-cockpit#64.
package consent

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// DefaultApprovalTimeout is the wait the worker gives an operator
// before defaulting to deny on a strict-mode per-action approval.
// 30 seconds is the spec's working choice: long enough for an
// operator at the terminal to read the modal and decide, short
// enough that an idle worker doesn't park indefinitely on a call
// the operator has walked away from.
const DefaultApprovalTimeout = 30 * time.Second

// Class names the broad category a (tool, action) pair belongs to.
// Used by Classify() to drive both the gate decision (today: same
// for both -- presence of an active window) and the per-action
// audit stream (today: surfaced verbatim in events for the future
// TUI pane).
//
// ClassObserve is the read-side: fs_read / fs_list / fs_stat /
// http_fetch / workerComputer.{screenshot,cursor_position,
// display_info,capabilities,window_list,wait}.
//
// ClassInteract is the write-side: exec / fs_write / pipeline_step /
// workerComputer.{mouse_click,mouse_down,mouse_up,mouse_move,
// mouse_drag,mouse_scroll,key_type,key_press,key_hold,key_combo,
// window_focus}.
//
// ClassUnknown is the safety net for unknown (tool, action) pairs;
// they're treated as ClassInteract for gate purposes (deny-by-
// default) until the classifier learns them.
type Class string

const (
	ClassObserve  Class = "observe"
	ClassInteract Class = "interact"
	ClassUnknown  Class = "unknown"
)

// Decision is the gate result. Reason is human-readable, suitable
// for the dispatcher to embed in its `consent_required` failure
// message back to the agent.
type Decision struct {
	Allowed bool
	Reason  string
}

// Region is an optional screen-coordinate rectangle attached to a
// strict-mode grant (memql-cockpit#131). When a region is set, a
// strict-mode `workerComputer.mouse_click` whose cursor falls
// INSIDE the rect is admitted WITHOUT the per-action approval gate
// -- the operator pre-authorised that zone of the screen. Clicks
// outside the rect still route through the Allow/Deny modal.
//
// The region is a static screen rect. A window-relative or per-app
// region is future work now that the window-bounds API exists
// (window_list / window_focus, memql-cockpit#167).
// key_type carries no spatial coordinate, so the region
// exemption never applies to it -- typed text stays fully gated
// under strict mode.
type Region struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Contains reports whether the point (px, py) falls inside the
// region. Half-open on the far edges (standard rect containment):
// the top-left corner is inside, the bottom-right corner is not.
// A zero-or-negative-area region contains nothing.
func (r Region) Contains(px, py int) bool {
	if r.W <= 0 || r.H <= 0 {
		return false
	}
	return px >= r.X && px < r.X+r.W && py >= r.Y && py < r.Y+r.H
}

// CursorPoint is the cursor position the dispatcher passes to
// AllowsAt for a region check. Known is false when the caller has
// no position info (headless build, or any non-mouse_click call) --
// an unknown cursor can never be in-region, so it falls through to
// the standard gate.
type CursorPoint struct {
	X     int
	Y     int
	Known bool
}

// Status is a point-in-time snapshot of the manager's state.
// Exposed via the IPC `status` op and the TUI's would-be Workers
// pane.
type Status struct {
	// DirectoryReadsPaused is an explicit local stop, independent of the
	// temporary window. A fresh, scoped owner receipt is still required
	// when false; this field does not grant access to any directory.
	DirectoryReadsPaused bool `json:"directory_reads_paused"`
	// Granted reports whether a window is open at the time of the
	// snapshot. Equivalent to (ExpiresAt.After(now)).
	Granted bool `json:"granted"`
	// ExpiresAt is the absolute time the window closes. Zero when
	// Granted == false.
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// Window is the original duration the grant was issued for.
	// Carried so the TUI / `consent status` can show "expires in
	// 12m of a 1h window" without doing the subtraction itself.
	Window time.Duration `json:"window_ms,omitempty"`
	// Strict reports whether the active window requires per-action
	// approval for the high-risk subset (workerComputer.key_type +
	// workerComputer.mouse_click).
	Strict bool `json:"strict,omitempty"`
	// Region is the optional in-region exemption rect for strict
	// mode (memql-cockpit#131). Nil when no region was set on the
	// grant -- strict mode then gates every high-risk call.
	Region *Region `json:"region,omitempty"`
}

// Manager holds the active-window state machine plus a small
// bounded event channel the IPC layer's WATCH op drains.
//
// Concurrency: Allows / Grant / Revoke / Snapshot are safe under
// arbitrary goroutine contention. The single-writer transitions
// take an exclusive lock; readers take the shared one.
type Manager struct {
	mu                   sync.RWMutex
	directoryReadsPaused bool
	statePath            string
	stateErr             error
	expiresAt            time.Time
	window               time.Duration
	strict               bool
	// region is the optional strict-mode in-region exemption rect.
	// Nil = no region (strict gates every high-risk call).
	region *Region

	// now is the time source. Tests inject a fixed clock; production
	// passes nil and we fall back to time.Now.UTC.
	now func() time.Time

	subsMu sync.Mutex
	subs   []chan Event

	// pendingApprovals is the registry of in-flight strict-mode
	// approval requests. Each entry's channel is single-writer
	// (the dispatch goroutine that registered it) and multi-reader
	// (Approve / Deny / Revoke can all unblock it). The map itself
	// is guarded by approvalsMu. Bounded by concurrent strict-mode
	// dispatches; the dispatcher hot-path serialises per-worker
	// today, so this is small in practice.
	approvalsMu      sync.Mutex
	pendingApprovals map[string]*pendingApproval

	// approvalTimeout is the per-action approval wait. Configurable
	// for tests; production uses DefaultApprovalTimeout.
	approvalTimeout time.Duration

	// home is the cluster this Manager gates, when it belongs to a Homes
	// registry (memql-cockpit#433); "" for a standalone Manager. Every
	// event carries it, and peers says how many homes share the registry,
	// so a refusal can name the home in the command it suggests exactly
	// when the command would be ambiguous without it.
	home  string
	peers func() int
}

// Home is the cluster this Manager gates, or "" for a standalone one.
func (m *Manager) Home() string { return m.home }

// grantCommand is the command a refusal suggests. It names this Manager's
// home when the machine serves more than one cluster, because a grant
// without a home is refused there -- a suggestion that does not work is
// worse than none.
func (m *Manager) grantCommand() string {
	cmd := "memql worker consent grant --window=<duration>"
	if m.home != "" && m.peers != nil && m.peers() > 1 {
		cmd += " --cluster " + m.home
	}
	return cmd
}

// pendingApproval is the registry entry for a single strict-mode
// approval awaiting an operator decision. ch is buffered 1 so
// Approve / Deny / cancelAllPending can deposit a result without
// blocking on the waiting dispatcher goroutine.
type pendingApproval struct {
	id          string
	tool        string
	action      string
	class       Class
	requestedAt time.Time
	ch          chan approvalResult
}

type approvalResult struct {
	allowed bool
	reason  string
}

// NewManager builds a Manager with the live clock. Pass NewManagerWithClock
// in tests to inject a deterministic time source.
func NewManager() *Manager {
	return &Manager{
		now:              func() time.Time { return time.Now().UTC() },
		pendingApprovals: make(map[string]*pendingApproval),
		approvalTimeout:  DefaultApprovalTimeout,
	}
}

// NewManagerWithClock is the test-friendly constructor. Passing
// nil yields the live clock.
func NewManagerWithClock(clock func() time.Time) *Manager {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Manager{
		now:              clock,
		pendingApprovals: make(map[string]*pendingApproval),
		approvalTimeout:  DefaultApprovalTimeout,
	}
}

// SetApprovalTimeout overrides the strict-mode per-action approval
// wait. Tests use this to drive the timeout path deterministically;
// production sticks with DefaultApprovalTimeout via the constructors.
func (m *Manager) SetApprovalTimeout(d time.Duration) {
	m.approvalsMu.Lock()
	defer m.approvalsMu.Unlock()
	m.approvalTimeout = d
}

// Grant opens a consent window of `window` duration starting at
// the manager's current clock. A second Grant within an existing
// window OVERWRITES (extends or shortens) -- the operator's most
// recent decision wins. Returns the absolute expiry time so
// callers can log it.
//
// region is the optional strict-mode in-region exemption rect
// (memql-cockpit#131); pass nil for no region. It's only
// meaningful when strict is true -- a non-strict grant ignores it.
//
// Window <= 0 errors out (use Revoke to close immediately).
func (m *Manager) Grant(window time.Duration, strict bool, region *Region) (time.Time, error) {
	if window <= 0 {
		return time.Time{}, errors.New("consent: window must be positive")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.saveDirectoryPause(false); err != nil {
		return time.Time{}, fmt.Errorf("consent: cannot save access state: %w", err)
	}
	m.directoryReadsPaused = false
	m.stateErr = nil
	exp := m.now().Add(window)
	m.expiresAt = exp
	m.window = window
	m.strict = strict
	// Region is only meaningful under strict mode; drop it on a
	// non-strict grant so Snapshot doesn't advertise a dead rect.
	if strict {
		m.region = region
	} else {
		m.region = nil
	}
	m.broadcast(Event{
		At:        m.now(),
		Kind:      EventGranted,
		ExpiresAt: exp,
		Window:    window,
		Strict:    strict,
		Region:    m.region,
	})
	return exp, nil
}

// Revoke closes any active window and pauses scoped directory reads,
// including after a restart. Always emits a revoked event. A storage
// failure is reported while keeping this process blocked.
//
// In strict mode this also denies every in-flight per-action
// approval -- the operator pulling the kill switch should mean
// no pending strict call can sneak through, even if the modal
// was already on screen and the timeout hadn't fired yet.
func (m *Manager) Revoke() error {
	m.mu.Lock()
	m.directoryReadsPaused = true
	err := m.saveDirectoryPause(true)
	m.stateErr = err
	m.expiresAt = time.Time{}
	m.window = 0
	m.strict = false
	m.region = nil
	m.mu.Unlock()
	// broadcast BEFORE cancelling pending so subscribers see the
	// revoke event first and can prepare to drain their pending
	// queue.
	m.broadcast(Event{At: m.now(), Kind: EventRevoked})
	m.cancelAllPending("consent revoked while strict-mode approval was pending")
	if err != nil {
		return fmt.Errorf("access stopped, but the stop could not be saved; stop Cockpit before restarting it: %w", err)
	}
	return nil
}

// AllowsDirectoryRead is called only AFTER the dispatcher validates the
// engine's scoped receipt. Expiry of a broad control window cannot revoke
// that independent approval. An explicit local stop always can.
func (m *Manager) AllowsDirectoryRead(action string) Decision {
	m.mu.RLock()
	paused, stateErr := m.directoryReadsPaused, m.stateErr
	m.mu.RUnlock()
	dec := Decision{Allowed: true, Reason: "approved directory read"}
	switch {
	case action != "fs_read" && action != "fs_list" && action != "fs_stat":
		dec = Decision{Reason: "directory approval allows only file reads, listings and metadata"}
	case stateErr != nil:
		dec = Decision{Reason: "Cannot verify this machine's saved access state. Check Cockpit locally before retrying."}
	case paused:
		dec = Decision{Reason: fmt.Sprintf("Directory access was stopped on this machine. Run `%s` locally to resume approved access.", m.grantCommand())}
	}
	m.recordDispatch("workerHost", action, Classify("workerHost", action), dec)
	return dec
}

// Allows decides whether a single (tool, action) call may run
// against the host right now. Used by the worker's dispatcher in
// the hot path; concurrent-safe under shared locking.
//
// Returns Decision{Allowed:true,...} when an active window covers
// the call. Otherwise the Reason field carries the operator-
// readable explanation the dispatcher copies into its
// `consent_required` failure message.
//
// Emits a `dispatch` event regardless of outcome so subscribers
// (the Workers pane) can render a live audit tail.
//
// Allows is the no-cursor entry point -- it's equivalent to
// AllowsAt with an unknown cursor. Use it for every call except
// workerComputer.mouse_click; that one should go through AllowsAt
// with the cursor position so the strict-mode region exemption
// (memql-cockpit#131) can fire.
func (m *Manager) Allows(tool, action string) Decision {
	return m.AllowsAt(tool, action, CursorPoint{})
}

// AllowsAt is Allows with a cursor position. The dispatcher passes
// the live cursor for workerComputer.mouse_click so that, under a
// strict grant carrying a region, an in-region click skips the
// per-action approval gate. For every other call the cursor is
// CursorPoint{} (Known=false) and AllowsAt behaves exactly like
// the old Allows.
func (m *Manager) AllowsAt(tool, action string, cursor CursorPoint) Decision {
	class := Classify(tool, action)
	m.mu.RLock()
	exp := m.expiresAt
	strict := m.strict
	region := m.region
	m.mu.RUnlock()

	now := m.now()
	if exp.IsZero() {
		dec := Decision{
			Allowed: false,
			Reason: fmt.Sprintf(
				"computer-use consent has not been granted -- run `%s` on this host before retrying (%s.%s, class=%s)",
				m.grantCommand(), tool, action, class),
		}
		m.recordDispatch(tool, action, class, dec)
		return dec
	}
	if !now.Before(exp) {
		dec := Decision{
			Allowed: false,
			Reason: fmt.Sprintf(
				"consent window expired at %s -- run `%s` to re-open (%s.%s, class=%s)",
				exp.Format(time.RFC3339), m.grantCommand(), tool, action, class),
		}
		m.recordDispatch(tool, action, class, dec)
		return dec
	}
	// Active window covers this call. In strict mode, the high-risk
	// subset (workerComputer.key_type + workerComputer.mouse_click)
	// routes through a per-action approval gate: the dispatcher
	// blocks until an operator clicks Allow / Deny in the Workers
	// pane, or the wait times out (defaults to deny). Other
	// ClassInteract calls (exec, fs_write, mouse_move, key_press)
	// stay admitted by the granted window.
	if strict && isHighRiskAction(tool, action) {
		// Region exemption (memql-cockpit#131): a mouse_click whose
		// cursor falls inside the grant's region is pre-authorised
		// and skips the approval gate. key_type has no cursor so it
		// never qualifies -- isMouseClick gates that. An unknown
		// cursor (headless, or the dispatcher didn't resolve it)
		// also falls through to the gate.
		if region != nil && isMouseClick(tool, action) && cursor.Known && region.Contains(cursor.X, cursor.Y) {
			dec := Decision{
				Allowed: true,
				Reason: fmt.Sprintf(
					"consent granted -- in-region click at (%d,%d) within strict-mode region [%d,%d %dx%d]",
					cursor.X, cursor.Y, region.X, region.Y, region.W, region.H),
			}
			m.recordDispatch(tool, action, class, dec)
			return dec
		}
		dec := m.requestApproval(tool, action, class)
		m.recordDispatch(tool, action, class, dec)
		return dec
	}
	dec := Decision{Allowed: true, Reason: "consent granted"}
	m.recordDispatch(tool, action, class, dec)
	return dec
}

// Snapshot returns a copy of the current state.
func (m *Manager) Snapshot() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.expiresAt.IsZero() || !m.now().Before(m.expiresAt) {
		return Status{Granted: false, DirectoryReadsPaused: m.directoryReadsPaused || m.stateErr != nil}
	}
	return Status{
		DirectoryReadsPaused: m.directoryReadsPaused || m.stateErr != nil,
		Granted:              true,
		ExpiresAt:            m.expiresAt,
		Window:               m.window,
		Strict:               m.strict,
		Region:               m.region,
	}
}

// EventKind names a Manager event.
type EventKind string

const (
	EventGranted           EventKind = "granted"
	EventRevoked           EventKind = "revoked"
	EventDispatch          EventKind = "dispatch"
	EventApprovalRequested EventKind = "approval_requested"
	EventApprovalResolved  EventKind = "approval_resolved"
)

// Event is the audit-tail / state-change record streamed to
// Subscribe() readers.
type Event struct {
	At        time.Time     `json:"at"`
	Kind      EventKind     `json:"kind"`
	Tool      string        `json:"tool,omitempty"`
	Action    string        `json:"action,omitempty"`
	Class     Class         `json:"class,omitempty"`
	Allowed   bool          `json:"allowed,omitempty"`
	Reason    string        `json:"reason,omitempty"`
	ExpiresAt time.Time     `json:"expires_at,omitempty"`
	Window    time.Duration `json:"window_ms,omitempty"`
	Strict    bool          `json:"strict,omitempty"`

	// ApprovalId carries the per-action approval handle on
	// EventApprovalRequested + EventApprovalResolved events. The
	// TUI uses it to correlate Allow/Deny responses back to the
	// blocked dispatcher goroutine.
	ApprovalId string `json:"approval_id,omitempty"`

	// Region carries the strict-mode in-region exemption rect on
	// EventGranted events (memql-cockpit#131). Nil when the grant
	// set no region.
	Region *Region `json:"region,omitempty"`

	// Home is the cluster the event is about (memql-cockpit#433). Empty
	// from a standalone Manager.
	Home string `json:"home,omitempty"`
}

// Subscribe returns a channel that receives Manager events plus a
// cancel func that detaches the subscription. Slow subscribers are
// dropped (channel buffer fills) -- the audit pane is a best-effort
// live tail, not a durable log.
func (m *Manager) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	m.subsMu.Lock()
	m.subs = append(m.subs, ch)
	m.subsMu.Unlock()
	cancel := func() {
		m.subsMu.Lock()
		defer m.subsMu.Unlock()
		for i, c := range m.subs {
			if c == ch {
				m.subs = append(m.subs[:i], m.subs[i+1:]...)
				close(c)
				return
			}
		}
	}
	return ch, cancel
}

// broadcast fans an event out to every subscriber. Slow consumers
// see a drop (default branch) rather than blocking the writer.
// Caller must NOT hold m.mu when calling (we acquire subsMu here).
func (m *Manager) broadcast(e Event) {
	e.Home = m.home
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for _, ch := range m.subs {
		select {
		case ch <- e:
		default:
			// Subscriber too slow; drop. The pane will resync on
			// next poll-derived snapshot.
		}
	}
}

func (m *Manager) recordDispatch(tool, action string, class Class, dec Decision) {
	m.broadcast(Event{
		At:      m.now(),
		Kind:    EventDispatch,
		Tool:    tool,
		Action:  action,
		Class:   class,
		Allowed: dec.Allowed,
		Reason:  dec.Reason,
	})
}

// Classify maps a (tool, action) pair to its consent class.
// Unknown pairs default to ClassInteract for the gate's purpose --
// deny-by-default until the classifier learns them.
func Classify(tool, action string) Class {
	switch strings.ToLower(strings.TrimSpace(tool)) {
	case "workerhost":
		switch strings.ToLower(strings.TrimSpace(action)) {
		case "fs_read", "fs_list", "fs_stat", "http_fetch":
			return ClassObserve
		// pipeline_step (memql#5494) runs a repository's own commands
		// here: interact, made explicit. It is admitted by policy.yaml's
		// pipelines block rather than by a window -- see AdmittedByPolicy
		// -- but its class does not change with who admits it.
		case "exec", "fs_write", "pipeline_step":
			return ClassInteract
		}
	case "workercomputer":
		switch strings.ToLower(strings.TrimSpace(action)) {
		// capabilities is pure self-introspection (build tag +
		// display-server env, memql-cockpit#162): reads nothing
		// sensitive, touches nothing -- observe-class.
		// window_list (memql-cockpit#167) enumerates window
		// metadata without touching anything -- observe-class.
		// wait (memql-cockpit#166) is a bounded sleep: posts no
		// input, reads nothing -- observe-class.
		// cursor_position / display_info read pointer + display
		// metadata without touching anything -- observe-class
		// (matches the memql-side scope.go classification,
		// memql-cockpit#179).
		case "screenshot", "capabilities", "window_list", "wait",
			"cursor_position", "display_info":
			return ClassObserve
		// window_focus (memql-cockpit#167) re-orders the user's
		// desktop (raises a window, steals focus) -- interact-class,
		// made explicit rather than riding the unknown->interact
		// default.
		// key_hold / mouse_down / mouse_up (memql-cockpit#166) post
		// synthetic input -- interact-class.
		// key_combo / mouse_drag / mouse_scroll also post synthetic
		// input -- interact-class, made explicit rather than riding
		// the unknown->interact default (memql-cockpit#179).
		case "mouse_click", "mouse_down", "mouse_up", "mouse_move",
			"mouse_drag", "mouse_scroll",
			"key_type", "key_press", "key_hold", "key_combo",
			"window_focus":
			return ClassInteract
		}
	}
	return ClassUnknown
}

// AdmittedByPolicy reports whether (tool, action) is admitted by the machine
// owner's STANDING consent in policy.yaml rather than by a consent window, so
// the dispatcher does not ask the window about it.
//
// workerHost.pipeline_step (memql#5494) is the one such pair. A CI step
// arrives when a push lands, not while somebody is at the machine to open a
// window, so the owner's consent is the pipelines block -- default-deny, and
// enforced by the step itself before anything runs. Every other pair,
// unknown ones included, keeps asking the window: answering true here for a
// pair whose handler checks no policy would run it on nobody's say-so.
func AdmittedByPolicy(tool, action string) bool {
	return strings.EqualFold(strings.TrimSpace(tool), "workerHost") &&
		strings.EqualFold(strings.TrimSpace(action), "pipeline_step")
}

// isHighRiskAction names the strict-mode per-action approval subset.
// Per the #64 spec these are the actions where a granted-window
// admission isn't tight enough -- typed text exfiltrates the most
// data in the shortest time, and a non-region-confined mouse click
// can fire arbitrary UI affordances. key_hold / mouse_down /
// mouse_up (memql-cockpit#166) join the set because they compose
// into exactly those primitives (a held modifier turns any
// subsequent click into a chord; a bare button-down/up pair IS a
// click) -- leaving them out would let strict mode be bypassed by
// decomposition. Other ClassInteract actions (exec / fs_write /
// mouse_move / key_press) stay admitted by the window's standing
// grant.
//
// Region-confined exemption from the spec is deferred -- the
// codebase has no notion of designated regions today; revisit when
// that concept lands.
func isHighRiskAction(tool, action string) bool {
	if strings.ToLower(strings.TrimSpace(tool)) != "workercomputer" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "key_type", "key_hold", "mouse_click", "mouse_down", "mouse_up":
		return true
	}
	return false
}

// isMouseClick reports whether (tool, action) is the one high-risk
// call the region exemption can apply to. key_type carries no
// cursor coordinate, so the region check is meaningless for it --
// it stays fully gated under strict mode regardless of region.
func isMouseClick(tool, action string) bool {
	return strings.ToLower(strings.TrimSpace(tool)) == "workercomputer" &&
		strings.ToLower(strings.TrimSpace(action)) == "mouse_click"
}

// requestApproval registers a pending strict-mode approval, broadcasts
// the request event for the TUI to render, and blocks until the
// operator clicks Allow / Deny -- or the approval times out (defaults
// to deny). Called from Allows() on the dispatcher hot path.
//
// Concurrency: each call gets its own pending entry + result channel
// so concurrent strict-mode dispatches don't fight over a shared
// signaller. The pending map's mutex covers add / remove / lookup.
func (m *Manager) requestApproval(tool, action string, class Class) Decision {
	id, err := newApprovalId()
	if err != nil {
		// Treat ID-mint failure as deny -- safer than admitting an
		// untracked call we can't correlate a response to.
		return Decision{
			Allowed: false,
			Reason:  fmt.Sprintf("strict mode: failed to mint approval id: %v", err),
		}
	}

	m.approvalsMu.Lock()
	timeout := m.approvalTimeout
	if timeout <= 0 {
		timeout = DefaultApprovalTimeout
	}
	p := &pendingApproval{
		id:          id,
		tool:        tool,
		action:      action,
		class:       class,
		requestedAt: m.now(),
		ch:          make(chan approvalResult, 1),
	}
	m.pendingApprovals[id] = p
	m.approvalsMu.Unlock()

	// Broadcast AFTER the registration is in place so a fast TUI
	// response can't race and find the entry missing.
	m.broadcast(Event{
		At:         m.now(),
		Kind:       EventApprovalRequested,
		Tool:       tool,
		Action:     action,
		Class:      class,
		ApprovalId: id,
	})

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var dec Decision
	select {
	case res := <-p.ch:
		dec = Decision{Allowed: res.allowed, Reason: res.reason}
	case <-timer.C:
		dec = Decision{
			Allowed: false,
			Reason:  fmt.Sprintf("strict-mode approval timed out after %s -- defaulting to deny", timeout),
		}
	}

	// Remove the pending entry on resolution. The double-check
	// guards against a concurrent cancelAllPending that already
	// drained the same id (e.g. Revoke arriving milliseconds
	// before Allow).
	m.approvalsMu.Lock()
	delete(m.pendingApprovals, id)
	m.approvalsMu.Unlock()

	// Broadcast the resolution so the TUI can close the modal
	// even if the operator's response came from a different
	// surface (e.g. CLI `worker consent approve` from another
	// terminal -- not yet shipped but the wire's ready).
	m.broadcast(Event{
		At:         m.now(),
		Kind:       EventApprovalResolved,
		Tool:       tool,
		Action:     action,
		Class:      class,
		Allowed:    dec.Allowed,
		Reason:     dec.Reason,
		ApprovalId: id,
	})

	return dec
}

// Approve resolves the pending approval with the given id as ALLOW.
// Returns an error when the id is unknown -- the TUI should treat
// that as "the request already timed out or was revoked" and clear
// its modal regardless.
func (m *Manager) Approve(id string) error {
	return m.resolvePending(id, approvalResult{
		allowed: true,
		reason:  "approved per-action (strict mode)",
	})
}

// Deny resolves the pending approval with the given id as DENY.
// Same error semantics as Approve.
func (m *Manager) Deny(id string) error {
	return m.resolvePending(id, approvalResult{
		allowed: false,
		reason:  "denied per-action (strict mode)",
	})
}

func (m *Manager) resolvePending(id string, res approvalResult) error {
	m.approvalsMu.Lock()
	p, ok := m.pendingApprovals[id]
	m.approvalsMu.Unlock()
	if !ok {
		return fmt.Errorf("consent: no pending approval with id %q (already resolved, revoked, or timed out)", id)
	}
	select {
	case p.ch <- res:
	default:
		// Channel already had a result deposited (double-resolve
		// race). Treat as no-op; the requesting goroutine has
		// what it needs.
	}
	return nil
}

// cancelAllPending denies every in-flight approval. Called from
// Revoke so the kill switch propagates to strict-mode calls already
// blocked on the per-action gate.
func (m *Manager) cancelAllPending(reason string) {
	m.approvalsMu.Lock()
	pending := make([]*pendingApproval, 0, len(m.pendingApprovals))
	for _, p := range m.pendingApprovals {
		pending = append(pending, p)
	}
	m.approvalsMu.Unlock()
	for _, p := range pending {
		select {
		case p.ch <- approvalResult{allowed: false, reason: reason}:
		default:
		}
	}
}

// PendingApprovals returns a snapshot of currently-pending approval
// requests. The TUI uses this on Watch reconnect to re-render its
// queue without waiting for fresh EventApprovalRequested broadcasts
// (which the worker only emits on registration, not periodically).
func (m *Manager) PendingApprovals() []PendingApprovalInfo {
	m.approvalsMu.Lock()
	defer m.approvalsMu.Unlock()
	out := make([]PendingApprovalInfo, 0, len(m.pendingApprovals))
	for _, p := range m.pendingApprovals {
		out = append(out, PendingApprovalInfo{
			Id:          p.id,
			Tool:        p.tool,
			Action:      p.action,
			Class:       p.class,
			RequestedAt: p.requestedAt,
			Home:        m.home,
		})
	}
	return out
}

// PendingApprovalInfo is the public, copy-safe shape of a pending
// approval. Exposed via Snapshot / PendingApprovals so the TUI never
// touches the underlying channel.
type PendingApprovalInfo struct {
	Id          string    `json:"id"`
	Tool        string    `json:"tool"`
	Action      string    `json:"action"`
	Class       Class     `json:"class"`
	RequestedAt time.Time `json:"requested_at"`
	// Home is the cluster whose call is waiting (memql-cockpit#433).
	Home string `json:"home,omitempty"`
}

// newApprovalId mints a fresh approval handle. 8 random bytes
// (16 hex chars) -- collision-resistant for any realistic pending
// queue while staying short enough to read in a log line.
func newApprovalId() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}
