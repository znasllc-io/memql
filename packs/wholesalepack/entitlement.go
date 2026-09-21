package wholesalepack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
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
// v1:shopify:store is @rowAuthz(clusterOwner) and epic memql#5530's D10
// left that tier deliberately UNCHANGED. So a caller may not be able to
// read the plan, and what that means is the ADAPTER'S to say rather than
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
	return "wholesale:entitlement:" + strings.TrimSpace(applicationID)
}

// provisionEntitlement grants through the store's chosen adapter.
func (p *Provider) provisionEntitlement(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return p.applyEntitlement(ctx, args, false)
}

// revokeEntitlement takes one back through the adapter that granted it.
func (p *Provider) revokeEntitlement(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return p.applyEntitlement(ctx, args, true)
}

// applyEntitlement is both directions, because they differ in three lines
// and sharing them keeps the state machine in one place.
func (p *Provider) applyEntitlement(ctx context.Context, args map[string]any, revoking bool) ([]memorynodes.MemoryNode, error) {
	applicationID := trimmed(args["applicationId"])
	if applicationID == "" {
		return nil, fmt.Errorf("wholesale: applicationId is required")
	}
	app, err := p.applicationRow(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	storeID := trimmed(app["storeId"])
	if storeID == "" {
		return nil, fmt.Errorf("wholesale: application %q carries no storeId", applicationID)
	}

	// THE DECISION LOG DECIDES WHETHER THIS IS ALLOWED, not an argument.
	// Provisioning an application nobody approved would grant trade terms
	// on the strength of a caller's say-so; revoking one nobody revoked
	// would leave the pack's state disagreeing with its own history.
	state, err := p.stateOf(ctx, applicationID)
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

	adapterName, err := p.adapterFor(ctx, storeID)
	if err != nil {
		return nil, err
	}
	adapter, err := AdapterByName(adapterName)
	if err != nil {
		return nil, err
	}

	grant := Grant{
		StoreID:       storeID,
		ApplicationID: applicationID,
		CompanyName:   trimmed(app["companyName"]),
		BuyerName:     trimmed(app["applicantName"]),
		BuyerEmail:    trimmed(app["applicantEmail"]),
	}
	// THE PLAN IS READ, NOT ASKED FOR. A caller that supplied its own would
	// be the one deciding whether an adapter is available, which is the
	// check's whole content.
	grant.Plan = p.storePlan(ctx, storeID)

	if revoking {
		prior, err := p.entitlementRow(ctx, applicationID)
		if err != nil {
			return nil, err
		}
		if prior == nil {
			return nil, fmt.Errorf("wholesale: application %q has no entitlement to revoke",
				applicationID)
		}
		grant.Reference = trimmed(prior["reference"])
		// REVOKED THROUGH THE ADAPTER THAT GRANTED IT, read off the row --
		// not through whatever the settings say TODAY. A merchant who
		// switches adapters must still be able to take back what the old
		// one gave, and the settings value has already moved on.
		if was := trimmed(prior["adapter"]); was != "" && was != adapterName {
			adapter, err = AdapterByName(was)
			if err != nil {
				return nil, fmt.Errorf("wholesale: entitlement for %q was granted by %q, "+
					"which this build no longer registers, so it cannot be revoked through "+
					"the pack: %w", applicationID, was, err)
			}
			adapterName = was
		}
	}

	if err := adapter.Available(grant.Plan); err != nil {
		return nil, err
	}

	var (
		result   Outcome
		applyErr error
	)
	if revoking {
		result, applyErr = adapter.Revoke(ctx, p.caller, grant)
	} else {
		result, applyErr = adapter.Provision(ctx, p.caller, grant)
	}

	entState := EntitlementGranted
	if revoking {
		entState = EntitlementRevoked
	}
	errText := ""
	if applyErr != nil {
		// PENDING, NOT FAILED, ON A GRANT. The approval stands and the push
		// is what is outstanding, so the row says what is true: somebody
		// approved this and the grant has not landed yet. `failed` is for
		// an adapter that refused rather than errored -- a distinction the
		// adapters draw with ErrAdapterRefused.
		entState = EntitlementPending
		if isAdapterRefusal(applyErr) {
			entState = EntitlementFailed
		}
		errText = applyErr.Error()
	}

	node, err := p.entitlementNode(applicationID, storeID, grant, adapterName, entState, result, errText)
	if err != nil {
		return nil, err
	}
	// THE ROW IS WRITTEN EITHER WAY AND THE ERROR IS NOT RETURNED. A
	// builtin that returned the error would leave no row at all, and the
	// merchant would have an approved application with nothing anywhere
	// saying a push had been attempted and failed. The row carries it.
	return node, nil
}

// adapterFor reads the store's chosen adapter name.
func (p *Provider) adapterFor(ctx context.Context, storeID string) (string, error) {
	settings, err := p.settingsRow(ctx, storeID)
	if err != nil {
		return "", err
	}
	if settings == nil {
		return "", fmt.Errorf("wholesale: store %q has no wholesale settings, so no "+
			"entitlement adapter is chosen", storeID)
	}
	name := trimmed(settings["entitlementAdapter"])
	if name == "" {
		// NAMED REFUSAL RATHER THAN A SILENT NO-OP. A merchant who approves
		// an application and sees no wholesale prices appear is owed the
		// reason, and "no adapter is configured" is a reason they can act
		// on in one setting.
		return "", fmt.Errorf("wholesale: store %q has no entitlement adapter configured; "+
			"set entitlementAdapter in this store's wholesale settings to one of %s",
			storeID, strings.Join(AdapterNames(), ", "))
	}
	return name, nil
}

// storePlan reads v1:shopify:store.plan, or "" when this caller cannot.
//
// NOT AN ERROR WHEN IT ANSWERS NOTHING. The store row is
// @rowAuthz(clusterOwner) and D10 left that tier unchanged, so an
// unreadable plan is the ORDINARY case for a merchant rather than a fault
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

// entitlementNode builds the one row this capability writes.
func (p *Provider) entitlementNode(applicationID, storeID string, g Grant,
	adapterName, state string, result Outcome, errText string) ([]memorynodes.MemoryNode, error) {
	reference := result.Reference
	if reference == "" {
		// KEEP THE PRIOR REFERENCE ON A FAILURE. An adapter that errored
		// may still have created the object it was told to; dropping what
		// it had told us last time would leave nothing to revoke.
		reference = g.Reference
	}
	payload, err := json.Marshal(map[string]any{
		"applicationId": applicationID,
		"storeId":       storeID,
		"buyerEmail":    g.BuyerEmail,
		"adapter":       adapterName,
		"state":         state,
		"reference":     reference,
		"error":         errText,
		"detail":        result.Detail,
	})
	if err != nil {
		return nil, fmt.Errorf("wholesale: marshal entitlement: %w", err)
	}
	return []memorynodes.MemoryNode{{
		ID:        EntitlementRowID(applicationID),
		Concept:   "v1:wholesale:entitlement",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
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

// renderLiteral writes one argument as MemQL source.
//
// QuoteString FOR EVERY STRING, never Go quoting: it is the engine's own
// literal escaping, and a company name is caller-supplied text that reaches
// this from a public form.
func renderLiteral(v any) string {
	switch t := v.(type) {
	case string:
		return langparser.QuoteString(t)
	case int:
		return strconv.Itoa(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return langparser.QuoteString(asString(v))
	}
}
