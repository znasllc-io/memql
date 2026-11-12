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
//
// EACH WRITE IS FENCED ON THE VERSION IT READ, so a provision or a settings
// change that lands between the read and the write is never reverted: the
// loser re-reads and ties the fresh version.
//
// A WRITE HERE IS A NEW VERSION OF A ROW A CLIENT TRIGGERS ON. The engine
// publishes graph.node.created for every version, with firstVersion false on
// all but the first, and offers no way for a maintenance write to publish
// nothing. So a client automation that must run once per row filters on
// firstVersion -- the fixture clients under testdata/clients show the pattern
// -- and a client package must carry that filter before this is run.

// backfillActor is the system actor the backfill reads as. It sees every row;
// the writes go out as each application's merchant.
const backfillActor = "wholesale-backfill"

// applicationsWalkWindow is applicationsForBackfill's paginate window: a page
// shorter than this is the last one.
const applicationsWalkWindow = 200

// backfillAttempts bounds how often one row is re-read and tied again after
// another write took its version first.
const backfillAttempts = 3

// Why an application is left untied. Verbatim in the report, because the
// operator acts on them: fix the site, then run the backfill again.
const (
	UntiedNoSite    = "no site serves this store"
	UntiedTwoSites  = "two sites serve this store"
	UntiedNoAccount = "its site has no organization"
	// The plain words for an application's own site serving another store,
	// and for one that names no merchant to write it as.
	UntiedForeignSite = "its site does not serve this store"
	UntiedNoOwner     = "it names no merchant"
	// A store's settings row beside the one every path reads: held under the
	// other spelling of the store id. Reported, never tied, merged or deleted.
	UntiedSecondSettings = "a second settings row for this store"
)

// The concepts the backfill fences its writes on.
const (
	decisionConcept    = "v1:wholesale:applicationDecision"
	entitlementConcept = "v1:wholesale:entitlement"
	settingsConcept    = "v1:wholesale:wholesaleSettings"
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
// application it could not tie with why. ONE BAD ROW NEVER ABORTS THE RUN:
// whatever stops one application is its untied reason, and the walk goes on.
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
	// THE ONE STAMPED READ: applicationsForBackfill is @serverOnly, so the
	// walk reads it at internal origin, inline, as the system, only after the
	// caller was admitted above.
	walk := auth.ContextWithInternalOrigin(run.sys)
	window := applicationsWalkWindow
	if p.backfillWindow > 0 && p.backfillWindow < window {
		window = p.backfillWindow
	}
	// OLDEST FIRST, A WINDOW AT A TIME, FROM THE LAST createdAt SEEN. The
	// bound is inclusive, so rows sharing a timestamp across a window's edge
	// are not skipped; the seen set skips them the second time, and skips an
	// application this run tied when its new version comes round at the end.
	storeID := trimmed(args["storeId"])
	seen := map[string]bool{}
	var cursor time.Time
	for {
		page, err := p.backfillPage(walk, storeID, cursor)
		if err != nil {
			return nil, err
		}
		if len(page) > window {
			page = page[:window]
		}
		for _, app := range page {
			id := bareID(trimmed(app["id"]))
			if seen[id] {
				continue
			}
			seen[id] = true
			run.tie(app)
		}
		if len(page) < window {
			break
		}
		// A FULL WINDOW THAT ENDS WHERE IT BEGAN cannot be walked past by a
		// createdAt cursor: every row in it shares one creation time.
		last := rowTime(page[len(page)-1]["createdAt"])
		if !last.After(cursor) {
			return nil, fmt.Errorf("wholesale: every application in a window of %d shares one creation "+
				"time (%s), so the backfill cannot walk past it; it tied %d rows and found %d already "+
				"tied before it stopped", window, last.UTC().Format(time.RFC3339Nano), run.tied, run.alreadyTied)
		}
		cursor = last
	}
	return confirm("wholesale:backfill:report", map[string]any{
		"tied":        run.tied,
		"alreadyTied": run.alreadyTied,
		"untied":      run.untied,
	})
}

// backfillPage reads one window of the walk from cursor, inclusive. A store
// is named in every form its id is stored in, as sitesForStore names it.
func (p *Provider) backfillPage(ctx context.Context, storeID string, cursor time.Time) ([]map[string]any, error) {
	q := map[string]string{}
	if !cursor.IsZero() {
		q["after"] = cursor.UTC().Format(time.RFC3339Nano)
	}
	if storeID == "" {
		return p.rowsFor(ctx, "applicationsForBackfill", q)
	}
	if p.reader == nil {
		return nil, fmt.Errorf("wholesale: the pack has no reader; this capability cannot run")
	}
	return p.reader.RowsWithList(ctx, "applicationsForBackfill", q, "storeIds", storeForms(storeID))
}

// storeForms is every form a store id is stored in.
func storeForms(storeID string) []string {
	bare := bareID(storeID)
	return []string{storeID, bare, "v1:shopify:store:" + bare}
}

// rowTime is a row's createdAt, the version a write at its id is fenced on.
func rowTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		if at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(t)); err == nil {
			return at
		}
	}
	return time.Time{}
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

// leave reports one application untied, with why.
func (r *backfillRun) leave(appID, reason string) {
	r.untied = append(r.untied, map[string]any{"id": appID, "reason": reason})
}

// tie ties one application, then its decisions, its entitlement and its
// store's settings. An application already tied is counted, never rewritten,
// and its rows are tied to its account; one that cannot be tied is reported
// and nothing of it is written.
func (r *backfillRun) tie(app map[string]any) {
	appID := bareID(trimmed(app["id"]))
	storeID := trimmed(app["storeId"])
	account := trimmed(app["accountId"])
	siteID := trimmed(app["siteId"])
	owner := trimmed(app["ownerUserId"])
	if owner == "" {
		r.leave(appID, UntiedNoOwner)
		return
	}
	merchant := auth.ContextWithUserActor(r.ctx, owner)
	failed := func(err error) { r.leave(appID, "it could not be tied: "+err.Error()) }
	if account != "" {
		r.alreadyTied++
	} else {
		sites, err := r.sitesFor(app)
		if err != nil {
			failed(err)
			return
		}
		site, reason := siteForApplication(app, sites)
		if reason != "" {
			r.leave(appID, reason)
			return
		}
		account = trimmed(site["accountId"])
		if siteID == "" {
			siteID = bareID(trimmed(site["id"]))
		}
		fresh, err := r.tieRow(merchant, applicationConcept, appID, app, account,
			func() (map[string]any, error) { return r.one("applicationById", "applicationId", appID) },
			func(row map[string]any) (string, map[string]any) {
				return "backfillApplicationRow", map[string]any{
					"applicationId":  appID,
					"ownerUserId":    userRef(trimmed(row["ownerUserId"])),
					"storeId":        trimmed(row["storeId"]),
					"siteId":         trimmed(row["siteId"]),
					"companyName":    asString(row["companyName"]),
					"applicantName":  asString(row["applicantName"]),
					"applicantEmail": asString(row["applicantEmail"]),
					"accountId":      account,
				}
			})
		if err != nil {
			failed(err)
			return
		}
		// ANOTHER WRITER TIED IT FIRST: its rows follow that account.
		if a := trimmed(fresh["accountId"]); a != "" {
			account = a
		}
	}
	if err := r.tieDecisions(merchant, app, account); err != nil {
		failed(err)
		return
	}
	if err := r.tieEntitlement(merchant, appID, account); err != nil {
		failed(err)
		return
	}
	if err := r.tieSettings(merchant, storeID, account, siteID); err != nil {
		failed(err)
	}
}

// tieRow writes one row's tie, fenced on the version read. A write that loses
// the fence re-reads the row: a fresh version that already carries an account
// counts as tied, and any other is tied again from the fresh read, a bounded
// number of times. It answers the row as last read.
func (r *backfillRun) tieRow(merchant context.Context, concept, rowID string, row map[string]any, account string,
	reread func() (map[string]any, error), build func(map[string]any) (string, map[string]any)) (map[string]any, error) {
	for attempt := 1; ; attempt++ {
		mutation, args := build(row)
		args["accountId"] = account
		fenced := memql.ContextWithRowVersionFence(merchant, concept, rowID, rowTime(row["createdAt"]))
		err := r.p.write(fenced, mutation, args)
		if err == nil {
			r.tied++
			return row, nil
		}
		if !errors.Is(err, memql.ErrRowVersionChanged) || attempt >= backfillAttempts {
			return nil, err
		}
		fresh, err := reread()
		if err != nil {
			return nil, err
		}
		if fresh == nil {
			return nil, fmt.Errorf("%s %s is gone", concept, rowID)
		}
		if trimmed(fresh["accountId"]) != "" {
			r.tied++
			return fresh, nil
		}
		row = fresh
	}
}

// one reads one row by a single argument as the system, or nil.
func (r *backfillRun) one(query, arg, value string) (map[string]any, error) {
	rows, err := r.p.rowsFor(r.sys, query, map[string]string{arg: value})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// tieDecisions ties each decision on the application and names its merchant.
// X5: every other field is written back exactly as the row held it --
// decidedBy and principalKind included, as arguments -- so the backfill
// changes no decision field.
func (r *backfillRun) tieDecisions(merchant context.Context, app map[string]any, account string) error {
	appID := bareID(trimmed(app["id"]))
	owner := userRef(trimmed(app["ownerUserId"]))
	decisions, err := r.p.rowsForApplication(r.sys, "decisionsForApplication", appID)
	if err != nil {
		return err
	}
	for _, d := range decisions {
		if trimmed(d["accountId"]) != "" {
			r.alreadyTied++
			continue
		}
		decisionID := bareID(trimmed(d["id"]))
		reread := func() (map[string]any, error) {
			rows, err := r.p.rowsForApplication(r.sys, "decisionsForApplication", appID)
			for _, row := range rows {
				if bareID(trimmed(row["id"])) == decisionID {
					return row, err
				}
			}
			return nil, err
		}
		if _, err := r.tieRow(merchant, decisionConcept, decisionID, d, account, reread,
			func(row map[string]any) (string, map[string]any) {
				return "backfillDecisionRow", map[string]any{
					"decisionId":    decisionID,
					"applicationId": trimmed(row["applicationId"]),
					"ownerUserId":   owner,
					"storeId":       trimmed(row["storeId"]),
					"transition":    trimmed(row["transition"]),
					"decidedBy":     trimmed(row["decidedBy"]),
					"principalKind": trimmed(row["principalKind"]),
					"note":          asString(row["note"]),
				}
			}); err != nil {
			return err
		}
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
	// The row it read is the row it ties, at that row's own id.
	entID := bareID(trimmed(ent["id"]))
	if entID == "" {
		entID = EntitlementRowID(appID)
	}
	_, err = r.tieRow(merchant, entitlementConcept, entID, ent, account,
		func() (map[string]any, error) { return r.p.entitlementRow(r.sys, appID) },
		func(row map[string]any) (string, map[string]any) {
			created, _ := row["customerCreated"].(bool)
			args := map[string]any{
				"entitlementId":     entID,
				"ownerUserId":       userRef(trimmed(row["ownerUserId"])),
				"applicationId":     trimmed(row["applicationId"]),
				"storeId":           trimmed(row["storeId"]),
				"buyerEmail":        asString(row["buyerEmail"]),
				"adapter":           trimmed(row["adapter"]),
				"state":             trimmed(row["state"]),
				"reference":         asString(row["reference"]),
				"error":             asString(row["error"]),
				"errorCode":         trimmed(row["errorCode"]),
				"shopifyCustomerId": trimmed(row["shopifyCustomerId"]),
				"customerCreated":   created,
			}
			// A TIE IS NOT AN ATTEMPT: the direction and the attempt the row holds
			// are carried forward as they are, and a row that holds none is left
			// holding none -- never given one the backfill would have to invent.
			if d := trimmed(row["direction"]); d != "" {
				args["direction"] = d
			}
			if n := attemptOf(row); n > 0 {
				args["attempt"] = n
			}
			return "backfillEntitlementRow", args
		})
	return err
}

// tieSettings ties the store's settings once per run, with the site they were
// made for: the one they name, or else the application's.
//
// ONCE PER STORE, whichever spelling of its id an application holds (the memo
// is keyed by the bare id). The row tied is the store's one row, the one every
// path reads (settingsRowAt); ANY OTHER settings row of the store -- held under
// the other spelling -- is reported with UntiedSecondSettings for the operator
// to remove, and never tied, merged or deleted here.
func (r *backfillRun) tieSettings(merchant context.Context, storeID, account, siteID string) error {
	key := bareID(storeID)
	if r.settings[key] {
		return nil
	}
	rows, err := r.p.storeSettingsRows(r.sys, storeID)
	if err != nil || len(rows) == 0 {
		return err
	}
	r.settings[key] = true
	for _, other := range rows[1:] {
		r.leave(settingsRowIDOf(other, storeID), UntiedSecondSettings)
	}
	settings := rows[0]
	if trimmed(settings["accountId"]) != "" {
		r.alreadyTied++
		return nil
	}
	// THE ROW IT READ IS THE ROW IT TIES: at that row's own id, with that row's
	// own storeId, never an id rebuilt from the application's store id.
	rowID := settingsRowIDOf(settings, storeID)
	_, err = r.tieRow(merchant, settingsConcept, rowID, settings, account,
		func() (map[string]any, error) { return r.p.settingsRowByID(r.sys, rowID) },
		func(row map[string]any) (string, map[string]any) {
			site := siteID
			if named := trimmed(row["siteId"]); named != "" {
				site = named
			}
			rowStore := trimmed(row["storeId"])
			if rowStore == "" {
				rowStore = storeID
			}
			open, _ := row["applicationsOpen"].(bool)
			return "backfillSettingsRow", map[string]any{
				"settingsId":         rowID,
				"ownerUserId":        userRef(trimmed(row["ownerUserId"])),
				"storeId":            rowStore,
				"applicationsOpen":   open,
				"entitlementAdapter": trimmed(row["entitlementAdapter"]),
				"introText":          asString(row["introText"]),
				"siteId":             site,
			}
		})
	return err
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
// why it cannot be told: its siteId site's, when that site serves the
// application's store, or, with no siteId, the one site whose serving or
// preview binding is the application's store.
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
	store := bareID(trimmed(app["storeId"]))
	withAccount := func(site map[string]any) (map[string]any, string) {
		if trimmed(site["accountId"]) == "" {
			return nil, UntiedNoAccount
		}
		return site, ""
	}
	// ITS OWN SITE, held to the same test as a site found by its store: a
	// siteId naming a site that does not serve the store would tie the
	// application, and through it the store's settings, to a foreign
	// organization.
	if siteID := bareID(trimmed(app["siteId"])); siteID != "" {
		for _, site := range sites {
			if bareID(trimmed(site["id"])) == siteID {
				if !servesStore(site, store) {
					return nil, UntiedForeignSite
				}
				return withAccount(site)
			}
		}
	}
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
