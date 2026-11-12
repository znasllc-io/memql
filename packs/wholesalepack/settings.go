package wholesalepack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/outbound"
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

// NotSetUpSentence is the refusal of every act of the Wholesale section on a
// store that is not set up: one with no settings row, or one whose settings row
// is tied to no organization (ruling 55). The OS shows it as written.
const NotSetUpSentence = "This store's wholesale program is not set up yet. The cluster's operators set it up."

// errNotSetUp is the store gate's refusal of a store that is not set up.
var errNotSetUp = errors.New(NotSetUpSentence)

// NameTheSiteSentence refuses a set-up that cannot tell which site the store's
// program belongs to: no siteId named, and not exactly one site bound to the
// store.
const NameTheSiteSentence = "Name the site this store's wholesale program belongs to."

// AnotherOwnerSentence refuses a set-up that would tie an existing row to a
// site of another owner than the row's: a row keeps its owner, and tying it to
// another owner's organization would split the two.
const AnotherOwnerSentence = "That site belongs to another owner than this store's wholesale program."

// maxStaffEmail is the longest address the store's people may name.
const maxStaffEmail = 320

// maxAcceptanceText is the longest acceptance sentence, in characters: the
// settings mutation's bound.
const maxAcceptanceText = 2000

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
// settings read as not set up, closed, with no route, requiring no terms, with
// the default link lifetime and acceptance sentence.
type Settings struct {
	Exists, ApplicationsOpen                                     bool
	EntitlementAdapter, IntroText, StaffEmail, AccountID, SiteID string
	// TermsRequired absent is false.
	TermsRequired bool
	// LinkLifetimeDays is the row's, or DefaultLinkLifetimeDays when it names
	// none in 1 to 365.
	LinkLifetimeDays int
	// AcceptanceText is the row's, or DefaultAcceptanceText when it is blank.
	AcceptanceText string
	// SenderIdentityID is the sender the applicant's emails go out from; empty
	// is the organization's configured sender. The cluster's operators choose it.
	SenderIdentityID string
	// The template of each of the applicant's emails; empty sends none.
	TermsTemplateID, ApprovedTemplateID, RejectedTemplateID, RevokedTemplateID string
}

// heldEmail is what a settings row holds of the five email settings: what a
// write of the whole row passes back.
type heldEmail struct {
	senderIdentityID, termsTemplateID, approvedTemplateID, rejectedTemplateID, revokedTemplateID string
}

// emailHeld reads the five email settings a row holds, nil reading as none.
func emailHeld(row map[string]any) heldEmail {
	return heldEmail{
		senderIdentityID:   trimmed(row["senderIdentityId"]),
		termsTemplateID:    trimmed(row["termsTemplateId"]),
		approvedTemplateID: trimmed(row["approvedTemplateId"]),
		rejectedTemplateID: trimmed(row["rejectedTemplateId"]),
		revokedTemplateID:  trimmed(row["revokedTemplateId"]),
	}
}

// args is the five settings as a settings mutation's arguments.
func (h heldEmail) args(into map[string]any) map[string]any {
	into["senderIdentityId"] = h.senderIdentityID
	into["termsTemplateId"] = h.termsTemplateID
	into["approvedTemplateId"] = h.approvedTemplateID
	into["rejectedTemplateId"] = h.rejectedTemplateID
	into["revokedTemplateId"] = h.revokedTemplateID
	return into
}

// TemplateFor is the template the store chose for an email of kind, or "".
func (s Settings) TemplateFor(kind string) string {
	switch kind {
	case EmailTerms:
		return s.TermsTemplateID
	case EmailApproved:
		return s.ApprovedTemplateID
	case EmailRejected:
		return s.RejectedTemplateID
	case EmailRevoked:
		return s.RevokedTemplateID
	}
	return ""
}

// heldTerms is what a settings row itself holds of the three terms settings,
// before any default: what a write of the whole row passes back.
type heldTerms struct {
	termsRequired    bool
	linkLifetimeDays int
	acceptanceText   string
}

// termsHeld reads the three terms settings a row holds, nil reading as none.
func termsHeld(row map[string]any) heldTerms {
	required, _ := row["termsRequired"].(bool)
	days, _ := intArg(row["linkLifetimeDays"])
	return heldTerms{termsRequired: required, linkLifetimeDays: days, acceptanceText: asString(row["acceptanceText"])}
}

// EffectiveSettings reads a settings row, or nil for a store with none.
func EffectiveSettings(row map[string]any) Settings {
	if row == nil {
		return Settings{LinkLifetimeDays: DefaultLinkLifetimeDays, AcceptanceText: DefaultAcceptanceText}
	}
	open, _ := row["applicationsOpen"].(bool)
	route := trimmed(row["entitlementAdapter"])
	if route == AdapterNone {
		route = ""
	}
	held := termsHeld(row)
	days := held.linkLifetimeDays
	if days < minLinkLifetimeDays || days > maxLinkLifetimeDays {
		days = DefaultLinkLifetimeDays
	}
	text := held.acceptanceText
	if strings.TrimSpace(text) == "" {
		text = DefaultAcceptanceText
	}
	email := emailHeld(row)
	return Settings{
		SenderIdentityID:   email.senderIdentityID,
		TermsTemplateID:    email.termsTemplateID,
		ApprovedTemplateID: email.approvedTemplateID,
		RejectedTemplateID: email.rejectedTemplateID,
		RevokedTemplateID:  email.revokedTemplateID,
		Exists:             true,
		ApplicationsOpen:   open,
		EntitlementAdapter: route,
		IntroText:          asString(row["introText"]),
		StaffEmail:         trimmed(row["staffEmail"]),
		AccountID:          trimmed(row["accountId"]),
		SiteID:             trimmed(row["siteId"]),
		TermsRequired:      held.termsRequired,
		LinkLifetimeDays:   days,
		AcceptanceText:     text,
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
	intro := asString(args["introText"])
	if place.tying {
		open, adapter, intro = place.held.open, place.held.adapter, place.held.text
	}
	// THE TERMS RULE, ON THE ROW AS IT WILL BE: this write's applicationsOpen
	// with the terms setting the row already holds.
	if err := p.termsRuleRefusal(ctx, place.storeID, open, place.terms.termsRequired); err != nil {
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
		"introText":          intro,
		// EVERY FIELD EXPLICIT: the row's organization and site are the named
		// site's, or kept, or, for a new row, the one site bound to the store;
		// the staff address is the one the row held (none on a new row).
		"accountId":  place.accountID,
		"siteId":     place.siteID,
		"staffEmail": place.staffEmail,
		// And the terms settings the row held, none on a new row: they are the
		// store's people's to change, through wholesaleUpdateSettings.
		"termsRequired":    place.terms.termsRequired,
		"linkLifetimeDays": place.terms.linkLifetimeDays,
		"acceptanceText":   place.terms.acceptanceText,
		// And the email settings the row held, none on a new row: the templates
		// are the store's people's to choose, and the sender the operators'.
		"senderIdentityId":   place.email.senderIdentityID,
		"termsTemplateId":    place.email.termsTemplateID,
		"approvedTemplateId": place.email.approvedTemplateID,
		"rejectedTemplateId": place.email.rejectedTemplateID,
		"revokedTemplateId":  place.email.revokedTemplateID,
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
	// terms is the terms settings the row already holds, written back whole.
	terms heldTerms
	// email is the email settings the row already holds, written back whole.
	email heldEmail
	// version is the createdAt of the row read, zero when there was none: the
	// write is fenced on it.
	version time.Time
	// tying is true when the write ties an existing row that was tied to no
	// organization: it then keeps every setting the row holds, and held is
	// what it holds of the three the set-up otherwise names.
	tying bool
	held  struct {
		open          bool
		adapter, text string
	}
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
// AND WHAT IT IS TIED TO, which is always an organization (settingsPlaceFrom):
// the named site's, or the one bound site's, or an existing tied row's own.
//
// THE EXISTING OWNER IS KEPT. Otherwise the operator who changed a setting
// would become the row's owner, and the merchant's own reads -- the shopper
// path's applicationsOpen among them -- would stop seeing it.
//
// A NEW ROW IS THE STORE'S MERCHANT'S: the owner of the site it is tied to.
func (p *Provider) settingsOwner(ctx context.Context, storeID, siteID string) (settingsPlace, error) {
	sys := auth.ContextWithSystemActor(ctx, storeLookupActor)
	existing, err := p.settingsRowAt(sys, storeID)
	if err != nil {
		return settingsPlace{}, err
	}
	place, err := p.settingsPlaceFrom(sys, existing, storeID, siteID)
	if err != nil {
		return settingsPlace{}, err
	}
	if existing != nil {
		place.rowID, place.storeID = settingsRowIDOf(existing, storeID), trimmed(existing["storeId"])
		// The terms and email settings are the row's, whoever it is found to
		// belong to.
		place.terms = termsHeld(existing)
		place.email = emailHeld(existing)
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
//
// THE SET-UP ALWAYS ENDS WITH A ROW TIED TO AN ORGANIZATION (ruling 55): the
// site the store's program belongs to is the named one, or else the one site
// bound to the store; with neither -- no siteId, and no site or more than one
// bound -- it is refused with NameTheSiteSentence, and so is a site that
// belongs to no organization. An existing row keeps its owner, and its
// organization and site unless a site is named; a row that is not tied yet is
// tied to the site found.
func (p *Provider) settingsPlaceFrom(sys context.Context, existing map[string]any, storeID, siteID string) (settingsPlace, error) {
	tie, err := p.siteOfTheProgram(sys, storeID, siteID)
	if err != nil {
		return settingsPlace{}, err
	}
	var place settingsPlace
	if owner := trimmed(existing["ownerUserId"]); existing != nil && owner != "" {
		place = settingsPlace{owner: userRef(owner), accountID: trimmed(existing["accountId"]),
			siteID: trimmed(existing["siteId"]), staffEmail: trimmed(existing["staffEmail"]),
			version: rowTime(existing["createdAt"])}
		if place.version.IsZero() {
			return settingsPlace{}, fmt.Errorf("wholesale: store %q's settings carry no version to save against; "+
				"reload and try again", storeID)
		}
		if tie != nil && (siteID != "" || bareID(place.accountID) == "") {
			// A ROW KEEPS ITS OWNER, so it is tied only to a site of that owner:
			// another owner's organization would split the two.
			if userRef(trimmed(tie["ownerUserId"])) != place.owner {
				return settingsPlace{}, errors.New(AnotherOwnerSentence)
			}
			// TYING AN UNTIED ROW CHANGES NOTHING ELSE: it keeps whether it is
			// open, its route and its text, whatever the set-up names.
			if bareID(place.accountID) == "" {
				place.tying = true
				place.held.open, _ = existing["applicationsOpen"].(bool)
				place.held.adapter, place.held.text = trimmed(existing["entitlementAdapter"]), asString(existing["introText"])
			}
			place.accountID, place.siteID = trimmed(tie["accountId"]), bareID(trimmed(tie["id"]))
		}
	} else if tie != nil {
		owner := userRef(trimmed(tie["ownerUserId"]))
		if owner == "" {
			return settingsPlace{}, fmt.Errorf("wholesale: site %q names no owner, so there is no merchant to own "+
				"store %q's wholesale settings", trimmed(tie["id"]), storeID)
		}
		place = settingsPlace{owner: owner, accountID: trimmed(tie["accountId"]), siteID: bareID(trimmed(tie["id"]))}
	}
	if bareID(place.accountID) == "" {
		if tie == nil {
			return settingsPlace{}, errors.New(NameTheSiteSentence)
		}
		return settingsPlace{}, fmt.Errorf("wholesale: site %q belongs to no organization, and a store's wholesale "+
			"program belongs to one; tie the site to its organization first", trimmed(tie["id"]))
	}
	return place, nil
}

// siteOfTheProgram is the site a set-up ties the store's program to, read as
// sys: the named site, checked to serve the store and to be of the
// organization any of the store's settings rows names; or, with none named,
// the one site whose serving or preview binding names the store. nil when no
// site is named and not exactly one is bound.
func (p *Provider) siteOfTheProgram(sys context.Context, storeID, siteID string) (map[string]any, error) {
	if siteID != "" {
		site, err := p.siteServingStore(sys, siteID, storeID)
		if err != nil {
			return nil, err
		}
		rows, err := p.storeSettingsRows(sys, storeID)
		if err != nil {
			return nil, err
		}
		if err := siteIsTheStoresOrganizations(site, siteID, storeID, rows); err != nil {
			return nil, err
		}
		return site, nil
	}
	sites, err := p.sitesForStore(sys, storeID)
	if err != nil {
		return nil, err
	}
	if len(sites) != 1 {
		return nil, nil
	}
	return sites[0], nil
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
		// A STORE THAT IS NOT SET UP is answered as not set up, in the reply's
		// own shape, and with NOTHING of the store: every setting at its
		// default, no document, and whether this caller may set it up.
		if errors.Is(err, errNotSetUp) {
			return notSetUpSettings(ctx)
		}
		return nil, err
	}
	row, err := p.settingsRowAt(auth.ContextWithSystemActor(ctx, storeLookupActor), storeID)
	if err != nil {
		return nil, err
	}
	s := EffectiveSettings(row)
	documents, err := p.documentsSummary(ctx, storeID)
	if err != nil {
		return nil, err
	}
	// THE TEMPLATES ARE READ UNDER THE CALLER, on purpose: they are the Email
	// app's, so a person the organization has not granted it is offered none
	// and can choose only none.
	templates, err := p.emailTemplates(ctx, s.AccountID)
	if err != nil {
		return nil, err
	}
	choices := make([]map[string]any, 0, len(templates))
	for _, t := range templates {
		choices = append(choices, map[string]any{"id": t.id, "name": t.name})
	}
	return confirm("wholesale:settings:store", map[string]any{
		"exists":             s.Exists,
		"applicationsOpen":   s.ApplicationsOpen,
		"entitlementAdapter": s.EntitlementAdapter,
		"introText":          s.IntroText,
		"staffEmail":         s.StaffEmail,
		"staffEmailAdmitted": s.StaffEmail != "" && outbound.EmailAdmitted(s.StaffEmail),
		"termsRequired":      s.TermsRequired,
		"linkLifetimeDays":   s.LinkLifetimeDays,
		"acceptanceText":     s.AcceptanceText,
		"senderIdentityId":   s.SenderIdentityID,
		"termsTemplateId":    s.TermsTemplateID,
		"approvedTemplateId": s.ApprovedTemplateID,
		"rejectedTemplateId": s.RejectedTemplateID,
		"revokedTemplateId":  s.RevokedTemplateID,
		"templates":          choices,
		"documents":          documents,
		"adapters":           AdapterNames(),
		"may": map[string]any{
			// Everyone past the gate holds the part or is an operator, so a
			// store that is set up may be changed by them.
			"update": s.Exists,
			// A store is set up by the cluster's operators alone.
			"setUp": !s.Exists && isClusterOperator(ctx),
			// And its sender is chosen by them alone.
			"setSender": isClusterOperator(ctx),
		},
	})
}

// notSetUpSettings is wholesaleStoreSettings' reply for a store that is not set
// up: exists false, every setting its default, each document null, the routes
// this engine has, may.update false, and may.setUp for a cluster operator. It
// reads nothing.
func notSetUpSettings(ctx context.Context) ([]memorynodes.MemoryNode, error) {
	s := EffectiveSettings(nil)
	documents := map[string]any{}
	for _, kind := range DocumentKinds {
		documents[kind] = nil
	}
	return confirm("wholesale:settings:store", map[string]any{
		"exists":             false,
		"applicationsOpen":   false,
		"entitlementAdapter": "",
		"introText":          "",
		"staffEmail":         "",
		"staffEmailAdmitted": false,
		"termsRequired":      false,
		"linkLifetimeDays":   s.LinkLifetimeDays,
		"acceptanceText":     s.AcceptanceText,
		"senderIdentityId":   "",
		"termsTemplateId":    "",
		"approvedTemplateId": "",
		"rejectedTemplateId": "",
		"revokedTemplateId":  "",
		"templates":          []map[string]any{},
		"documents":          documents,
		"adapters":           AdapterNames(),
		"may":                map[string]any{"update": false, "setUp": isClusterOperator(ctx), "setSender": false},
	})
}

// updateSettings merges a change into a store's settings row.
//
// THE CALLER REACHED THIS PAST THE ORGANIZATION GATE (the part, in the site's
// organization), and the store gate (siteForStore) admits the act BEFORE
// anything is written: a store with no settings row, or one tied to no
// organization, is refused with NotSetUpSentence -- creating or tying one is
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
	// A STORE WITH NO SETTINGS ROW never reaches here: the store gate above
	// refused it with NotSetUpSentence.
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
	// THE TERMS SETTINGS AS THE ROW HOLDS THEM, not their defaults: a lifetime
	// or a sentence the store never named stays unnamed, and reads as the
	// default whatever the default is then.
	terms := termsHeld(row)
	if v, named := args["termsRequired"]; named && v != nil {
		required, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("wholesale: termsRequired must be a bool")
		}
		terms.termsRequired = required
	}
	if v, named := args["linkLifetimeDays"]; named && v != nil {
		days, err := validLinkLifetime(v)
		if err != nil {
			return nil, err
		}
		terms.linkLifetimeDays = days
	}
	if v, named := args["acceptanceText"]; named && v != nil {
		text := asString(v)
		if n := utf8.RuneCountInString(text); n > maxAcceptanceText {
			return nil, fmt.Errorf("wholesale: the acceptance sentence is at most %d characters, or empty for the default; "+
				"this one is %d", maxAcceptanceText, n)
		}
		terms.acceptanceText = text
	}
	email, err := p.mergeEmailSettings(ctx, args, row)
	if err != nil {
		return nil, err
	}
	// THE TERMS RULE, ON THE MERGED RESULT: a store that would take
	// applications while requiring terms it has not published is refused.
	if err := p.termsRuleRefusal(ctx, storeID, next.ApplicationsOpen, terms.termsRequired); err != nil {
		return nil, err
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
	if err := p.write(fenced, "putWholesaleSettingsRow", email.args(map[string]any{
		"settingsId":         rowID,
		"ownerUserId":        owner,
		"storeId":            rowStore,
		"applicationsOpen":   next.ApplicationsOpen,
		"entitlementAdapter": next.EntitlementAdapter,
		"introText":          next.IntroText,
		"staffEmail":         next.StaffEmail,
		"accountId":          next.AccountID,
		"siteId":             next.SiteID,
		"termsRequired":      terms.termsRequired,
		"linkLifetimeDays":   terms.linkLifetimeDays,
		"acceptanceText":     terms.acceptanceText,
	})); err != nil {
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

// validStaffEmail admits one address or none, by the pack's one rule of what an
// address is: the applicant's (memql.IsEmailAddress). Only a write is judged; a
// stored address is read as it is.
func validStaffEmail(email string) error {
	if email == "" {
		return nil
	}
	if !memql.IsEmailAddress(email) {
		return fmt.Errorf("wholesale: the staff address must be one address -- at most %d bytes, "+
			"exactly one @ with something on each side and no whitespace -- or empty for none; %q is not",
			maxStaffEmail, email)
	}
	return nil
}

// siteForStore is THE STORE GATE every act of the Wholesale section on a store
// as a whole goes through, directly or through admitStore (ruling 55). It
// answers the site the act names once it has checked three things, in order:
//
//   - the site serves the store (siteServingStore);
//   - the store is SET UP: its settings row exists and is tied to an
//     organization. A store with no row, or whose row names no organization,
//     refuses every act with NotSetUpSentence -- before anything of the store
//     beyond its settings rows is read, and before any write. Without this, a
//     site of another organization bound to the store would have nothing to be
//     checked against, and a person granted the part there could write into
//     the store: a buyer, a document version, a settings change;
//   - the site belongs to that organization: a site of another organization
//     bound to the same store is refused.
//
// The only act that works on a store that is not set up is the set-up itself
// (wholesaleSetSettings), which does not come through here (settingsPlaceFrom).
//
// The settings rows are read as the system, so an untied row cannot hide its
// organization from the check: reading them IS the check.
func (p *Provider) siteForStore(ctx context.Context, siteID, storeID string) (map[string]any, error) {
	site, err := p.siteServingStore(ctx, siteID, storeID)
	if err != nil {
		return nil, err
	}
	if err := p.storeAdmitsSite(ctx, site, siteID, storeID); err != nil {
		return nil, err
	}
	return site, nil
}

// storeAdmitsSite is THE STORE GATE'S JUDGEMENT of a site already known to serve
// the store: the store is set up -- its one settings row (storeSettingsRows'
// first, the row every read uses) exists and is tied to an organization, else
// errNotSetUp -- and the site belongs to that organization, else
// errAnotherOrganization. siteForStore and the storefront's submission
// (ruling 56) both ask it, so the two cannot drift. The rows are read as the
// system.
func (p *Provider) storeAdmitsSite(ctx context.Context, site map[string]any, siteID, storeID string) error {
	settingsRows, err := p.storeSettingsRows(auth.ContextWithSystemActor(ctx, storeLookupActor), storeID)
	if err != nil {
		return err
	}
	if len(settingsRows) == 0 || bareID(trimmed(settingsRows[0]["accountId"])) == "" {
		return errNotSetUp
	}
	return siteIsTheStoresOrganizations(site, siteID, storeID, settingsRows)
}

// errAnotherOrganization is the store gate's refusal of a site of another
// organization than the store's program.
var errAnotherOrganization = errors.New("wholesale: the site belongs to another organization than the store's wholesale settings")

// siteServingStore reads the named site under ctx and checks it serves the
// store (serving or preview binding).
//
// A deliberate copy of packs/reviewspack/settings.go's siteForStore's first
// half (a pack may not import another pack).
func (p *Provider) siteServingStore(ctx context.Context, siteID, storeID string) (map[string]any, error) {
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
	return site, nil
}

// siteIsTheStoresOrganizations refuses a site of another organization than any
// of the store's settings rows that names one -- EVERY settings row of the
// store, under every form of its id, so the check does not depend on how the
// caller spelled the store.
func siteIsTheStoresOrganizations(site map[string]any, siteID, storeID string, settingsRows []map[string]any) error {
	for _, settings := range settingsRows {
		if want := bareID(trimmed(settings["accountId"])); want != "" && bareID(trimmed(site["accountId"])) != want {
			return fmt.Errorf("%w: site %q belongs to another organization than store %q's wholesale settings",
				errAnotherOrganization, siteID, storeID)
		}
	}
	return nil
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
