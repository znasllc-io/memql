package wholesalepack

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// entitlement.go -- THE ENTITLEMENT SEAM (epic memql#5533, issue
// memql#5557).
//
// # Approval grants wholesale prices, and HOW is the client's choice
//
// The pack owns the entitlement STATE -- granted, to whom, against which
// store, revoked when. It does not own the mechanism. That is a NAMED
// ADAPTER the client chooses in its settings, and this pack ships two
// (design record, D4): shopifyB2B, which creates a real B2B company and
// attaches a catalog, and customerTag, which needs no Shopify tier at all.
//
// # STATE IS NEVER INFERRED FROM SHOPIFY
//
// A company that exists over there says nothing about whether THIS pack
// granted it -- a merchant may have created one by hand, an old integration
// may have left one, a company may have been deleted underneath us. Reading
// entitlement back out of somebody else's system would also make an
// operator's own record of what they granted depend on an API call that can
// fail, so "what have I granted?" would stop being answerable exactly when
// Shopify is down.
//
// # A FAILED PUSH DOES NOT UNDO AN APPROVAL
//
// When an adapter reports an error the application STAYS APPROVED and the
// entitlement is written `pending` carrying that error. An approval is a
// decision somebody made; a push that failed is an operational fact about
// somebody else's API, and collapsing the two would mean a Shopify outage
// silently un-approving a merchant's customers. Re-provisioning is calling
// the builtin again, which is a new VERSION of the one entitlement row.
//
// # AVAILABILITY, AND WHY AN UNREADABLE PLAN IS NOT FATAL TO EVERY ADAPTER
//
// v1:shopify:store is readable by a cluster owner or a developer only
// (@rowAuthz(clusterOwner, rankFloor="developer"), Connect Shopify D3). So a
// caller may not be able to read the plan, and what that means is the ADAPTER'S to say rather than
// this file's: shopifyB2B refuses, because it cannot establish the catalog
// ceiling it has to respect; customerTag proceeds, because it needs no plan
// at all. That difference IS what "plan-independent" means, and putting the
// decision in Available() is what keeps it from being a comment.

// Grant is everything an adapter is told about one entitlement.
//
// FLAT VALUES, NO ROW. An adapter is handed what it needs and no handle to
// the graph, so an adapter cannot read a row the caller could not and
// cannot write one outside the pack's own concepts. The pack's state stays
// the pack's.
type Grant struct {
	StoreID       string
	ApplicationID string
	CompanyName   string
	BuyerName     string
	BuyerEmail    string
	// Plan is v1:shopify:store.plan, or "" when this caller cannot read the
	// store row. Available reports what that means for this adapter.
	Plan string
	// Reference is what a previous Provision returned. Empty on the first
	// grant; set on a revoke, which is the only way an adapter knows what
	// to undo.
	Reference string
}

// Outcome is what an adapter reports back.
type Outcome struct {
	// Reference is what the adapter must be handed to undo itself: a
	// Shopify company GID, a customer tag. Opaque to the pack, which never
	// parses it.
	Reference string
	// Detail is one human sentence for an operator reading the row. Never
	// a credential and never a token.
	Detail string
	// CustomerID is the Shopify customer the grant landed on, when the
	// integration says; CustomerCreated is whether the grant had to create it.
	CustomerID      string
	CustomerCreated bool
}

// Caller is the ONLY thing an adapter may do besides compute.
//
// A NAMED CONSTRUCT AND ITS ARGUMENTS, and nothing else. An adapter is not
// handed the engine, a row reader or a connector: it is handed the ability
// to call constructs the DSL already declares, which is exactly the reach a
// .memql body would have. That is what keeps an adapter from reading a row
// its caller could not, from writing outside the pack's concepts, and --
// the one that matters most here -- from importing integrations/shopify.
//
// The wholesale pack must not know Shopify exists. Its Shopify adapters
// reach the connector through @executor builtin NAMES
// (shopifyProvisionWholesale and its siblings), which is the documented
// route from a pack to an integration. A client entitling buyers on some
// other commerce platform writes an adapter against ITS integration's
// builtins and registers it, and nothing in this package changes.
type Caller interface {
	Call(ctx context.Context, construct string, args map[string]any) (map[string]any, error)
}

// Adapter is the seam. Two ship with the pack; a client may register its
// own before the node serves.
type Adapter interface {
	// Name is what a merchant puts in
	// v1:wholesale:wholesaleSettings.entitlementAdapter.
	Name() string
	// Available reports whether this adapter can run against a store on
	// this plan, and says why not when it cannot. An empty plan means the
	// caller could not read the store row.
	Available(plan string) error
	// Provision grants. It must be IDEMPOTENT: the pack calls it again
	// after a failure, and a second call for an entitlement that already
	// exists must report the existing one rather than making a second.
	Provision(ctx context.Context, call Caller, g Grant) (Outcome, error)
	// Revoke takes it back, using g.Reference.
	Revoke(ctx context.Context, call Caller, g Grant) (Outcome, error)
}

// The two adapters this pack ships. A client naming anything else has
// registered it itself.
const (
	AdapterShopifyB2B  = "shopifyB2B"
	AdapterCustomerTag = "customerTag"
)

// AdapterNone is what an entitlement row names when no adapter was ever
// reached: the store had no settings or no route, so nothing was called.
const AdapterNone = "none"

// The causes an entitlement row names when it grants nothing (U2).
const (
	ErrorNoSettings      = "no_settings"
	ErrorNoRoute         = "no_route"
	ErrorScopeMissing    = "scope_missing"
	ErrorPlanCeiling     = "plan_ceiling"
	ErrorCustomerRefused = "customer_refused"
	ErrorShopifyError    = "shopify_error"
)

// The four entitlement states, matching the concept's enum.
const (
	EntitlementPending = "pending"
	EntitlementGranted = "granted"
	EntitlementFailed  = "failed"
	EntitlementRevoked = "revoked"
)

var (
	adapterMu sync.RWMutex
	adapters  = map[string]Adapter{}
)

// RegisterAdapter adds one adapter to the registry.
//
// A DUPLICATE PANICS rather than winning, for the reason
// RegisterShopperForm gives about its own routes: which adapter is live
// must be readable off the declarations, and last-wins makes it depend on
// init order. Every caller is a Register running before the node serves.
func RegisterAdapter(a Adapter) {
	if a == nil {
		panic("wholesalepack.RegisterAdapter: nil adapter")
	}
	name := strings.TrimSpace(a.Name())
	if name == "" {
		panic("wholesalepack.RegisterAdapter: an adapter with no name cannot be chosen " +
			"in settings, so it is a registration nobody can use")
	}
	adapterMu.Lock()
	defer adapterMu.Unlock()
	if _, taken := adapters[name]; taken {
		panic(fmt.Sprintf("wholesalepack.RegisterAdapter: %q is already registered", name))
	}
	adapters[name] = a
}

// AdapterByName resolves a settings value.
//
// The refusal LISTS what is registered, because the caller is a merchant
// who typed a name into a settings field and "unknown adapter" alone is a
// message they cannot act on.
func AdapterByName(name string) (Adapter, error) {
	n := strings.TrimSpace(name)
	adapterMu.RLock()
	defer adapterMu.RUnlock()
	if a, ok := adapters[n]; ok {
		return a, nil
	}
	known := make([]string, 0, len(adapters))
	for k := range adapters {
		known = append(known, k)
	}
	sort.Strings(known)
	if len(known) == 0 {
		return nil, fmt.Errorf("wholesale: no entitlement adapter is registered in this build")
	}
	return nil, fmt.Errorf("wholesale: %q is not a registered entitlement adapter; this "+
		"build has %s", name, strings.Join(known, ", "))
}

// AdapterNames is every registered adapter, for the module inventory and
// the pack guide.
func AdapterNames() []string {
	adapterMu.RLock()
	defer adapterMu.RUnlock()
	out := make([]string, 0, len(adapters))
	for k := range adapters {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// EntitlementRowID derives the one entitlement row id for an application.
//
// ONE PER APPLICATION, so a re-provision after a failure is a new VERSION
// of one logical row rather than a second row a read would have to choose
// between -- and "revoked when" is that version's own createdAt, which is
// why the concept carries no revokedAt.
func EntitlementRowID(applicationID string) string {
	return "wholesale-entitlement-" + slugOf(applicationID)
}

// provisionActor is the system actor provisioning READS as.
//
// The automation that calls these builtins runs as the scheduler's own
// synthetic reader, which owns nothing a merchant's row is readable by -- so
// read under it, the application answers "not readable by this caller" and no
// row is written. The deployment reads instead, by name, and the writes go
// out as the application's merchant (asMerchant), because a row written as a
// system actor is owned by nobody.
const provisionActor = "wholesale-provision"

// provisionEntitlement grants through the store's chosen adapter.
//
// FOR THE STORE'S AUTOMATION ONLY. It reads as the deployment and acts as the
// application's merchant, so a client that could call it could provision any
// application it can name; the refusal comes before anything is read.
func (p *Provider) provisionEntitlement(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireInternal(ctx, "wholesaleProvisionEntitlement"); err != nil {
		return nil, err
	}
	return p.applyEntitlement(ctx, args, false)
}

// revokeEntitlement takes one back through the adapter that granted it. For
// the store's automation only, for the reason provisionEntitlement gives.
func (p *Provider) revokeEntitlement(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := requireInternal(ctx, "wholesaleRevokeEntitlement"); err != nil {
		return nil, err
	}
	return p.applyEntitlement(ctx, args, true)
}

// applyEntitlement is both directions, because they differ in three lines
// and sharing them keeps the state machine in one place.
//
// IT CHECKS NO ORIGIN, and that is deliberate: the two capabilities above
// decide who may reach it. It reads the application, its decisions, the
// settings and the store's plan as the deployment, and calls the adapter and
// writes the entitlement as the application's merchant.
func (p *Provider) applyEntitlement(ctx context.Context, args map[string]any, revoking bool) ([]memorynodes.MemoryNode, error) {
	applicationID := trimmed(args["applicationId"])
	if applicationID == "" {
		return nil, fmt.Errorf("wholesale: applicationId is required")
	}
	sys := auth.ContextWithSystemActor(ctx, provisionActor)
	app, err := p.applicationRow(sys, applicationID)
	if err != nil {
		return nil, err
	}
	storeID := trimmed(app["storeId"])
	if storeID == "" {
		return nil, fmt.Errorf("wholesale: application %q carries no storeId", applicationID)
	}
	merchant, err := p.asMerchant(ctx, app)
	if err != nil {
		return nil, err
	}

	// THE DECISION LOG DECIDES WHETHER THIS IS ALLOWED, not an argument.
	// Provisioning an application nobody approved would grant trade terms
	// on the strength of a caller's say-so; revoking one nobody revoked
	// would leave the pack's state disagreeing with its own history.
	state, err := p.stateOf(sys, applicationID)
	if err != nil {
		return nil, err
	}
	if revoking && state != StateRevoked {
		return nil, fmt.Errorf("wholesale: application %q is %s; an entitlement is revoked "+
			"by recording a revoke decision first, so the history and the state agree",
			applicationID, state)
	}
	if !revoking && state != StateApproved {
		return nil, fmt.Errorf("wholesale: application %q is %s; only an approved "+
			"application has an entitlement to provision", applicationID, state)
	}

	grant := Grant{
		StoreID:       storeID,
		ApplicationID: applicationID,
		CompanyName:   trimmed(app["companyName"]),
		BuyerName:     trimmed(app["applicantName"]),
		BuyerEmail:    trimmed(app["applicantEmail"]),
	}
	owner := trimmed(app["ownerUserId"])
	var adapter Adapter
	adapterName := ""

	if revoking {
		prior, err := p.entitlementRow(sys, applicationID)
		if err != nil {
			return nil, err
		}
		was := ""
		if prior != nil {
			grant.Reference = trimmed(prior["reference"])
			was = trimmed(prior["adapter"])
		}
		// NO ROW, OR A ROW THAT NEVER REACHED A STORE (X17). An approval
		// that granted nothing has nothing to take back, and the decision
		// log already says revoked; the row says so too, and no adapter is
		// called against a store nobody was ever entitled on.
		if prior == nil || was == AdapterNone {
			return p.writeEntitlement(merchant, applicationID, storeID, owner, grant,
				AdapterNone, EntitlementRevoked, Outcome{}, "", "")
		}
		// REVOKED THROUGH THE ADAPTER THAT GRANTED IT, read off the row --
		// not through whatever the settings say TODAY. A merchant who
		// switches adapters must still be able to take back what the old
		// one gave, and the settings value has already moved on.
		if was != "" {
			adapter, err = AdapterByName(was)
			if err != nil {
				return nil, fmt.Errorf("wholesale: entitlement for %q was granted by %q, "+
					"which this build no longer registers, so it cannot be revoked through "+
					"the pack: %w", applicationID, was, err)
			}
			adapterName = was
		}
	}
	if adapter == nil {
		settings, err := p.settingsRow(sys, storeID)
		if err != nil {
			return nil, err
		}
		// A RETURNED ERROR LEAVES NO ROW SAYING WHY NOTHING HAPPENED (spec
		// N3), so a store with no settings, or settings naming no route, is
		// written as a failed entitlement on adapter "none" and answered
		// with a nil error. The merchant's screen reads the row.
		if settings == nil {
			return p.writeEntitlement(merchant, applicationID, storeID, owner, grant,
				AdapterNone, EntitlementFailed, Outcome{}, ErrorNoSettings,
				fmt.Sprintf("wholesale: store %q has no wholesale settings, so no entitlement "+
					"adapter is chosen", storeID))
		}
		adapterName = trimmed(settings["entitlementAdapter"])
		if adapterName == "" {
			return p.writeEntitlement(merchant, applicationID, storeID, owner, grant,
				AdapterNone, EntitlementFailed, Outcome{}, ErrorNoRoute,
				fmt.Sprintf("wholesale: store %q has no entitlement adapter configured; "+
					"set entitlementAdapter in this store's wholesale settings to one of %s",
					storeID, strings.Join(AdapterNames(), ", ")))
		}
		adapter, err = AdapterByName(adapterName)
		if err != nil {
			return nil, err
		}
	}

	// THE PLAN IS READ, NOT ASKED FOR. A caller that supplied its own would
	// be the one deciding whether an adapter is available, which is the
	// check's whole content.
	grant.Plan = p.storePlan(sys, storeID)

	// AVAILABILITY IS PART OF THE ATTEMPT, NOT A GUARD IN FRONT OF IT, and
	// the difference is a row. An earlier version returned this refusal
	// straight to the caller, which contradicted the contract issue
	// memql#5557 sets for this builtin: "a provisioning failure leaves the
	// application approved and the entitlement pending with its error." A
	// returned error leaves NO ROW, so a merchant whose store's plan cannot
	// be read would see an approved application, no wholesale prices, and
	// nothing anywhere saying why -- which is exactly the state the
	// entitlement row exists to prevent. It is folded into applyErr below
	// and recorded like any other refusal.
	applyErr := adapter.Available(grant.Plan)

	var result Outcome
	if applyErr == nil {
		if revoking {
			result, applyErr = adapter.Revoke(merchant, p.caller, grant)
		} else {
			result, applyErr = adapter.Provision(merchant, p.caller, grant)
		}
	}

	entState := EntitlementGranted
	if revoking {
		entState = EntitlementRevoked
	}
	errText, errCode := "", ""
	if applyErr != nil {
		// PENDING, NOT FAILED, BY DEFAULT. The decision stands and the push
		// is what is outstanding, so the row says what is true: somebody
		// decided this and the push has not landed yet. `failed` is for an
		// adapter that REFUSED rather than errored -- a ceiling, a buyer
		// with no customer account, a plan nobody can read -- which will
		// fail identically for ever, and which AdapterRefusal marks.
		//
		// On a REVOKE a `pending` row means the un-grant has not landed.
		// The application is already revoked in the decision log either
		// way; this field is about the push, not about the decision.
		entState = EntitlementPending
		if isAdapterRefusal(applyErr) {
			entState = EntitlementFailed
		}
		errText = applyErr.Error()
		errCode = errorCodeOf(applyErr)
	}

	// THE ROW IS WRITTEN EITHER WAY AND THE ERROR IS NOT RETURNED. A
	// builtin that returned the error would leave no row at all, and the
	// merchant would have an approved application with nothing anywhere
	// saying a push had been attempted and failed. The row carries it.
	return p.writeEntitlement(merchant, applicationID, storeID, owner, grant,
		adapterName, entState, result, errCode, errText)
}

// errorCodeOf names the cause of an adapter error for the entitlement row.
// A refusal carries its own code; a refusal with none is the buyer's
// customer record being refused; anything else is Shopify failing.
func errorCodeOf(err error) string {
	for e := err; e != nil; {
		if r, ok := e.(*AdapterRefusal); ok {
			if c := strings.TrimSpace(r.Code); c != "" {
				return c
			}
			return ErrorCustomerRefused
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	if isAdapterRefusal(err) {
		return ErrorCustomerRefused
	}
	return ErrorShopifyError
}

// asMerchant is ctx acting as the application's merchant: its ownerUserId,
// the owner of the deployable it was submitted through.
//
// REFUSED WHEN THE APPLICATION NAMES NONE, because auth.ContextWithUserActor
// returns ctx unchanged for a blank id, and the write would then go out as
// whoever the context happened to carry -- for an automation, a system actor
// whose rows nobody owns.
func (p *Provider) asMerchant(ctx context.Context, app map[string]any) (context.Context, error) {
	owner := trimmed(app["ownerUserId"])
	if owner == "" {
		return nil, fmt.Errorf("wholesale: application %q names no ownerUserId, so there is no "+
			"merchant to act as", trimmed(app["id"]))
	}
	return auth.ContextWithUserActor(ctx, owner), nil
}

// storePlan reads v1:shopify:store.plan, or "" when this caller cannot.
//
// NOT AN ERROR WHEN IT ANSWERS NOTHING. The store row is readable by a
// cluster owner or a developer only, so an unreadable plan is the ORDINARY
// case for a merchant rather than a fault
// -- and what it means is the adapter's to say, not this function's.
func (p *Provider) storePlan(ctx context.Context, storeID string) string {
	rows, err := p.rowsFor(ctx, "storePlanById", map[string]string{"storeId": storeID})
	if err != nil || len(rows) == 0 {
		return ""
	}
	return trimmed(rows[0]["plan"])
}

// entitlementRow reads the current entitlement for an application.
func (p *Provider) entitlementRow(ctx context.Context, applicationID string) (map[string]any, error) {
	rows, err := p.rowsFor(ctx, "entitlementForApplication",
		map[string]string{"applicationId": applicationID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// writeEntitlement persists the one entitlement row for this application.
//
// THROUGH THE MUTATION, for the reason mutations.memql gives at length: a
// builtin's returned nodes are the expression's value and are never
// persisted. ownerUserId is the application's merchant, passed explicitly
// and written as that merchant: a row owned by anybody else is invisible to
// the merchant who granted it.
func (p *Provider) writeEntitlement(ctx context.Context, applicationID, storeID, ownerUserID string, g Grant,
	adapterName, state string, result Outcome, errCode, errText string) ([]memorynodes.MemoryNode, error) {
	reference := result.Reference
	if reference == "" {
		// KEEP THE PRIOR REFERENCE ON A FAILURE. An adapter that errored
		// may still have created the object it was told to; dropping what
		// it had told us last time would leave nothing to revoke.
		reference = g.Reference
	}
	if err := p.write(ctx, "writeEntitlementRow", map[string]any{
		"entitlementId": EntitlementRowID(applicationID),
		"ownerUserId":   ownerUserID,
		"applicationId": applicationID,
		"storeId":       storeID,
		"buyerEmail":    g.BuyerEmail,
		"adapter":       adapterName,
		"state":         state,
		"reference":     reference,
		"errorCode":     errCode,
		"error":         errText,
		// EVERY FIELD EXPLICIT, empty and false included: a write at an
		// existing id merges into the current version, so an omitted field
		// would carry the attempt before it.
		"shopifyCustomerId": result.CustomerID,
		"customerCreated":   result.CustomerCreated,
	}); err != nil {
		return nil, err
	}
	return confirm("wholesale:entitlement:written", map[string]any{
		"applicationId": applicationID,
		"storeId":       storeID,
		"adapter":       adapterName,
		"state":         state,
		"reference":     reference,
		"errorCode":     errCode,
		"error":         errText,
		"detail":        result.Detail,
	})
}

// AdapterRefusal is a failure that will repeat identically for ever.
//
// THE PACK RECORDS THESE DIFFERENTLY AND THAT IS THE WHOLE REASON THE TYPE
// EXISTS. A store below Plus that already holds three catalogs will hold
// three catalogs tomorrow; a customer who has no account on the store will
// have none until somebody invites them. Retrying either is how a queue
// stops draining -- which is propagate.go's own dead-letter rule, applied
// one tier up. So a refusal is written `failed`, and everything else is
// written `pending` and may be provisioned again.
type AdapterRefusal struct {
	Adapter string
	Reason  string
	// Code is the cause the entitlement row names (one of the Error*
	// constants); empty means the buyer's customer record was refused.
	Code string
}

func (e *AdapterRefusal) Error() string {
	return fmt.Sprintf("wholesale: %s refused: %s", e.Adapter, e.Reason)
}

// Refused reports that this will fail identically for ever.
func (e *AdapterRefusal) Refused() bool { return true }

// isAdapterRefusal reports whether err is final rather than transient.
func isAdapterRefusal(err error) bool {
	for err != nil {
		type refusable interface{ Refused() bool }
		if r, ok := err.(refusable); ok && r.Refused() {
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

// engineCaller is the production Caller: it renders a construct call and
// runs it through the engine under the caller's own actor.
type engineCaller struct {
	engine memql.IntegrationEngineAccess
}

// Call runs one named BUILTIN.
//
// ONE NODE OR NOTHING. Every construct an adapter calls is a builtin, and a
// builtin replies with exactly one node; a reply with more would mean the
// adapter had called something else, and picking the first would hide that.
func (e *engineCaller) Call(ctx context.Context, construct string, args map[string]any) (map[string]any, error) {
	if e == nil || e.engine == nil {
		return nil, fmt.Errorf("wholesale: the pack has no engine handle, so %q cannot be called", construct)
	}
	result, err := e.engine.Execute(ctx, renderBuiltinCall(construct, args))
	if err != nil {
		return nil, fmt.Errorf("wholesale: %s: %w", construct, err)
	}
	rows := memql.MaterializeRows(result)
	if len(rows) == 0 {
		return nil, fmt.Errorf("wholesale: %s answered no rows", construct)
	}
	return rows[0], nil
}

// renderBuiltinCall writes the invocation an adapter's construct becomes.
//
// THE `builtin ` KEYWORD IS LOAD-BEARING AND IT IS EASY TO LOSE. A bare
// `name(args)` is the MUTATION form; a builtin is `builtin name(args)` and a
// query is `query name(args)`. component/memql.ShopperCallPrefix is the
// engine's own statement of exactly this, and the bff renders every declared
// shopper route through it -- so this function asks that function rather than
// spelling the keyword again, and the two cannot drift.
//
// Losing it is the kind of mistake a fake Caller cannot catch: every test in
// this package would go on passing while provisioning failed at runtime
// against a construct the engine could not resolve. renderBuiltinCall exists
// as a named function so the rendered TEXT is assertable.
func renderBuiltinCall(construct string, args map[string]any) string {
	names := make([]string, 0, len(args))
	for k := range args {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(memql.ShopperCallPrefix(memql.ShopperReadKindBuiltin))
	b.WriteString(construct)
	b.WriteByte('(')
	for i, k := range names {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(renderLiteral(args[k]))
	}
	b.WriteByte(')')
	return b.String()
}
