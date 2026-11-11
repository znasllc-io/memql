package wholesalepack

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// backfill.go -- TIE THE ROWS WRITTEN BEFORE ORGANIZATIONS REACHED THIS PACK
// (U5; design record X19).
//
// Every wholesale row now carries the organization of the site it came
// through, and the row tiers admit that organization's people. A row written
// before this version carries none, so it stays its owner's and the
// operators' -- readable exactly as before, and invisible to the store's own
// staff. This is the one operator call that ties them, run once at rollout
// and safe to run again: a row already tied is counted and never rewritten,
// and two runs at once write identical versions.
//
// THE WRITES ESCAPE THE ROW GATE. They go through engineWriter at internal
// origin, which the engine does not owner-check, so the builtin admits
// internal origin or a cluster operator and refuses everybody else BEFORE it
// reads anything.

// backfillActor is the system actor the backfill reads as. It sees every row;
// the writes go out as each application's merchant.
const backfillActor = "wholesale-backfill"

// applicationsWalkWindow is applicationsWithoutAccount's paginate window: a
// page shorter than this is the last one.
const applicationsWalkWindow = 200

// Why an application is left untied. Verbatim in the report, because the
// operator acts on them: fix the site, then run the backfill again.
const (
	UntiedNoSite    = "no site serves this store"
	UntiedTwoSites  = "two sites serve this store"
	UntiedNoAccount = "its site has no organization"
)

// mayBackfill admits internal origin or a cluster operator.
func mayBackfill(ctx context.Context) error {
	if auth.OriginFromContext(ctx).IsInternal() {
		return nil
	}
	if ac, ok := auth.AccessFromContext(ctx); ok && auth.IsClusterOperator(ac.Role) {
		return nil
	}
	return fmt.Errorf("wholesale: accounts are backfilled by the cluster's operators")
}

// backfillAccounts ties every application in scope, its decisions, its
// entitlement and its store's settings to the organization of its site.
//
// It answers {tied, alreadyTied, untied: [{id, reason}]}: tied counts the rows
// this run wrote, alreadyTied the rows it found tied, and untied names each
// application it could not tie with why.
func (p *Provider) backfillAccounts(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := mayBackfill(ctx); err != nil {
		return nil, err
	}
	run := &backfillRun{
		p:        p,
		ctx:      ctx,
		sys:      auth.ContextWithSystemActor(ctx, backfillActor),
		sites:    map[string][]map[string]any{},
		settings: map[string]bool{},
		untied:   []map[string]any{},
	}
	// THE ONE STAMPED READ: applicationsWithoutAccount is @serverOnly, so the
	// walk reads it at internal origin, inline, as the system, only after the
	// caller was admitted above.
	walk := auth.ContextWithInternalOrigin(run.sys)
	// OLDEST FIRST, A WINDOW AT A TIME, FROM THE LAST createdAt SEEN. The
	// bound is inclusive, so rows sharing a timestamp across a window's edge
	// are not skipped; the seen set skips them the second time, and skips an
	// application this run tied when its new version comes round at the end.
	storeID := trimmed(args["storeId"])
	seen := map[string]bool{}
	after := ""
	for {
		q := map[string]string{}
		if storeID != "" {
			q["storeId"] = storeID
		}
		if after != "" {
			q["after"] = after
		}
		page, err := p.rowsFor(walk, "applicationsWithoutAccount", q)
		if err != nil {
			return nil, err
		}
		fresh := 0
		for _, app := range page {
			id := bareID(trimmed(app["id"]))
			if seen[id] {
				continue
			}
			seen[id] = true
			fresh++
			if err := run.tie(app); err != nil {
				return nil, err
			}
		}
		// A SHORT WINDOW IS THE LAST; a window of nothing new means every row
		// left shares one timestamp the walk has already read past.
		if len(page) < applicationsWalkWindow || fresh == 0 {
			break
		}
		after = asString(page[len(page)-1]["createdAt"])
	}
	return confirm("wholesale:backfill:report", map[string]any{
		"tied":        run.tied,
		"alreadyTied": run.alreadyTied,
		"untied":      run.untied,
	})
}

// backfillRun is one call's state: its counts and what it has already read.
type backfillRun struct {
	p           *Provider
	ctx         context.Context // the caller's, which each write acts from as the merchant
	sys         context.Context // the system actor every read runs as
	sites       map[string][]map[string]any
	settings    map[string]bool // stores whose settings this run has handled
	tied        int
	alreadyTied int
	untied      []map[string]any
}

// tie ties one application, then its decisions, its entitlement and its
// store's settings. An application already tied is counted, never rewritten,
// and its rows are tied to its account; one that cannot be tied is reported
// and nothing of it is written.
func (r *backfillRun) tie(app map[string]any) error {
	appID := bareID(trimmed(app["id"]))
	storeID := trimmed(app["storeId"])
	account := trimmed(app["accountId"])
	siteID := trimmed(app["siteId"])
	merchant, err := r.p.asMerchant(r.ctx, app)
	if err != nil {
		return err
	}
	if account != "" {
		r.alreadyTied++
	} else {
		sites, err := r.sitesFor(app)
		if err != nil {
			return err
		}
		site, reason := siteForApplication(app, sites)
		if reason != "" {
			r.untied = append(r.untied, map[string]any{"id": appID, "reason": reason})
			return nil
		}
		account = trimmed(site["accountId"])
		if siteID == "" {
			siteID = bareID(trimmed(site["id"]))
		}
		if err := r.p.write(merchant, "backfillApplicationRow", map[string]any{
			"applicationId":  appID,
			"ownerUserId":    trimmed(app["ownerUserId"]),
			"storeId":        storeID,
			"siteId":         trimmed(app["siteId"]),
			"companyName":    asString(app["companyName"]),
			"applicantName":  asString(app["applicantName"]),
			"applicantEmail": asString(app["applicantEmail"]),
			"accountId":      account,
		}); err != nil {
			return err
		}
		r.tied++
	}
	if err := r.tieDecisions(merchant, app, account); err != nil {
		return err
	}
	if err := r.tieEntitlement(merchant, appID, account); err != nil {
		return err
	}
	return r.tieSettings(merchant, storeID, account, siteID)
}

// tieDecisions ties each decision on the application and names its merchant.
// X5: every other field is written back exactly as the row held it --
// decidedBy and principalKind included, as arguments -- so the backfill
// changes no decision field.
func (r *backfillRun) tieDecisions(merchant context.Context, app map[string]any, account string) error {
	appID := bareID(trimmed(app["id"]))
	decisions, err := r.p.rowsFor(r.sys, "decisionsForApplication", map[string]string{"applicationId": appID})
	if err != nil {
		return err
	}
	for _, d := range decisions {
		if trimmed(d["accountId"]) != "" {
			r.alreadyTied++
			continue
		}
		if err := r.p.write(merchant, "backfillDecisionRow", map[string]any{
			"decisionId":    bareID(trimmed(d["id"])),
			"applicationId": trimmed(d["applicationId"]),
			"ownerUserId":   trimmed(app["ownerUserId"]),
			"storeId":       trimmed(d["storeId"]),
			"transition":    trimmed(d["transition"]),
			"decidedBy":     trimmed(d["decidedBy"]),
			"principalKind": trimmed(d["principalKind"]),
			"note":          asString(d["note"]),
			"accountId":     account,
		}); err != nil {
			return err
		}
		r.tied++
	}
	return nil
}

// tieEntitlement ties the application's entitlement, every field passed
// explicitly, the empty and false ones included.
func (r *backfillRun) tieEntitlement(merchant context.Context, appID, account string) error {
	ent, err := r.p.entitlementRow(r.sys, appID)
	if err != nil || ent == nil {
		return err
	}
	if trimmed(ent["accountId"]) != "" {
		r.alreadyTied++
		return nil
	}
	created, _ := ent["customerCreated"].(bool)
	if err := r.p.write(merchant, "backfillEntitlementRow", map[string]any{
		"entitlementId":     EntitlementRowID(appID),
		"ownerUserId":       trimmed(ent["ownerUserId"]),
		"applicationId":     trimmed(ent["applicationId"]),
		"storeId":           trimmed(ent["storeId"]),
		"buyerEmail":        asString(ent["buyerEmail"]),
		"adapter":           trimmed(ent["adapter"]),
		"state":             trimmed(ent["state"]),
		"reference":         asString(ent["reference"]),
		"error":             asString(ent["error"]),
		"errorCode":         trimmed(ent["errorCode"]),
		"shopifyCustomerId": trimmed(ent["shopifyCustomerId"]),
		"customerCreated":   created,
		"accountId":         account,
	}); err != nil {
		return err
	}
	r.tied++
	return nil
}

// tieSettings ties the store's settings once per run, with the site they were
// made for: the one they name, or else the application's.
func (r *backfillRun) tieSettings(merchant context.Context, storeID, account, siteID string) error {
	if r.settings[storeID] {
		return nil
	}
	settings, err := r.p.settingsRow(r.sys, storeID)
	if err != nil || settings == nil {
		return err
	}
	r.settings[storeID] = true
	if trimmed(settings["accountId"]) != "" {
		r.alreadyTied++
		return nil
	}
	if named := trimmed(settings["siteId"]); named != "" {
		siteID = named
	}
	open, _ := settings["applicationsOpen"].(bool)
	if err := r.p.write(merchant, "backfillSettingsRow", map[string]any{
		"settingsId":         SettingsRowID(storeID),
		"ownerUserId":        trimmed(settings["ownerUserId"]),
		"storeId":            storeID,
		"applicationsOpen":   open,
		"entitlementAdapter": trimmed(settings["entitlementAdapter"]),
		"introText":          asString(settings["introText"]),
		"accountId":          account,
		"siteId":             siteID,
	}); err != nil {
		return err
	}
	r.tied++
	return nil
}

// sitesFor is every site that may say which organization the application
// belongs to: those bound to its store, read once per store, and its own
// siteId site when that is not among them.
func (r *backfillRun) sitesFor(app map[string]any) ([]map[string]any, error) {
	storeID := trimmed(app["storeId"])
	sites, read := r.sites[storeID]
	if !read {
		var err error
		if sites, err = r.p.sitesForStore(r.sys, storeID); err != nil {
			return nil, err
		}
		r.sites[storeID] = sites
	}
	siteID := bareID(trimmed(app["siteId"]))
	if siteID == "" {
		return sites, nil
	}
	for _, s := range sites {
		if bareID(trimmed(s["id"])) == siteID {
			return sites, nil
		}
	}
	own, err := r.p.rowsFor(r.sys, "siteById", map[string]string{"siteId": siteID})
	if err != nil {
		return nil, err
	}
	return append(append([]map[string]any(nil), sites...), own...), nil
}

// accountForApplication is the organization an application belongs to, or
// why it cannot be told: its siteId site's, or, with no siteId, the one site
// whose serving or preview binding is the application's store.
func accountForApplication(app map[string]any, sites []map[string]any) (account, reason string) {
	site, reason := siteForApplication(app, sites)
	if reason != "" {
		return "", reason
	}
	return trimmed(site["accountId"]), ""
}

// siteForApplication is accountForApplication's site, for the caller that
// also needs its id.
func siteForApplication(app map[string]any, sites []map[string]any) (map[string]any, string) {
	withAccount := func(site map[string]any) (map[string]any, string) {
		if trimmed(site["accountId"]) == "" {
			return nil, UntiedNoAccount
		}
		return site, ""
	}
	if siteID := bareID(trimmed(app["siteId"])); siteID != "" {
		for _, site := range sites {
			if bareID(trimmed(site["id"])) == siteID {
				return withAccount(site)
			}
		}
	}
	store := bareID(trimmed(app["storeId"]))
	var serving []map[string]any
	for _, site := range sites {
		if servesStore(site, store) {
			serving = append(serving, site)
		}
	}
	switch len(serving) {
	case 0:
		return nil, UntiedNoSite
	case 1:
		return withAccount(serving[0])
	}
	return nil, UntiedTwoSites
}

// servesStore reports whether a site's serving or preview binding names the
// store, in whichever form either id is stored.
func servesStore(site map[string]any, bareStore string) bool {
	if bareStore == "" {
		return false
	}
	for _, key := range []string{"binding", "previewBinding"} {
		b, _ := site[key].(map[string]any)
		if strings.EqualFold(bareID(trimmed(b["storeId"])), bareStore) {
			return true
		}
	}
	return false
}
