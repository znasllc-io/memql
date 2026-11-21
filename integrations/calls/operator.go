package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"

	callrules "github.com/znasllc-io/memql/component/calls"
)

// operator.go -- the four DSL-callable acts an operator takes on the calls capability:
//
//	integration.calls.testKeyCreate  mint a test key           (dsl builtin callsTestKeyCreate)
//	integration.calls.siteEnable     switch a deployable's calls (dsl builtin callsSiteEnable)
//	integration.calls.policySet      set a deployable's limits  (dsl builtin callsPolicySet)
//	integration.calls.mediaServers   list the media servers     (dsl builtin callsMediaServers)
//
// # Who may call
//
// Each builtin declares `execute app:deployables/calls` (owner and developer by role),
// which the engine checks before a handler runs. The part can also be GRANTED through an
// organization's group, and the global capability folds every group's grants together,
// so the capability alone says nothing about WHICH deployables a holder may act on.
//
//   - The three acts that name a deployable -- a test key, the switch, the limits -- are
//     judged against THAT deployable's organization (organizationCapabilityTarget,
//     component/memql), so the part held in one organization acts on no other's. Each
//     handler then reads the deployable UNDER THE CALLER, so a person who cannot see it
//     is refused by name before anything is written, and writes the site's rows as the
//     site's owner.
//   - The media servers are the cluster's, shared by every deployable, so their list
//     admits a cluster owner and nobody else, a check its handler makes.
//
// # A person's acts
//
// Minting a key, switching a deployable's calls and changing its limits are a PERSON's
// decisions, so all three refuse a caller who names nobody (the operator credential,
// the cluster acting, a connector) and any call made inside a run. Minting refuses a
// run above all: a run's journal writes what each statement returned into durable rows,
// and the key would be in them. The media server list is a cluster owner's read, not a
// decision, and asks only that.
//
// # The dispatch checks nothing about the arguments
//
// A builtin's declared arguments are documentation to its caller: a handler gets what
// the call said, so every value is checked here before anything rests on it.
//
// # A refusal is a sentence
//
// The OS shows a refusal as written, so each opens with a capital, says what is wrong
// and what to do, and carries neither a construct's name nor this package's. A failure
// a person cannot act on is worded for them too (failed), and the engine's own words go
// only to the log, as a fixed reason.

// resultConcept is the synthetic node concept these capabilities return: an in-flight
// result, never persisted.
const resultConcept = "integration:calls:result"

// maxLabelLength is the longest name an operator may give a key, counted in characters
// a person sees, so a name in another alphabet is held to the same limit as one in ASCII.
const maxLabelLength = 80

// The reasons a run is refused, each with what to do instead.
const (
	keyInARun    = "A calls key is minted only by a person in Deployables, because a run's journal would keep the key. Add it there."
	switchInARun = "Calls are switched on and off for a deployable only by a person in Deployables, because who can hold calls there is an operator's decision. Switch it there."
	limitsInARun = "A deployable's calls limits are changed only by a person in Deployables, because what a deployable allows is an operator's decision. Change them there."
)

// ---------------------------------------------------------------------------
// a test key
// ---------------------------------------------------------------------------

// handleTestKeyCreate mints a TEST key for a deployable and hands it back once.
//
// EVERY CHECK RUNS BEFORE THE KEY IS MINTED, and a refusal at any of them has minted and
// written nothing. The key is read from the randomness, digested over its sixteen RAW
// bytes under the pepper (so every spelling of one key has one digest), and written as
// the site's owner. The reply is the one place the key exists: it is in no row, no log
// line and no event, and a lost key is replaced, never recovered.
func (i *Integration) handleTestKeyCreate(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := refuseInARun(ctx, keyInARun); err != nil {
		return nil, err
	}
	actor, err := requireCaller(ctx, "A calls key is minted by a person, and this call resolves to no one. Sign in as yourself and try again.")
	if err != nil {
		return nil, err
	}
	siteID := memql.BareShortId(strings.TrimSpace(asString(args["siteId"])))
	if siteID == "" {
		return nil, refuse("Choose a deployable to add the calls key to.")
	}
	label := strings.TrimSpace(asString(args["label"]))
	if utf8.RuneCountInString(label) > maxLabelLength {
		return nil, refuse("Shorten the key's name to %d characters or fewer.", maxLabelLength)
	}
	site, err := i.siteFor(ctx, siteID)
	if err != nil {
		return nil, i.failed("testKeyCreate", siteID, err)
	}
	if site.OwnerUserID == "" {
		return nil, refuse("This deployable has no owner, so it cannot have calls keys: a key belongs to the deployable's owner. Give it an owner, then add the key.")
	}
	// A TEST PERSONA CAN ONLY MINT WHAT IS ITS OWN. The row is written as the site's
	// owner, and a persona may not borrow anybody else (auth.ContextWithUserActor), so
	// the engine would refuse the write whatever came next.
	if auth.IsRoleTestContext(ctx) && actor != site.OwnerUserID {
		return nil, refuse("A test persona can only add a calls key to a deployable it owns, because the key belongs to the deployable's owner. Add it as yourself.")
	}
	pepper, err := i.pepper(ctx)
	if err != nil {
		return nil, i.failed("testKeyCreate", siteID, err)
	}
	raw, key, err := callrules.NewKey(i.random)
	if err != nil {
		return nil, i.failed("testKeyCreate", siteID, err)
	}
	keyID := id.NewShortId()
	row := newKey{ID: keyID, SiteID: site.ID, Digest: callrules.Digest(pepper, raw[:]), Label: label, Kind: "test"}
	if err := i.store.createKey(ctx, site.OwnerUserID, row, i.now()); err != nil {
		return nil, i.failed("testKeyCreate", siteID, err)
	}
	return i.node("testKeyCreate:"+keyID, map[string]any{
		"keyId": keyID,
		// THE ONE TIME THE KEY IS READABLE.
		"key": key,
	})
}

// errNoPepper is the cause behind the refusal of a cluster whose media bootstrap has
// never run: no pepper, so no key was ever minted and none can be looked up. The calls
// service (service.go) asks for it by identity, not by the sentence.
var errNoPepper = errors.New("calls: no key pepper: no media server has been configured")

// pepper is the key pepper, read fresh: the bootstrap writes it, and a node that cached
// its absence would refuse every mint for as long as the cache held.
func (i *Integration) pepper(ctx context.Context) ([]byte, error) {
	stored, err := i.store.resolveSecret(ctx, SecretKeyPepper)
	switch {
	case memql.IsVariableNotFound(err):
		return nil, &refusal{
			sentence: "Calls are not set up on this cluster yet: no media server has been configured, so there is nothing for a key to open. Ask a cluster operator to add one.",
			cause:    errNoPepper,
		}
	case err != nil:
		return nil, err
	}
	return decodePepper(stored)
}

// ---------------------------------------------------------------------------
// the site switch
// ---------------------------------------------------------------------------

// handleSiteEnable turns a deployable's calls capability on or off.
//
// THE WRITE RUNS AS THE PERSON, unstamped (Store.setSiteCalls), so the engine judges it
// as any write of theirs: updateSiteCalls' own gate on the part, the organization gate,
// the site's row tier and the organization's calls part for the change. Nothing here
// widens what they may do; what is checked here is what makes the answer a sentence.
func (i *Integration) handleSiteEnable(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := refuseInARun(ctx, switchInARun); err != nil {
		return nil, err
	}
	if _, err := requireCaller(ctx, "Calls are switched by a person, and this call resolves to no one. Sign in as yourself and try again."); err != nil {
		return nil, err
	}
	siteID := memql.BareShortId(strings.TrimSpace(asString(args["siteId"])))
	if siteID == "" {
		return nil, refuse("Choose a deployable to switch calls on or off for.")
	}
	on, ok := args["on"].(bool)
	if !ok {
		return nil, refuse("Say whether calls should be on or off for this deployable.")
	}
	site, err := i.siteFor(ctx, siteID)
	if err != nil {
		return nil, i.failed("siteEnable", siteID, err)
	}
	// Turning calls OFF needs no owner: it only takes something away.
	if on && site.OwnerUserID == "" {
		return nil, refuse("This deployable has no owner, so calls cannot be turned on: every call's rows belong to the deployable's owner. Give it an owner first.")
	}
	if err := i.store.setSiteCalls(ctx, site.ID, on); err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, organizationRequired):
			// The engine refuses a person's change to an existing record no organization
			// holds, a cluster owner's included (validateOrganizationOwnership). Said as it
			// is, because "did not complete" sends the person to try again, which can
			// never succeed: the deployable needs an organization first.
			return nil, &refusal{sentence: "This deployable belongs to no organization yet, so calls were not switched. Give it an organization first.", cause: err}
		case strings.Contains(msg, memql.CodeCapabilityNotHeld) || strings.Contains(msg, "row-authz") || strings.Contains(msg, organizationForbidden):
			return nil, &refusal{sentence: "You cannot change this deployable, so calls were not switched. Ask whoever manages it.", cause: err}
		}
		return nil, i.failed("siteEnable", siteID, err)
	}
	return i.node("siteEnable:"+site.ID, map[string]any{"siteId": site.ID, "calls": on})
}

// The codes the engine's organization rules open their refusals with
// (component/memql/organization_ownership.go). Matched by prefix, as row-authz is above;
// TestSiteEnableWritesTheSwitchAsThePerson drives organizationRequired through the real
// engine, so a change of wording there fails that test rather than going quiet here.
const (
	organizationRequired  = "organization_required:"
	organizationForbidden = "organization_forbidden:"
)

// siteFor is the deployable an act names, read under the caller, with the refusals
// every act shares: one the caller cannot read, and the cluster's own surfaces.
func (i *Integration) siteFor(ctx context.Context, siteID string) (*siteView, error) {
	site, err := i.store.siteForCaller(ctx, siteID)
	if err != nil {
		return nil, err
	}
	if site == nil {
		// A site this caller cannot read and a site that does not exist are one
		// answer: telling them apart would tell a caller which ids exist.
		return nil, refuse("No deployable you can read has that id. Reload the list of deployables and choose one you can see.")
	}
	if site.SystemOwned {
		return nil, refuse("This deployable is a surface the cluster is managed through, so it cannot hold calls. Use one of your own deployables.")
	}
	return site, nil
}

// ---------------------------------------------------------------------------
// the limits
// ---------------------------------------------------------------------------

// handlePolicySet sets any of a site's calls limits (v1:calls:policy, policy.go) and
// answers the effective limits. A limit not named keeps what it had, and one nobody ever
// set is the shipped default (calls.DefaultPolicy). Durations are whole seconds, the
// concept's unit; the store converts them to and from the rules module's durations.
//
// A TARGETED ACT ON THE SITE, exactly as the switch is: the organization gate has judged
// the part in the site's own organization before this runs, and the site is read here
// under the caller. Every value is held to the cluster's bounds (policyFields), so no
// site can claim more than the cluster allows. The row is the site owner's and is
// written as them.
//
// THE ROW IS READ AND WRITTEN UNDER A GATE, keyed by the site, with a fresh read and a
// version stamped after the one read: two operators changing one site's limits at once
// each keep what they named, and two first writes meet at one row (policyIDFor).
func (i *Integration) handlePolicySet(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := refuseInARun(ctx, limitsInARun); err != nil {
		return nil, err
	}
	actor, err := requireCaller(ctx, "A deployable's calls limits are changed by a person, and this call resolves to no one. Sign in as yourself and try again.")
	if err != nil {
		return nil, err
	}
	siteID := memql.BareShortId(strings.TrimSpace(asString(args["siteId"])))
	if siteID == "" {
		return nil, refuse("Choose a deployable to set the calls limits of.")
	}
	changed := map[string]int{}
	var named []string
	for _, f := range policyFields {
		raw, present := args[f.name]
		if !present || raw == nil {
			continue
		}
		value, ok := wholeNumber(raw)
		if !ok {
			return nil, refuse("The %s limit must be a whole number.", f.name)
		}
		if value < f.min || value > f.max {
			unit := ""
			if f.seconds {
				unit = " seconds"
			}
			return nil, refuse("The %s limit must be between %d and %d%s.", f.name, f.min, f.max, unit)
		}
		changed[f.name] = value
		named = append(named, f.name)
	}
	if len(named) == 0 {
		return nil, refuse("Name at least one limit to change.")
	}
	site, err := i.siteFor(ctx, siteID)
	if err != nil {
		return nil, i.failed("policySet", siteID, err)
	}
	if site.OwnerUserID == "" {
		return nil, refuse("This deployable has no owner, so it cannot have calls limits: the limits belong to the deployable's owner. Give it an owner first.")
	}
	// A TEST PERSONA CAN ONLY SET THE LIMITS OF WHAT IS ITS OWN, for the key's reason:
	// the row is written as the site's owner, and a persona borrows nobody else.
	if auth.IsRoleTestContext(ctx) && actor != site.OwnerUserID {
		return nil, refuse("A test persona can only set the calls limits of a deployable it owns, because the limits belong to the deployable's owner. Set them as yourself.")
	}

	pool, err := memql.ResolveWriteGatePool(i.gateDB, "calls: the calls limits", "the calls plug-in takes it from PluginContext.GateDB")
	if err != nil {
		return nil, i.failed("policySet", siteID, err)
	}
	release, err := memql.AcquireWriteGate(ctx, pool, "calls.policy."+site.ID)
	if err != nil {
		return nil, i.failed("policySet", siteID, err)
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	prior, err := i.store.policyRowFor(fresh, site.OwnerUserID, site.ID)
	if err != nil {
		return nil, i.failed("policySet", siteID, err)
	}
	merged := map[string]int{}
	var priorVersion time.Time
	if prior != nil {
		for name, value := range prior.Values {
			merged[name] = value
		}
		priorVersion = prior.Version
	}
	for name, value := range changed {
		merged[name] = value
	}
	if merged["reportsEnabled"] == 1 && (merged["reportMetadataDays"] < 1 || merged["reportMetadataDays"] > 365) {
		return nil, refuse("Choose an explicit report metadata expiry before enabling reports.")
	}
	effective := policyFromValues(merged)
	// A waiter proves it is still there by polling; an entry that expires sooner than
	// the next poll would expire every waiter.
	if effective.QueueEntryTTL <= effective.QueuePoll {
		return nil, refuse("The queueEntryTtl limit must be longer than queuePoll, or a waiter's place expires before it can poll.")
	}
	policyRowID, err := i.store.savePolicy(fresh, site.OwnerUserID, prior, site.ID, policyFromValues(changed), named, memql.VersionTimeAfter(priorVersion, i.now()))
	if err != nil {
		return nil, i.failed("policySet", siteID, err)
	}
	reply := map[string]any{"policyId": policyRowID, "siteId": site.ID}
	for _, f := range policyFields {
		reply[f.name] = f.get(effective)
	}
	return i.node("policySet:"+policyRowID, reply)
}

// policySetArgsSchema is the advisory schema of callsPolicySet's arguments.
func policySetArgsSchema() map[string]string {
	schema := map[string]string{"siteId": "string (required) -- the deployable the limits are for."}
	for _, f := range policyFields {
		unit := ""
		if f.seconds {
			unit = " seconds,"
		}
		schema[f.name] = fmt.Sprintf("int (optional) --%s from %d to %d.", unit, f.min, f.max)
	}
	return schema
}

// wholeNumber reads an argument that must be a whole number. A decoded JSON number
// arrives as a float64, and one that is not whole, or does not fit, is not one.
func wholeNumber(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		// A value outside int answers "not a whole number" rather than a wrapped one;
		// what fits is then held to the field's bounds, all far inside an int.
		if value := num.Int64OrZero(n); value != 0 || n == 0 {
			return value, true
		}
		return 0, false
	case float64:
		whole, ok := num.WholeInt64(n)
		if !ok {
			return 0, false
		}
		if value := num.Int64OrZero(whole); value != 0 || whole == 0 {
			return value, true
		}
		return 0, false
	case json.Number:
		whole, err := n.Int64()
		if err != nil {
			return 0, false
		}
		if value := num.Int64OrZero(whole); value != 0 || whole == 0 {
			return value, true
		}
		return 0, false
	default:
		return 0, false
	}
}

// ---------------------------------------------------------------------------
// the media servers
// ---------------------------------------------------------------------------

// handleMediaServers lists the active media servers, one row each, by id.
//
// A CLUSTER OWNER'S READ, decided here. The calls part is not enough: it is held by a
// developer's role and can be granted through any organization's group, while the media
// servers are the cluster's, shared by every deployable. So the builtin's capability is
// the surface and this check is the gate.
//
// NEVER A CREDENTIAL, never the name of the secret holding one, and never the control
// address, which only the engine uses (mediaServer.apiUrl).
func (i *Integration) handleMediaServers(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if ac, ok := auth.AccessFromContext(ctx); !ok || ac == nil || !ac.IsClusterOwner() {
		return nil, refuse("The media servers are the cluster's, so only a cluster owner can list them.")
	}
	servers, err := i.store.ActiveMediaServers(ctx)
	if err != nil {
		return nil, i.failed("mediaServers", "", err)
	}
	sort.Slice(servers, func(a, b int) bool { return servers[a].ID < servers[b].ID })
	nodes := make([]memorynodes.MemoryNode, 0, len(servers))
	for _, s := range servers {
		row := map[string]any{
			"id":        s.ID,
			"provider":  s.Provider,
			"publicUrl": s.PublicURL,
			"seats":     s.Seats,
			"state":     s.State,
		}
		if s.Region != "" {
			row["region"] = s.Region
		}
		if !s.Version.IsZero() {
			row["createdAt"] = s.Version.Format(time.RFC3339Nano)
		}
		node, err := i.node("mediaServers:"+s.ID, row)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node...)
	}
	return nodes, nil
}

// ---------------------------------------------------------------------------
// shared by the acts
// ---------------------------------------------------------------------------

// refusal is a sentence a person reads, with the failure behind it if there was one.
// The cause stays reachable for errors.Is and out of the sentence.
type refusal struct {
	sentence string
	cause    error
}

func (r *refusal) Error() string { return r.sentence }
func (r *refusal) Unwrap() error { return r.cause }

func refuse(format string, args ...any) error {
	return &refusal{sentence: fmt.Sprintf(format, args...)}
}

// failed is what a handler returns for whatever stopped an act. A refusal is already
// worded. Anything else is a failure the person cannot act on beyond trying again, so
// it gets a sentence that says so and claims nothing about what landed; the log gets a
// fixed reason and the id, never the engine's own words, which can repeat a statement.
func (i *Integration) failed(act, about string, err error) error {
	var worded *refusal
	if errors.As(err, &worded) {
		return err
	}
	if memql.IsWriteGateBusy(err) {
		return &refusal{sentence: "Someone else is changing this at the same moment. Try again in a moment.", cause: err}
	}
	i.logger.Warn("calls: an operator act failed", "component", pluginName, "act", act, "about", about, "reason", failureReason(err))
	return &refusal{sentence: "That did not complete. Reload to see where it stands, and try again.", cause: err}
}

// failureReason says why an act failed in words a log line can keep.
func failureReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "the request was cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "the request timed out"
	case errors.Is(err, memql.ErrRowVersionChanged):
		return "the row changed between the read and the write"
	default:
		return "the engine did not complete the statement"
	}
}

// refuseInARun refuses an act inside a run, in the words given: a run is DSL acting,
// and every step of one carries a run on its context (component/automations).
func refuseInARun(ctx context.Context, sentence string) error {
	if _, inRun := common.RunFromContext(ctx); inRun {
		return refuse("%s", sentence)
	}
	return nil
}

// requireCaller is the person behind an act, bare. The operator credential, the
// cluster acting, a connector and an anonymous caller name nobody, and are refused in
// the words given rather than admitted under a name that is not a person's.
func requireCaller(ctx context.Context, sentence string) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.IsAnonymousActor() || ac.IsConnector() || auth.ActsForNoPerson(ctx, ac.UserId) {
		return "", refuse("%s", sentence)
	}
	return memql.BareShortId(strings.TrimSpace(ac.UserId)), nil
}

func (i *Integration) node(suffix string, payload map[string]any) ([]memorynodes.MemoryNode, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("calls: marshal result: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        "calls:" + suffix,
		Concept:   resultConcept,
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   raw,
	}}, nil
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
