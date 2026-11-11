package wholesalepack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// settings.go -- ONE STORE'S WHOLESALE SETTINGS: who sets a store up, who
// changes it afterwards, and what a reader is told.
//
// # Two doors, two kinds of caller
//
// SETTING A STORE UP is the cluster's operators' (or trusted server code's):
// wholesaleSetSettings creates the row, refusing everyone else before it reads.
// The wholesale part does not open that door, so a person on the store's side
// cannot open a program the operators have not hosted.
//
// CHANGING A STORE ONCE IT IS SET UP is the store's own people's, and the
// operators': wholesaleUpdateSettings, gated by the part and judged by the
// organization gate against the store's site, merges a change into the row the
// operators made, and refuses a store that has none.
//
// Both write at internal origin, which the row gate does not owner-check, so
// neither relies on the row's tier: each establishes its caller before it reads
// or writes, and neither ever moves the row to whoever wrote.

// ChangedWhileEditingSentence is the refusal of a save that lost its fence: the
// row changed between the read the change was merged into and the write.
const ChangedWhileEditingSentence = "These settings changed while you were editing. Reload and try again."

// NotSetUpSentence is the refusal of a change to a store with no settings row.
// The OS shows it as written.
const NotSetUpSentence = "This store's wholesale program is not set up yet. The cluster's operators set it up."

// maxStaffEmail is the longest address the store's people may name.
const maxStaffEmail = 320

// SettingsRowID derives the one settings row id for a store.
//
// DERIVED RATHER THAN GENERATED, so the row is a singleton per store and a
// second write is a new VERSION of one logical row rather than a second row
// the read would have to choose between. reviewspack.SettingsRowID is the
// same decision for the same reason.
//
// IT SLUGS THE ID AS GIVEN, so the two forms of one store id -- bare, and
// v1:shopify:store:<id> -- derive two ids. Every read and write here resolves
// the store's ONE row whichever form the caller named (storeSettingsRows), and
// a new row is written at the BARE id's (newSettingsRowID).
func SettingsRowID(storeID string) string {
	return "wholesale-settings-" + slugOf(storeID)
}

// newSettingsRowID is where a store's first settings row is written: at the
// bare store id's settings id, the form the shopper path, the OS's binding and
// every row set up from them hold.
func newSettingsRowID(storeID string) string {
	return SettingsRowID(bareID(strings.TrimSpace(storeID)))
}

// settingsRowIDOf is the id a settings row is held at: its own, else the id
// its storeId derives, else where a new row for storeID goes.
func settingsRowIDOf(row map[string]any, storeID string) string {
	if id := bareID(trimmed(row["id"])); id != "" {
		return id
	}
	if held := trimmed(row["storeId"]); held != "" {
		return SettingsRowID(held)
	}
	return newSettingsRowID(storeID)
}

// settingsStoreForms is every form of a store id a settings row may be held
// under, the bare form first.
func settingsStoreForms(storeID string) []string {
	bare := bareID(strings.TrimSpace(storeID))
	var out []string
	seen := map[string]bool{}
	for _, f := range []string{bare, "v1:shopify:store:" + bare, strings.TrimSpace(storeID)} {
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// storeSettingsRows is every settings row of a store, whichever form of its id
// it was written under: at each form's derived id, the bare form's first, then
// any held under another id whose storeId field names a form. Each row once.
func (p *Provider) storeSettingsRows(ctx context.Context, storeID string) ([]map[string]any, error) {
	var out []map[string]any
	seen := map[string]bool{}
	add := func(rows []map[string]any) {
		for _, row := range rows {
			if id := settingsRowIDOf(row, storeID); !seen[id] {
				seen[id] = true
				out = append(out, row)
			}
		}
	}
	forms := settingsStoreForms(storeID)
	isForm := map[string]bool{}
	for _, form := range forms {
		isForm[form] = true
	}
	for _, form := range forms {
		rows, err := p.rowsFor(ctx, "wholesaleSettingsById", map[string]string{"settingsId": SettingsRowID(form)})
		if err != nil {
			return nil, err
		}
		// A ROW AT THE ID IS THIS STORE'S ONLY WHEN IT SAYS SO: two store ids
		// that slug alike derive one id, and must never share a row.
		var own []map[string]any
		for _, row := range rows {
			if isForm[trimmed(row["storeId"])] {
				own = append(own, row)
			}
		}
		add(own)
	}
	for _, form := range forms {
		rows, err := p.rowsFor(ctx, "wholesaleSettingsForStore", map[string]string{"storeId": form})
		if err != nil {
			return nil, err
		}
		add(rows)
	}
	return out, nil
}

// Settings is one store's settings as every reader here takes them: absent
// settings read as not set up, closed, with no route.
type Settings struct {
	Exists, ApplicationsOpen                                     bool
	EntitlementAdapter, IntroText, StaffEmail, AccountID, SiteID string
}

// EffectiveSettings reads a settings row, or nil for a store with none.
func EffectiveSettings(row map[string]any) Settings {
	if row == nil {
		return Settings{}
	}
	open, _ := row["applicationsOpen"].(bool)
	route := trimmed(row["entitlementAdapter"])
	if route == AdapterNone {
		route = ""
	}
	return Settings{
		Exists:             true,
		ApplicationsOpen:   open,
		EntitlementAdapter: route,
		IntroText:          asString(row["introText"]),
		StaffEmail:         trimmed(row["staffEmail"]),
		AccountID:          trimmed(row["accountId"]),
		SiteID:             trimmed(row["siteId"]),
	}
}

// setWholesaleSettings writes one store's settings row: SETTING A STORE UP.
//
// THE CLUSTER'S OPERATORS AND TRUSTED SERVER CODE ONLY, refused before
// anything is read. The write runs at internal origin, which the row gate
// does not owner-check, so the tier on the settings row protects nothing
// here: without this check any caller who reaches the builtin could open,
// close or re-route any store's program at its derived id.
func (p *Provider) setWholesaleSettings(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := mayConfigureStores(ctx); err != nil {
		return nil, err
	}
	storeID := trimmed(args["storeId"])
	if storeID == "" {
		return nil, fmt.Errorf("wholesale: storeId is required; settings are per store")
	}
	open, ok := args["applicationsOpen"].(bool)
	if !ok {
		return nil, fmt.Errorf("wholesale: applicationsOpen must be a bool")
	}
	adapter := trimmed(args["entitlementAdapter"])
	// AN UNKNOWN ADAPTER IS REFUSED AT SETTINGS TIME, not at approval time.
	// A merchant who typed it wrong learns now, rather than when the first
	// approved application fails to provision -- which is the moment they
	// are least able to act on it.
	if adapter != "" {
		if _, err := AdapterByName(adapter); err != nil {
			return nil, err
		}
	}
	place, err := p.settingsOwner(ctx, storeID, trimmed(args["siteId"]))
	if err != nil {
		return nil, err
	}
	// FENCED ON WHAT IT READ: an existing row on its version, an absent row on
	// "absent". A staff save that lands between the read and this write is not
	// overwritten; the set-up is refused and the operator reloads.
	fenced := memql.ContextWithRowVersionFence(ctx, settingsConcept, place.rowID, place.version)
	if err := p.write(fenced, "setWholesaleSettingsRow", map[string]any{
		"settingsId":         place.rowID,
		"ownerUserId":        place.owner,
		"storeId":            place.storeID,
		"applicationsOpen":   open,
		"entitlementAdapter": adapter,
		"introText":          asString(args["introText"]),
		// EVERY FIELD EXPLICIT: the row's organization and site are the named
		// site's, or kept, or, for a new row, the one site bound to the store;
		// the staff address is the one the row held (none on a new row).
		"accountId":  place.accountID,
		"siteId":     place.siteID,
		"staffEmail": place.staffEmail,
	}); err != nil {
		if errors.Is(err, memql.ErrRowVersionChanged) {
			return nil, errors.New(ChangedWhileEditingSentence)
		}
		return nil, err
	}
	// THE RECEIPT SAYS WHOSE STORE IT NOW IS, so the operator who set it up
	// sees the owner and the site the row was given.
	return confirm("wholesale:settings:written", map[string]any{
		"storeId":            storeID,
		"applicationsOpen":   open,
		"entitlementAdapter": adapter,
		"ownerUserId":        place.owner,
		"siteId":             place.siteID,
		"accountId":          place.accountID,
	})
}

// mayConfigureStores admits internal origin or a cluster operator.
func mayConfigureStores(ctx context.Context) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	if isClusterOperator(ctx) {
		return nil
	}
	return fmt.Errorf("wholesale: a store's wholesale settings are set by the cluster's operators")
}

// isClusterOperator reports whether the caller is one of the cluster's own
// operators.
func isClusterOperator(ctx context.Context) bool {
	ac, ok := auth.AccessFromContext(ctx)
	return ok && auth.IsClusterOperator(ac.Role)
}

// storeLookupActor is the system actor that resolves whose a store's
// settings are: it reads the existing row and the sites bound to the store.
const storeLookupActor = "wholesale-store-lookup"

// settingsPlace is whose a store's settings row is after a write: its owner,
// and the organization and site it is tied to.
type settingsPlace struct {
	owner, accountID, siteID string
	// staffEmail is the address the row already holds, written back whole.
	staffEmail string
	// version is the createdAt of the row read, zero when there was none: the
	// write is fenced on it.
	version time.Time
	// rowID is the row the write goes to -- the store's one row, read whichever
	// form of its id the caller named, or the bare id's for a new row -- and
	// storeID the store id it holds: the existing row's, or the bare form.
	rowID, storeID string
}

// settingsOwner is who owns a store's settings row after this write.
//
// THE EXISTING ROW IS THE STORE'S ONE ROW (storeSettingsRows), read at the id
// each form of the store id derives before any read by its storeId field, and
// the write goes to that row's own id: two store ids that slug alike share the
// id, and a write at an id it did not read could replace a row it never saw.
//
// AND WHAT IT IS TIED TO: with a siteId, the named site's organization and id
// (the site checked to serve the store); otherwise an existing row keeps its
// own (empty on a row from before this version, until the backfill ties it),
// and a new row takes them from the store's site when exactly one is bound to it.
//
// THE EXISTING OWNER IS KEPT. Otherwise the operator who changed a setting
// would become the row's owner, and the merchant's own reads -- the shopper
// path's applicationsOpen among them -- would stop seeing it.
//
// A NEW ROW IS THE STORE'S MERCHANT'S: the named site's owner, or else the one
// owner of the deployables whose serving or preview binding names the store
// (sitesBoundToStore). Two owners are refused rather than chosen between. With
// no bound site, trusted server code acting as a real person names that person;
// an operator's call is refused, because the operator is not the merchant and a
// row they owned would be readable by nobody on the store's side.
func (p *Provider) settingsOwner(ctx context.Context, storeID, siteID string) (settingsPlace, error) {
	sys := auth.ContextWithSystemActor(ctx, storeLookupActor)
	existing, err := p.settingsRowAt(sys, storeID)
	if err != nil {
		return settingsPlace{}, err
	}
	place, err := p.settingsPlaceFrom(ctx, sys, existing, storeID, siteID)
	if err != nil {
		return settingsPlace{}, err
	}
	if existing != nil {
		place.rowID, place.storeID = settingsRowIDOf(existing, storeID), trimmed(existing["storeId"])
	}
	if place.rowID == "" {
		place.rowID = newSettingsRowID(storeID)
		// THE ID A NEW ROW GOES TO MAY BE HELD BY ANOTHER STORE'S ROW: two store
		// ids that slug alike derive one id. That row is not this store's, and
		// is never written over or handed to it.
		held, err := p.settingsRowByID(sys, place.rowID)
		if err != nil {
			return settingsPlace{}, err
		}
		if held != nil {
			return settingsPlace{}, fmt.Errorf("wholesale: store %q's settings would be written at %s, which holds "+
				"store %q's settings; the two store ids slug alike", storeID, place.rowID, trimmed(held["storeId"]))
		}
	}
	if place.storeID == "" {
		place.storeID = bareID(storeID)
	}
	return place, nil
}

// settingsPlaceFrom is whose the row is and what it is tied to after the write
// (settingsOwner's rule), given the existing row read.
func (p *Provider) settingsPlaceFrom(ctx, sys context.Context, existing map[string]any, storeID, siteID string) (settingsPlace, error) {
	var err error
	var named map[string]any
	if siteID != "" {
		// Read as the system: the caller is an operator or trusted code, already
		// admitted, and the site is judged by what it serves, not who can read it.
		if named, err = p.siteForStore(sys, siteID, storeID); err != nil {
			return settingsPlace{}, err
		}
	}
	if existing != nil {
		if owner := trimmed(existing["ownerUserId"]); owner != "" {
			place := settingsPlace{owner: userRef(owner), accountID: trimmed(existing["accountId"]),
				siteID: trimmed(existing["siteId"]), staffEmail: trimmed(existing["staffEmail"]),
				version: rowTime(existing["createdAt"])}
			if place.version.IsZero() {
				return settingsPlace{}, fmt.Errorf("wholesale: store %q's settings carry no version to save against; "+
					"reload and try again", storeID)
			}
			if named != nil {
				place.accountID, place.siteID = trimmed(named["accountId"]), bareID(trimmed(named["id"]))
			}
			return place, nil
		}
	}
	if named != nil {
		owner := userRef(trimmed(named["ownerUserId"]))
		if owner == "" {
			return settingsPlace{}, fmt.Errorf("wholesale: site %q names no owner, so there is no merchant to own "+
				"store %q's wholesale settings", siteID, storeID)
		}
		return settingsPlace{owner: owner, accountID: trimmed(named["accountId"]), siteID: bareID(trimmed(named["id"]))}, nil
	}
	sites, err := p.sitesForStore(sys, storeID)
	if err != nil {
		return settingsPlace{}, err
	}
	owners := map[string]bool{}
	for _, site := range sites {
		if owner := userRef(trimmed(site["ownerUserId"])); owner != "" {
			owners[owner] = true
		}
	}
	switch len(owners) {
	case 1:
		place := settingsPlace{}
		for owner := range owners {
			place.owner = owner
		}
		if len(sites) == 1 {
			place.accountID = trimmed(sites[0]["accountId"])
			place.siteID = bareID(trimmed(sites[0]["id"]))
		}
		return place, nil
	case 0:
		if auth.OriginFromContext(ctx).IsInternal() {
			if ac, ok := auth.AccessFromContext(ctx); ok && !ac.Synthetic && strings.TrimSpace(ac.UserId) != "" {
				return settingsPlace{owner: userRef(ac.UserId)}, nil
			}
		}
		return settingsPlace{}, fmt.Errorf("wholesale: no deployable owned by a person is bound to store %q, so "+
			"there is no merchant to own its wholesale settings; bind the store to its storefront first", storeID)
	}
	return settingsPlace{}, fmt.Errorf("wholesale: the deployables bound to store %q belong to more than one "+
		"owner, so whose wholesale settings these are cannot be told from the store alone", storeID)
}

// storeSettings answers one store's settings and what the caller may do.
//
// THE CALLER REACHED THIS PAST THE ORGANIZATION GATE, holding the wholesale
// part in the site's organization (or as one of the cluster's operators who
// hold it), and the site is checked here to serve the store. So the row is read
// as the system: the caller has been established, and an untied row from
// before this version must still answer whether the store is set up.
func (p *Provider) storeSettings(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := trimmed(args["storeId"])
	if storeID == "" {
		return nil, fmt.Errorf("wholesale: storeId is required; settings are per store")
	}
	if _, err := p.siteForStore(ctx, trimmed(args["siteId"]), storeID); err != nil {
		return nil, err
	}
	row, err := p.settingsRowAt(auth.ContextWithSystemActor(ctx, storeLookupActor), storeID)
	if err != nil {
		return nil, err
	}
	s := EffectiveSettings(row)
	return confirm("wholesale:settings:store", map[string]any{
		"exists":             s.Exists,
		"applicationsOpen":   s.ApplicationsOpen,
		"entitlementAdapter": s.EntitlementAdapter,
		"introText":          s.IntroText,
		"staffEmail":         s.StaffEmail,
		"adapters":           AdapterNames(),
		"may": map[string]any{
			// Everyone past the gate holds the part or is an operator, so a
			// store that is set up may be changed by them.
			"update": s.Exists,
			// A store is set up by the cluster's operators alone.
			"setUp": !s.Exists && isClusterOperator(ctx),
		},
	})
}

// updateSettings merges a change into a store's settings row.
//
// THE CALLER REACHED THIS PAST THE ORGANIZATION GATE (the part, in the site's
// organization) and the site is checked to serve the store BEFORE anything is
// written. A store with no settings row is refused: creating one is
// wholesaleSetSettings', and the operators'. The row's owner, organization and
// site stay the row's own; only the fields named change.
func (p *Provider) updateSettings(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := trimmed(args["storeId"])
	if storeID == "" {
		return nil, fmt.Errorf("wholesale: storeId is required; settings are per store")
	}
	if _, err := p.siteForStore(ctx, trimmed(args["siteId"]), storeID); err != nil {
		return nil, err
	}
	row, err := p.settingsRowAt(auth.ContextWithSystemActor(ctx, storeLookupActor), storeID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errors.New(NotSetUpSentence)
	}
	owner := userRef(trimmed(row["ownerUserId"]))
	if owner == "" {
		return nil, fmt.Errorf("wholesale: store %q's settings name no owner; the cluster's operators set it up again", storeID)
	}
	next := EffectiveSettings(row)
	if v, named := args["applicationsOpen"]; named && v != nil {
		open, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("wholesale: applicationsOpen must be a bool")
		}
		next.ApplicationsOpen = open
	}
	if v, named := args["entitlementAdapter"]; named && v != nil {
		route, err := routeName(asString(v))
		if err != nil {
			return nil, err
		}
		next.EntitlementAdapter = route
	}
	if v, named := args["introText"]; named && v != nil {
		next.IntroText = asString(v)
	}
	if v, named := args["staffEmail"]; named && v != nil {
		email := trimmed(v)
		if err := validStaffEmail(email); err != nil {
			return nil, err
		}
		next.StaffEmail = email
	}
	// THE ROW READ IS THE ROW WRITTEN, at its own id with its own storeId,
	// whichever form of the store id the caller named.
	rowID := settingsRowIDOf(row, storeID)
	rowStore := trimmed(row["storeId"])
	if rowStore == "" {
		rowStore = bareID(storeID)
	}
	// FENCED ON THE VERSION READ: a change merged into a row that another save
	// replaced meanwhile would put back what that save changed. The loser is
	// refused, not retried, so the person re-reads what is there now.
	read := rowTime(row["createdAt"])
	if read.IsZero() {
		return nil, fmt.Errorf("wholesale: store %q's settings carry no version to save against; reload and try again", storeID)
	}
	fenced := memql.ContextWithRowVersionFence(ctx, settingsConcept, rowID, read)
	// EVERY FIELD, the empty and false ones included: a write at an existing id
	// merges into the row's current version, and this one means the whole row.
	if err := p.write(fenced, "putWholesaleSettingsRow", map[string]any{
		"settingsId":         rowID,
		"ownerUserId":        owner,
		"storeId":            rowStore,
		"applicationsOpen":   next.ApplicationsOpen,
		"entitlementAdapter": next.EntitlementAdapter,
		"introText":          next.IntroText,
		"staffEmail":         next.StaffEmail,
		"accountId":          next.AccountID,
		"siteId":             next.SiteID,
	}); err != nil {
		if errors.Is(err, memql.ErrRowVersionChanged) {
			return nil, errors.New(ChangedWhileEditingSentence)
		}
		return nil, err
	}
	return confirm("wholesale:settings:written", map[string]any{
		"storeId":            rowStore,
		"applicationsOpen":   next.ApplicationsOpen,
		"entitlementAdapter": next.EntitlementAdapter,
		"staffEmail":         next.StaffEmail,
	})
}

// routeName resolves a route a caller names: empty and none both mean none,
// and anything else must be a registered adapter, refused by name otherwise.
func routeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == AdapterNone {
		return "", nil
	}
	if _, err := AdapterByName(name); err != nil {
		return "", err
	}
	return name, nil
}

// validStaffEmail admits one address or none.
func validStaffEmail(email string) error {
	if email == "" {
		return nil
	}
	if len(email) > maxStaffEmail || strings.Count(email, "@") != 1 {
		return fmt.Errorf("wholesale: the staff address must be one address -- at most %d characters with "+
			"exactly one @ -- or empty for none; %q is not", maxStaffEmail, email)
	}
	return nil
}

// siteForStore answers the site an act names, once it has checked the site is
// the STORE'S OWN. The organization gate judged the act against that site's
// organization, so the site must serve the store, and -- once the store's
// settings carry an organization -- belong to that organization: a site of
// another organization bound to the same store would let a person granted the
// part there act on this store.
//
// A deliberate copy of packs/reviewspack/settings.go's siteForStore (a pack
// may not import another pack), with one difference: the settings row is read
// as the system, so an untied row cannot hide its organization from the check.
func (p *Provider) siteForStore(ctx context.Context, siteID, storeID string) (map[string]any, error) {
	siteID = strings.TrimSpace(siteID)
	if siteID == "" {
		return nil, fmt.Errorf("wholesale: siteId is required: the act is judged against the organization of the store's site")
	}
	rows, err := p.rowsFor(ctx, "siteById", map[string]string{"siteId": siteID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("wholesale: site %q is not readable by this caller", siteID)
	}
	site := rows[0]
	if !servesStore(site, bareID(storeID)) {
		return nil, fmt.Errorf("wholesale: site %q is not bound to store %q", siteID, storeID)
	}
	// EVERY SETTINGS ROW OF THE STORE, under every form of its id: the check
	// must not depend on how the caller spelled the store.
	settingsRows, err := p.storeSettingsRows(auth.ContextWithSystemActor(ctx, storeLookupActor), storeID)
	if err != nil {
		return nil, err
	}
	for _, settings := range settingsRows {
		if want := bareID(trimmed(settings["accountId"])); want != "" && bareID(trimmed(site["accountId"])) != want {
			return nil, fmt.Errorf("wholesale: site %q belongs to another organization than store %q's wholesale settings", siteID, storeID)
		}
	}
	return site, nil
}

// sitesForStore reads every site whose serving or preview binding names the
// store, in any of the forms a store id is stored in.
func (p *Provider) sitesForStore(ctx context.Context, storeID string) ([]map[string]any, error) {
	if p.reader == nil {
		return nil, fmt.Errorf("wholesale: the pack has no reader; this capability cannot run")
	}
	bare := bareID(storeID)
	return p.reader.RowsWithList(ctx, "sitesBoundToStore", map[string]string{},
		"storeIds", []string{storeID, bare, "v1:shopify:store:" + bare})
}

// applicationsOpen reads this store's gate.
//
// ABSENT IS CLOSED. A store whose merchant has never touched the setting
// has not asked for applications, and a missing row must not open a public
// write endpoint that collects somebody's business contact details.
func (p *Provider) applicationsOpen(ctx context.Context, storeID string) (bool, error) {
	settings, err := p.settingsRowAt(ctx, storeID)
	if err != nil {
		return false, err
	}
	return EffectiveSettings(settings).ApplicationsOpen, nil
}

// settingsRowAt is the store's ONE settings row, whichever form of its id the
// caller named (storeSettingsRows' first), or nil when there is none.
func (p *Provider) settingsRowAt(ctx context.Context, storeID string) (map[string]any, error) {
	rows, err := p.storeSettingsRows(ctx, storeID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// settingsRowByID reads one settings row by its id, or nil.
func (p *Provider) settingsRowByID(ctx context.Context, settingsID string) (map[string]any, error) {
	rows, err := p.rowsFor(ctx, "wholesaleSettingsById", map[string]string{"settingsId": settingsID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}
