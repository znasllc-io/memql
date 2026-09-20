package memql

// The `SubscriptionApp` provider type: a signed-in Claude Code or Codex on one
// of the user's own machines, as an INFERENCE DOOR (epic memql#5096, task
// memql#5100, design D3).
//
// THE INVERSION IS THE POINT. Everywhere else in this tree MemQL is the agent
// and a model is its provider. Here the app is the agent -- it has its own
// loop, its own tools, its own judgement -- and MemQL is the thing it calls
// back into over MCP. So this provider serves the surfaces where MemQL hands
// over a prompt and takes back an answer (chat, and structured chat where the
// harness supports a response schema) and DELIBERATELY NOT MemQL's own
// tool-calling turns: a tool-calling turn is MemQL driving, and driving an app
// that is itself driving produces two agents fighting over one conversation.
//
// WHY IT LOOKS LIKE THE FLEET PROVIDER. It is the same shape for the same
// reason: what exists depends on which machines are awake and which apps a
// person has signed into, so there is ONE base provider and the doors are
// resolved from a live registration read at SELECTION time. A static child
// would be a claim this file cannot keep.
//
// WHAT IS DIFFERENT, AND IT IS ONE THING: the money. A fleet call spends
// electricity; an app call spends a subscription the user already pays for.
// Both are zero dollars to MemQL, so both are excluded from the dollar
// ceiling and included in the loop caps -- a runaway loop on a subscription is
// still a runaway loop, and it is somebody's monthly quota.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/core/common"
)

// AppProviderType is the @type annotation value on the base provider.
const AppProviderType = "SubscriptionApp"

// AppProviderName is the base provider's registry name.
const AppProviderName = "app"

// AppReferencePrefix is how a policy names an app door:
// `@fallback("app:claude-code")`.
//
// IT IS THE SAME WORD THE ROUTING LABEL USES, on purpose (design D10). A
// policy naming `app:claude-code` and a machine advertising the label
// `app:claude-code` are talking about one thing. The two never MEET in code --
// a provider reference resolves through this registry, a label lives on a
// registration -- so the id set behind both is pinned by a test rather than by
// a shared lookup.
const AppReferencePrefix = "app:"

// AppWildcardId is what follows `app:` to mean ANY runnable app, in the
// owner's own order. It is NOT a member of the closed runnable set: it is a
// selector over that set, which is why airoute.IsRunnableApp refuses it.
const AppWildcardId = "*"

// AppWildcard names ANY runnable app, in the owner's own order.
const AppWildcard = AppReferencePrefix + AppWildcardId

// ErrAppUnavailable is what an app call returns when no machine can serve it.
var ErrAppUnavailable = errors.New("no machine can run this app right now")

// AppRefusalCode is the stable tag for a refused app call. It sits beside
// RefusalCodeNoLocalModel in the same vocabulary an operator reads across
// the park card, the log line and the row.
const AppRefusalCode = "no_app_available"

// AppMachine is one machine that could run an app.
type AppMachine struct {
	RegistrationId string
	Name           string
	DisplayName    string
	Online         bool
	// Harness is how this machine drives the app ("claude-headless",
	// "codex-app-server", "codex-mcp"), empty when the cockpit reported no
	// descriptor.
	Harness string
	// StructuredResult and FollowUps come from the machine's descriptor.
	// Both DEFAULT TRUE when no descriptor was reported: absent is "this
	// cockpit predates the field", not a declared no, and reading silence as
	// a refusal would take structured answers away from every machine that
	// has not upgraded.
	StructuredResult bool
	FollowUps        bool
	// Subscription is what the app REPORTED about itself: unknown, none or
	// present. Never inferred.
	Subscription string
	// LocalStream is true when THIS replica holds the machine's stream. A
	// machine on a sibling replica is skipped rather than failed: the
	// app-session envelope has no cross-node forward yet (design section 8).
	LocalStream bool
}

// AppDoor is one app id and the machines behind it.
type AppDoor struct {
	AppId    string
	Machines []AppMachine
}

// Runnable reports whether at least one machine can carry a session right now.
func (d AppDoor) Runnable() bool {
	for _, m := range d.Machines {
		if m.Online && m.LocalStream {
			return true
		}
	}
	return false
}

// SupportsStructured reports whether any runnable machine's harness can
// return a structured final answer.
func (d AppDoor) SupportsStructured() bool {
	for _, m := range d.Machines {
		if m.Online && m.LocalStream && m.StructuredResult {
			return true
		}
	}
	return false
}

// AppCallRequest is one turn handed to an app.
type AppCallRequest struct {
	// ActingUserId scopes the call to that user's machines. EMPTY IS SYSTEM
	// WORK and reaches nothing: unlike the fleet, there is no shared-app
	// opt-in, because a session runs under a per-run credential whose
	// subject is a PERSON and there is no person to name.
	ActingUserId string
	AppId        string
	Messages     []common.ChatMessage
	// Schema is set for a structured call. Its presence is what makes the
	// call require a harness that can return a structured final answer.
	Schema  *common.StructuredSchema
	Purpose string
	RunId   string
	StepId  string

	// Model is the model a policy PINNED with `app:<id>:<model>` (design D8).
	// EMPTY IS NO PIN, and the app runs at its own default.
	Model string
	// Level is the call's LEVEL, one of core/airoute's closed four. It rides
	// AppSessionStart.level, and THE COCKPIT owns the translation into the
	// app's own knobs -- the knob names are the app's, and only the machine
	// knows which app, at which version, is installed. Empty means no level
	// was named, which is what every session did before the field existed.
	Level string
	// Inputs are Library artifact ids the cockpit pulls into the session
	// workspace before the run starts.
	Inputs []string
}

// AppCallResult is the answer.
type AppCallResult struct {
	Content string
	// Model and Effort are what the APP REPORTED serving this turn with
	// (epic memql#5391, design D9), never what was asked for. Empty means it
	// did not say, which the decision row records as unknown.
	Model  string
	Effort string
	// Usage is what the APP reported about its own spend, never inferred.
	Usage AppUsage
	// ExecutionSurface names where the call ran, in the
	// `app:<appId>@<registrationId>` form the ledger stores.
	ExecutionSurface string
	MachineLabel     string
	// Billing is "subscription" when the app reported a subscription and
	// "unknown" when it said nothing. It is never "metered" from here: MemQL
	// was not billed for a call it did not make.
	Billing string
}

// AppUsage is what the app REPORTED. Known=false means it said nothing, which
// the ledger records as `unknown` rather than as a free call.
type AppUsage struct {
	InputTokens  int64
	OutputTokens int64
	Known        bool
}

// AppInference is the contract an agent-tagged build fills in. It mirrors
// FleetInference for the reason stated at the top of fleet_provider.go: app
// sessions travel over the WorkerService stream, which terminates on the agent
// node, so the code that can place one is behind `//go:build agent` while this
// package is linked into every binary.
type AppInference interface {
	// Doors returns the apps this user could reach right now, sorted by id.
	// A user with no machines gets an empty list, not an error.
	Doors(ctx context.Context, actingUserId string) ([]AppDoor, error)
	// Call runs one turn through an app session. ErrAppUnavailable when no
	// machine could serve it.
	Call(ctx context.Context, req AppCallRequest) (AppCallResult, error)
	// AppOrder is the owner's preferred app order, from their delegation
	// policy. Nil when they have none, which is most users.
	AppOrder(ctx context.Context, actingUserId string) ([]string, error)
}

// SetAppInference installs the implementation. Called once during cluster
// wiring on a build that has a worker service; every other build leaves it nil
// and reports an unavailable door.
func (r *ProviderRegistry) SetAppInference(a AppInference) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.apps = a
}

// AppInferenceInstalled reports whether this node can open app sessions at
// all, which distinguishes "you have not signed in anywhere" from "the node
// answering this request has no worker service".
func (r *ProviderRegistry) AppInferenceInstalled() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.apps != nil
}

// AppDoors returns the doors open to an acting user.
func (r *ProviderRegistry) AppDoors(ctx context.Context, actingUserId string) ([]AppDoor, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.RLock()
	a := r.apps
	r.mu.RUnlock()
	if a == nil {
		return nil, nil
	}
	doors, err := a.Doors(ctx, actingUserId)
	if err != nil {
		return nil, err
	}
	sort.Slice(doors, func(i, j int) bool { return doors[i].AppId < doors[j].AppId })
	return doors, nil
}

// SplitAppReference decomposes an app door reference into the app id and the
// MODEL PIN a policy may write after it (`app:<id>:<model>`, design D8).
//
// The wildcard answers ("*", "", true): `app:*` names any signed-in app and
// carries no pin, because a model name belongs to one app. The parser refuses
// `app:*:<model>` at load, so a pinned wildcard cannot reach here.
func SplitAppReference(name string) (appId, model string, ok bool) {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, AppReferencePrefix) {
		return "", "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(name, AppReferencePrefix))
	if rest == "" {
		return "", "", false
	}
	appId, model, _ = strings.Cut(rest, ":")
	appId = strings.TrimSpace(appId)
	model = strings.TrimSpace(model)
	if appId == "" {
		return "", "", false
	}
	return appId, model, true
}

// IsAppReference reports whether a provider name refers to an app door, and
// returns the app id. The wildcard returns "*".
//
// It answers the APP ID and NOT the model pin: `app:claude-code:sonnet` is a
// door on claude-code with a model asked for, and a caller resolving the door
// must look up "claude-code". SplitAppReference is the one that returns both.
func IsAppReference(name string) (string, bool) {
	appId, _, ok := SplitAppReference(name)
	return appId, ok
}

// IsAppWildcard reports whether a reference names ANY runnable app.
func IsAppWildcard(name string) bool {
	return strings.TrimSpace(name) == AppWildcard
}

// appEntry synthesizes the registry entry for `app:<appId>` and, since epic
// memql#5391, for `app:<appId>:<model>` -- the model a policy PINNED.
//
// entryName is the reference an AUTHOR WROTE, and it is what the entry is named
// after rather than a re-composition from appId: the decision record has to
// say what the policy said, and `app:claude-code` where the policy wrote
// `app:claude-code:claude-opus-5` would hide the pin from the one reader who
// needs to see it.
//
// Resolved in EntryForUser rather than at load, for the reason fleetEntry
// states about its own subject: an app that nobody has signed into must not
// refuse boot, and signing in must not need a reload.
func (r *ProviderRegistry) appEntry(ctx context.Context, actingUserId, appId, model, entryName string) (*ProviderConfigEntry, bool) {
	r.mu.RLock()
	a := r.apps
	r.mu.RUnlock()

	wildcard := appId == AppWildcardId
	cfg := ProviderConfig{
		Name:  entryName,
		Type:  AppProviderType,
		Model: appId,
	}
	client := &appProvider{registry: r, appId: appId, model: model, actingUserId: actingUserId, wildcard: wildcard}
	entry := &ProviderConfigEntry{Config: cfg, Client: client}
	if a == nil {
		entry.err = fmt.Errorf("this node has no app sessions installed")
		return entry, true
	}

	doors, err := a.Doors(ctx, actingUserId)
	if err != nil {
		entry.err = err
		return entry, true
	}
	for _, d := range doors {
		if !wildcard && d.AppId != appId {
			continue
		}
		if d.Runnable() {
			entry.Available = true
			return entry, true
		}
	}
	if wildcard {
		entry.err = fmt.Errorf("no app is allowed, signed in and online on a machine this replica holds")
		return entry, true
	}
	entry.err = fmt.Errorf("no machine has %s allowed, signed in and online on a stream this replica holds", appId)
	return entry, true
}

// appProvider is the client behind an `app:<appId>` entry.
type appProvider struct {
	registry *ProviderRegistry
	appId    string
	// model is the model a policy PINNED with `app:<id>:<model>` (design D8).
	// EMPTY IS NO PIN, and the app runs at its own default -- which is every
	// call that came through `app:*` or a bare `app:<id>`.
	model        string
	actingUserId string
	wildcard     bool

	// level is the call's LEVEL, bound per RESOLUTION rather than held on the
	// registry entry (epic memql#5391, design D8). The entry is per (user,
	// reference) and is shared by every call that resolves to it; a level is
	// one call's, so binding it on the entry would let a `fast` turn run at
	// whatever the last `reasoning` turn asked for.
	level string

	lastMu      sync.Mutex
	lastSurface string
	lastUsage   AppUsage
	lastBilling string
	lastModel   string
	lastEffort  string
}

// LastCall reports the machine, usage and billing of the most recent call. The
// provider interfaces return a bare string, so the accounting is read back
// rather than returned -- the same shape fleetProvider uses.
// ExecutionSurface reports where the last call ran, for the router's decision
// row (epic memql#5146). See the fleet provider's for the seam's shape.
func (p *appProvider) ExecutionSurface() string {
	if p == nil {
		return ""
	}
	p.lastMu.Lock()
	defer p.lastMu.Unlock()
	return p.lastSurface
}

func (p *appProvider) LastCall() (surface string, usage AppUsage, billing string) {
	if p == nil {
		return "", AppUsage{}, ""
	}
	p.lastMu.Lock()
	defer p.lastMu.Unlock()
	return p.lastSurface, p.lastUsage, p.lastBilling
}

func (p *appProvider) inference() AppInference {
	if p == nil || p.registry == nil {
		return nil
	}
	p.registry.mu.RLock()
	defer p.registry.mu.RUnlock()
	return p.registry.apps
}

func (p *appProvider) call(ctx context.Context, req AppCallRequest) (AppCallResult, error) {
	a := p.inference()
	if a == nil {
		return AppCallResult{}, fmt.Errorf("%w: this node has no app sessions installed", ErrAppUnavailable)
	}
	req.AppId = p.appId
	if strings.TrimSpace(req.Model) == "" {
		req.Model = p.model
	}
	if strings.TrimSpace(req.Level) == "" {
		req.Level = p.level
	}
	req.ActingUserId = p.actingUserId
	if strings.TrimSpace(req.ActingUserId) == "" {
		req.ActingUserId = actingUserFromContext(ctx)
	}

	if p.wildcard {
		chosen, err := p.resolveWildcard(ctx, a, req)
		if err != nil {
			return AppCallResult{}, err
		}
		req.AppId = chosen
	}

	// THE GUARDS, with the dollar figure at zero. Same four checks, same
	// shared state, no bill: MemQL was not charged for a call that ran
	// inside somebody's own subscription. The CALL still counts against the
	// loop caps, which is the half that matters here -- a runaway loop on a
	// subscription is still a runaway loop, and it is somebody's quota.
	if err := GuardLocalModelCall(ctx, AppCallFingerprint(req)); err != nil {
		return AppCallResult{}, err
	}

	res, err := a.Call(ctx, req)
	if err != nil {
		return res, err
	}
	p.lastMu.Lock()
	p.lastSurface = res.ExecutionSurface
	p.lastUsage = res.Usage
	p.lastBilling = res.Billing
	p.lastModel = res.Model
	p.lastEffort = res.Effort
	p.lastMu.Unlock()
	return res, nil
}

// WithLevel binds one resolution's level without changing the registry client
// or another resolution, the way fleetProvider.WithMinContextTokens binds a
// floor -- and for the same reason: the entry is shared and a level is one
// call's. Constructing a fresh provider also keeps the per-call bookkeeping
// and its mutex independent, which is what lets ServedModel answer about THIS
// call rather than about whichever finished last.
func (p *appProvider) WithLevel(level string) any {
	if p == nil {
		return p
	}
	return &appProvider{
		registry:     p.registry,
		appId:        p.appId,
		model:        p.model,
		actingUserId: p.actingUserId,
		wildcard:     p.wildcard,
		level:        strings.TrimSpace(level),
	}
}

// ServedModel reports what the APP said it served the most recent turn with,
// for the router's decision row (epic memql#5391, design D9).
//
// The same structural seam ExecutionSurface rides, and for the same reason:
// component/router pins this module at a published version. Two empty strings
// means the app said nothing, which the row records as unknown -- never the
// app id, never the level, never a guess.
func (p *appProvider) ServedModel() (string, string) {
	if p == nil {
		return "", ""
	}
	p.lastMu.Lock()
	defer p.lastMu.Unlock()
	return p.lastModel, p.lastEffort
}

// resolveWildcard picks which app an `app:*` call runs.
//
// The owner's delegationPolicy.appOrder wins, then the engine's own closed
// order. A structured call additionally requires a harness that can return a
// structured answer, so a fleet whose only signed-in app cannot do that
// refuses the structured turn rather than answering it in prose -- the caller
// is about to parse the reply.
func (p *appProvider) resolveWildcard(ctx context.Context, a AppInference, req AppCallRequest) (string, error) {
	doors, err := a.Doors(ctx, req.ActingUserId)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrAppUnavailable, err)
	}
	order, err := a.AppOrder(ctx, req.ActingUserId)
	if err != nil {
		// A policy read that failed must not decide the app; the engine's
		// own order is the honest degrade.
		order = nil
	}

	considered := map[string]string{}
	for _, d := range orderDoors(doors, order) {
		switch {
		case !d.Runnable():
			considered[d.AppId] = "not allowed, signed in and online on a machine this replica holds"
		case req.Schema != nil && !d.SupportsStructured():
			considered[d.AppId] = "its harness on this machine cannot return a structured answer"
		default:
			return d.AppId, nil
		}
	}
	return "", &AppUnavailable{AppId: "*", Considered: considered, Total: len(doors)}
}

// orderDoors ranks the doors: the owner's preference for the ids it names,
// then the engine's own closed order (which is alphabetical and stable, so
// every replica agrees with no shared state).
func orderDoors(doors []AppDoor, preference []string) []AppDoor {
	out := make([]AppDoor, len(doors))
	copy(out, doors)

	rank := make(map[string]int, len(preference))
	for i, id := range preference {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, seen := rank[id]; !seen {
			rank[id] = i
		}
	}
	prefRank := func(d AppDoor) int {
		if r, ok := rank[d.AppId]; ok {
			return r
		}
		return len(preference) + 1
	}
	sort.SliceStable(out, func(i, j int) bool {
		if ra, rb := prefRank(out[i]), prefRank(out[j]); ra != rb {
			return ra < rb
		}
		return out[i].AppId < out[j].AppId
	})
	return out
}

// AppCallFingerprint derives the loop-breaker key for an app call. It hashes
// what makes two calls the same call -- the app and the exact conversation --
// exactly as FleetCallFingerprint does, and for the same reason: the purpose,
// the run id and a timestamp all vary across a genuine loop and would make
// every repetition look novel.
func AppCallFingerprint(req AppCallRequest) string {
	h := sha256.New()
	h.Write([]byte(req.AppId))
	for _, m := range req.Messages {
		h.Write([]byte{0})
		h.Write([]byte(m.Role))
		h.Write([]byte{1})
		h.Write([]byte(m.Content))
	}
	if req.Schema != nil {
		h.Write([]byte{2})
		h.Write([]byte(req.Schema.Name))
		h.Write(req.Schema.Schema)
	}
	return "app:" + hex.EncodeToString(h.Sum(nil))
}

// Call implements AIProvider -- the bare prompt form.
func (p *appProvider) Call(ctx context.Context, prompt string) (any, error) {
	res, err := p.call(ctx, AppCallRequest{
		Messages: []common.ChatMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return nil, err
	}
	return res.Content, nil
}

// CallChat implements common.ChatAIProvider.
func (p *appProvider) CallChat(ctx context.Context, messages []common.ChatMessage) (string, error) {
	res, err := p.call(ctx, AppCallRequest{Messages: messages})
	if err != nil {
		return "", err
	}
	return res.Content, nil
}

// CallChatStructured implements common.ChatStructuredProvider.
//
// The schema is the app's OWN structured-output option (`claude --json-schema`,
// the Codex app-server turn), handed to the harness rather than described in
// the prompt -- so a harness that cannot honour it refuses instead of
// answering prose the caller is about to parse.
func (p *appProvider) CallChatStructured(ctx context.Context, messages []common.ChatMessage, schema common.StructuredSchema) (string, error) {
	res, err := p.call(ctx, AppCallRequest{Messages: messages, Schema: &schema})
	if err != nil {
		return "", err
	}
	return res.Content, nil
}

// The tool-calling surfaces are DELIBERATELY ABSENT, and their absence is
// enforced by the router: resolveChain skips a chain entry that does not
// implement the modality it needs, so a tool turn walks past every app door
// without a special case. See the file header for why -- the app is the agent
// on this path, and MemQL is its tool provider over MCP.
var (
	_ AIProvider                    = (*appProvider)(nil)
	_ common.ChatAIProvider         = (*appProvider)(nil)
	_ common.ChatStructuredProvider = (*appProvider)(nil)
)

// AppUnavailable is the typed refusal: no machine could run the app, WITH the
// apps considered and why each was ruled out. It is the app-door twin of
// FleetUnavailable and reads in the same grammar, because an operator looking
// at a parked run should not have to learn two vocabularies for "no door
// opened".
type AppUnavailable struct {
	AppId string
	// Considered maps app id -> the reason it was ruled out.
	Considered map[string]string
	// Total is how many doors were looked at before filtering, which
	// separates "you have signed into nothing" from "none of your two fit".
	Total int
	// LastError is the failure of the last app actually attempted.
	LastError string
}

// Code is the stable machine-readable tag.
func (e *AppUnavailable) Code() string { return AppRefusalCode }

func (e *AppUnavailable) Error() string {
	if e == nil {
		return ErrAppUnavailable.Error()
	}
	var b strings.Builder
	if e.AppId == "*" {
		fmt.Fprintf(&b, "%s: no app door is open", AppRefusalCode)
	} else {
		fmt.Fprintf(&b, "%s: no machine can run %s", AppRefusalCode, e.AppId)
	}
	if e.Total == 0 {
		b.WriteString(" (no machine reports an app this engine drives)")
	}
	for _, k := range e.consideredKeys() {
		fmt.Fprintf(&b, "; %s: %s", k, e.Considered[k])
	}
	if e.LastError != "" {
		fmt.Fprintf(&b, "; last attempt: %s", e.LastError)
	}
	return b.String()
}

func (e *AppUnavailable) consideredKeys() []string {
	keys := make([]string, 0, len(e.Considered))
	for k := range e.Considered {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Unwrap makes errors.Is(err, ErrAppUnavailable) true.
func (e *AppUnavailable) Unwrap() error { return ErrAppUnavailable }

// AsMap renders the refusal for a park card. The list is a SLICE so the order
// a person reads is the order every reader gets.
func (e *AppUnavailable) AsMap() map[string]any {
	if e == nil {
		return nil
	}
	considered := make([]map[string]any, 0, len(e.Considered))
	for _, k := range e.consideredKeys() {
		considered = append(considered, map[string]any{"app": k, "reason": e.Considered[k]})
	}
	out := map[string]any{
		"code":         AppRefusalCode,
		"app":          e.AppId,
		"appsTotal":    e.Total,
		"appsRuledOut": considered,
	}
	if e.LastError != "" {
		out["lastError"] = e.LastError
	}
	return out
}

// AppRefusal builds the typed refusal by asking the doors what is open, so the
// report and the decision read the same source.
func (r *ProviderRegistry) AppRefusal(ctx context.Context, actingUserId, appId string) *AppUnavailable {
	out := &AppUnavailable{AppId: appId, Considered: map[string]string{}}
	if r == nil {
		return out
	}
	r.mu.RLock()
	a := r.apps
	r.mu.RUnlock()
	if a == nil {
		out.Considered["(this node)"] = "this node has no app sessions installed"
		return out
	}
	doors, err := a.Doors(ctx, actingUserId)
	if err != nil {
		out.LastError = err.Error()
		return out
	}
	out.Total = len(doors)
	for _, d := range doors {
		if appId != "*" && d.AppId != appId {
			continue
		}
		switch {
		case len(d.Machines) == 0:
			out.Considered[d.AppId] = "no machine reports it"
		case !d.Runnable():
			out.Considered[d.AppId] = "not allowed, signed in and online on a machine this replica holds"
		default:
			out.Considered[d.AppId] = "runnable, but not eligible for what this call needs"
		}
	}
	if len(out.Considered) == 0 && appId != "*" {
		out.Considered[appId] = "no machine reports this app at all"
	}
	return out
}
