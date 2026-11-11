package wholesalepack

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// reads.go -- THE READS THE WHOLESALE SECTION OPENS (U6): the list, one
// application, the decision log and the buyers. export.go is the fifth.
//
// # Who is admitted, and what is read as whom
//
// Every one of them is gated by the wholesale part. The organization gate
// judges a store-wide read against the SITE's organization and wholesaleGet
// against the APPLICATION's; a store-wide read then checks the site serves the
// store and belongs to the organization its settings are tied to
// (siteForStore) before it reads anything keyed by the store.
//
// THE APPLICATIONS AND THE DECISION LOG ARE READ UNDER THE CALLER. The row gate
// decides which applications a person sees, and a person who cannot read an
// application learns nothing of it here: no row, no count, no line in the log.
//
// AN ADMITTED APPLICATION'S STATE IS JUDGED AGAINST ITS WHOLE LOG (X27). Once
// the caller's own read returned an application, its decisions are folded and
// its entitlement read as the deployment, as wholesaleRecordDecision and
// wholesaleApplicationState do -- a decision written before the account tie
// names no merchant, and a fold over the part the caller reads would show a
// rejected application as ready to review. So the list, the buyers,
// wholesaleGet and wholesaleApplicationState always agree.
//
// A PERSON'S NAME is read through @serverOnly userByIdSystem, at internal
// origin inline on that one read, as the deployment, and only after the
// caller was admitted. Somebody who cannot be found has an empty name.
//
// EVERY REPLY NAMES AN APPLICATION BY THE ROW'S OWN id, as the engine answers
// it, whichever form the caller named it in.

const (
	defaultReadLimit = 50
	maxReadLimit     = 200
	// maxStorePages bounds a read over a store's applications: twenty pages of
	// applicationsForStore's fifty, a thousand applications. Past it a reply
	// says truncated.
	maxStorePages = 20
	// stageAll lists every stage.
	stageAll = "all"
	// AccessNone is a buyer's access when nothing was ever granted.
	AccessNone = "none"
	// extensionForm is the pack's form a client's own fields extend.
	extensionForm = "application"
)

// Stages is every stage an application is shown in, in the order a reader
// meets them.
var Stages = []string{StageWaitingForTerms, StageReadyToReview, StateApproved, StateRejected, StateRevoked}

// The acts the Wholesale section offers on one application.
const (
	ActReject      = "reject"
	ActApprove     = "approve"
	ActRemove      = "remove"
	ActRetryAccess = "retryAccess"
)

// extensionReader is the engine's read of a client's own fields on a row of
// the pack's form (memql.MemQLEngine.ShopperExtensionRow).
type extensionReader interface {
	ShopperExtensionRow(ctx context.Context, pack, form, rowID string) (memql.ExtensionRow, bool, error)
}

// ActsInput is what the acts on one application depend on.
type ActsInput struct {
	Stage, AccessState          string
	EntitlementExists, Operator bool
}

// ActsFor is the acts open on one application.
//
// A person of the store decides a waiting application (reject; approve once it
// is ready), removes an approved buyer, and retries access that did not land:
// on an approved application whose access is not granted, or a revoked one
// whose entitlement is still not revoked. A cluster operator decides nothing
// (X4) and is offered the retry under the same two conditions and nothing else
// (ruling 30): a retry is not a decision (X26).
func ActsFor(in ActsInput) []string {
	retry := (in.Stage == StateApproved && in.AccessState != EntitlementGranted) ||
		(in.Stage == StateRevoked && in.EntitlementExists && in.AccessState != EntitlementRevoked)
	acts := []string{}
	if in.Operator {
		if retry {
			acts = append(acts, ActRetryAccess)
		}
		return acts
	}
	switch in.Stage {
	case StageWaitingForTerms:
		acts = append(acts, ActReject)
	case StageReadyToReview:
		acts = append(acts, ActReject, ActApprove)
	case StateApproved:
		acts = append(acts, ActRemove)
		if retry {
			acts = append(acts, ActRetryAccess)
		}
	case StateRevoked:
		if retry {
			acts = append(acts, ActRetryAccess)
		}
	}
	return acts
}

// staged is one application the caller reads, with the state its whole log
// gives it.
type staged struct {
	row          map[string]any
	id           string // the row's own id, as the engine answers it
	state, stage string
	at           time.Time
}

// storeApplications reads the store's applications under ctx, newest first,
// in every form a store id is held in, at most maxStorePages pages, and says
// whether it stopped short -- in which case the OLDEST are left out. limited
// false reads every page.
func (p *Provider) storeApplications(ctx context.Context, storeID string, limited bool) ([]map[string]any, bool, error) {
	var out []map[string]any
	seen := map[string]bool{}
	cursor := ""
	for page := 0; !limited || page < maxStorePages; page++ {
		rows, next, err := p.rowsPageWithList(ctx, "applicationsForStore", map[string]string{"storeId": storeID},
			"storeIds", storeIDForms(storeID), cursor)
		if err != nil {
			return nil, false, err
		}
		for _, row := range rows {
			if id := trimmed(row["id"]); id != "" && !seen[id] {
				seen[id] = true
				out = append(out, row)
			}
		}
		if next == "" {
			return out, false, nil
		}
		cursor = next
	}
	return out, true, nil
}

// storeIDForms is every form a store id is held in, each once.
func storeIDForms(storeID string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range storeForms(strings.TrimSpace(storeID)) {
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// stagedApplications is the store's applications the caller reads, each staged
// by its whole log, read as the deployment (X27).
func (p *Provider) stagedApplications(ctx context.Context, storeID string) ([]staged, bool, error) {
	apps, truncated, err := p.storeApplications(ctx, storeID, true)
	if err != nil {
		return nil, false, err
	}
	ids := make([]string, 0, len(apps))
	for _, app := range apps {
		ids = append(ids, trimmed(app["id"]))
	}
	logs, err := p.rowsForApplications(auth.ContextWithSystemActor(ctx, provisionActor), "decisionsForApplicationIds", ids)
	if err != nil {
		return nil, false, err
	}
	out := make([]staged, 0, len(apps))
	for _, app := range apps {
		id := trimmed(app["id"])
		if id == "" {
			continue
		}
		state := FoldState(logs[bareID(id)])
		out = append(out, staged{row: app, id: id, state: state, stage: StageOf(state, false, false), at: rowTime(app["createdAt"])})
	}
	return out, truncated, nil
}

// list answers wholesaleList.
func (p *Provider) list(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID, siteID, err := p.admitStore(ctx, args)
	if err != nil {
		return nil, err
	}
	stage := trimmed(args["stage"])
	if stage == "" {
		stage = StageReadyToReview
	}
	if stage != stageAll && !knownStage(stage) {
		return nil, fmt.Errorf("wholesale: %q is not a stage; a stage is %s or %s", stage, strings.Join(Stages, ", "), stageAll)
	}
	search := searchOf(args)
	key := readKey("list", storeID, siteID, stage, search)
	cursor, err := readCursor(trimmed(args["after"]), key)
	if err != nil {
		return nil, err
	}
	apps, truncated, err := p.stagedApplications(ctx, storeID)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, s := range Stages {
		counts[s] = 0
	}
	var matched []staged
	for _, a := range apps {
		counts[a.stage]++
		if stage != stageAll && a.stage != stage {
			continue
		}
		if !searchMatches(search, a.row["companyName"], a.row["applicantName"], a.row["applicantEmail"]) {
			continue
		}
		matched = append(matched, a)
	}
	// The queue is oldest first: how long an application has waited is what
	// the store's people answer to. Every other view is newest first.
	oldestFirst := stage == StageReadyToReview
	page, next := pageOf(matched, cursor, limitOf(args), oldestFirst, key)
	rows := make([]map[string]any, 0, len(page))
	for _, a := range page {
		primary, secondary, err := p.displayFacts(ctx, a.row)
		if err != nil {
			return nil, err
		}
		rows = append(rows, map[string]any{
			"applicationId":  a.id,
			"companyName":    asString(a.row["companyName"]),
			"applicantName":  asString(a.row["applicantName"]),
			"applicantEmail": asString(a.row["applicantEmail"]),
			"stage":          a.stage,
			"primary":        primary,
			"secondary":      secondary,
			"createdAt":      timeText(a.row["createdAt"]),
		})
	}
	return confirm("wholesale:list", map[string]any{"rows": rows, "counts": counts, "next": next, "truncated": truncated})
}

// get answers wholesaleGet.
func (p *Provider) get(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	applicationID := trimmed(args["applicationId"])
	if applicationID == "" {
		return nil, fmt.Errorf("wholesale: applicationId is required")
	}
	// THE CALLER READS THE APPLICATION FIRST; one who cannot is answered
	// nothing, and nothing is read as the deployment for them.
	rows, err := p.rowsFor(ctx, "applicationById", map[string]string{"applicationId": applicationID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("wholesale: application %q is not readable by this caller", applicationID)
	}
	app := rows[0]
	sys := auth.ContextWithSystemActor(ctx, provisionActor)
	log, err := p.rowsForApplication(sys, "decisionsForApplication", applicationID)
	if err != nil {
		return nil, err
	}
	ent, err := p.entitlementRow(sys, applicationID)
	if err != nil {
		return nil, err
	}
	state := FoldState(log)
	stage := StageOf(state, false, false)
	people := p.people(ctx)
	decisions := make([]map[string]any, 0, len(log))
	for _, d := range log {
		decisions = append(decisions, map[string]any{
			"transition":    trimmed(d["transition"]),
			"decidedBy":     trimmed(d["decidedBy"]),
			"decidedByName": people.name(trimmed(d["decidedBy"])),
			"note":          asString(d["note"]),
			"createdAt":     timeText(d["createdAt"]),
		})
	}
	details, err := p.details(ctx, app)
	if err != nil {
		return nil, err
	}
	access := AccessNone
	var entitlement any
	if ent != nil {
		access = trimmed(ent["state"])
		customerCreated, _ := ent["customerCreated"].(bool)
		entitlement = map[string]any{
			"state":             access,
			"errorCode":         trimmed(ent["errorCode"]),
			"error":             asString(ent["error"]),
			"adapter":           trimmed(ent["adapter"]),
			"shopifyCustomerId": trimmed(ent["shopifyCustomerId"]),
			"customerCreated":   customerCreated,
			"updatedAt":         timeText(ent["createdAt"]),
		}
	}
	operator := isClusterOperator(ctx)
	addedBy := trimmed(app["addedBy"])
	return confirm("wholesale:application", map[string]any{
		"application": map[string]any{
			"id":             trimmed(app["id"]),
			"storeId":        trimmed(app["storeId"]),
			"siteId":         trimmed(app["siteId"]),
			"companyName":    asString(app["companyName"]),
			"applicantName":  asString(app["applicantName"]),
			"applicantEmail": asString(app["applicantEmail"]),
			"source":         sourceOf(app),
			"addedBy":        addedBy,
			"addedByName":    people.name(addedBy),
			"createdAt":      timeText(app["createdAt"]),
		},
		"state":       state,
		"stage":       stage,
		"details":     details,
		"decisions":   decisions,
		"entitlement": entitlement,
		"operator":    operator,
		"acts":        ActsFor(ActsInput{Stage: stage, AccessState: access, EntitlementExists: ent != nil, Operator: operator}),
	})
}

// logCursor is where the decision log's next page starts: a page of
// decisionsForStore and how many of its rows were already shown.
type logCursor struct {
	Version int    `json:"v"`
	Key     string `json:"key"`
	Page    string `json:"page"`
	Offset  int    `json:"offset"`
}

// decisionLog answers wholesaleDecisionLog.
func (p *Provider) decisionLog(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID, siteID, err := p.admitStore(ctx, args)
	if err != nil {
		return nil, err
	}
	search := searchOf(args)
	limit := limitOf(args)
	key := readKey("decisionLog", storeID, siteID, "", search)
	at := logCursor{Version: 1, Key: key}
	if after := trimmed(args["after"]); after != "" {
		raw, err := base64.RawURLEncoding.DecodeString(after)
		if err != nil || len(after) > 4096 || json.Unmarshal(raw, &at) != nil || at.Version != 1 || at.Key != key || at.Offset < 0 {
			return nil, fmt.Errorf("wholesale: this page cursor does not match the decision log")
		}
	}
	// Each decision is shown only beside an application the caller reads:
	// the join is the caller's own read of it, by its bare id.
	apps := map[string]map[string]any{}
	appOf := func(applicationID string) (map[string]any, error) {
		bare := bareID(applicationID)
		if app, seen := apps[bare]; seen {
			return app, nil
		}
		rows, err := p.rowsFor(ctx, "applicationById", map[string]string{"applicationId": bare})
		if err != nil {
			return nil, err
		}
		var app map[string]any
		if len(rows) > 0 {
			app = rows[0]
		}
		apps[bare] = app
		return app, nil
	}
	people := p.people(ctx)
	rows := []map[string]any{}
	next := ""
	for page := 0; ; page++ {
		if page == maxStorePages {
			next = encodeLogCursor(at)
			break
		}
		decisions, following, err := p.rowsPageWithList(ctx, "decisionsForStore", map[string]string{"storeId": storeID},
			"storeIds", storeIDForms(storeID), at.Page)
		if err != nil {
			return nil, err
		}
		full := false
		for i := at.Offset; i < len(decisions); i++ {
			if len(rows) == limit {
				next, full = encodeLogCursor(logCursor{Version: 1, Key: key, Page: at.Page, Offset: i}), true
				break
			}
			d := decisions[i]
			app, err := appOf(trimmed(d["applicationId"]))
			if err != nil {
				return nil, err
			}
			if app == nil {
				continue
			}
			decider := trimmed(d["decidedBy"])
			name := people.name(decider)
			if !searchMatches(search, app["companyName"], app["applicantName"], app["applicantEmail"], name, d["note"]) {
				continue
			}
			rows = append(rows, map[string]any{
				"applicationId": trimmed(app["id"]),
				"companyName":   asString(app["companyName"]),
				"transition":    trimmed(d["transition"]),
				"decidedBy":     decider,
				"decidedByName": name,
				"note":          asString(d["note"]),
				"createdAt":     timeText(d["createdAt"]),
			})
		}
		if full {
			break
		}
		if following == "" {
			break
		}
		at = logCursor{Version: 1, Key: key, Page: following}
		if len(rows) == limit {
			next = encodeLogCursor(at)
			break
		}
	}
	return confirm("wholesale:decisionLog", map[string]any{"rows": rows, "next": next})
}

func encodeLogCursor(c logCursor) string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// buyers answers wholesaleBuyers.
func (p *Provider) buyers(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID, siteID, err := p.admitStore(ctx, args)
	if err != nil {
		return nil, err
	}
	search := searchOf(args)
	key := readKey("buyers", storeID, siteID, "", search)
	cursor, err := readCursor(trimmed(args["after"]), key)
	if err != nil {
		return nil, err
	}
	apps, truncated, err := p.stagedApplications(ctx, storeID)
	if err != nil {
		return nil, err
	}
	var matched []staged
	for _, a := range apps {
		if a.state == StateApproved && searchMatches(search, a.row["companyName"], a.row["applicantName"], a.row["applicantEmail"]) {
			matched = append(matched, a)
		}
	}
	page, next := pageOf(matched, cursor, limitOf(args), false, key)
	sys := auth.ContextWithSystemActor(ctx, provisionActor)
	rows := make([]map[string]any, 0, len(page))
	for _, a := range page {
		ent, err := p.entitlementRow(sys, a.id)
		if err != nil {
			return nil, err
		}
		access, code, grantedAt, customer := AccessNone, "", "", ""
		if ent != nil {
			access, code, customer = trimmed(ent["state"]), trimmed(ent["errorCode"]), trimmed(ent["shopifyCustomerId"])
			if access == EntitlementGranted {
				grantedAt = timeText(ent["createdAt"])
			}
		}
		rows = append(rows, map[string]any{
			"applicationId":     a.id,
			"companyName":       asString(a.row["companyName"]),
			"applicantEmail":    asString(a.row["applicantEmail"]),
			"accessState":       access,
			"errorCode":         code,
			"grantedAt":         grantedAt,
			"shopifyCustomerId": customer,
		})
	}
	return confirm("wholesale:buyers", map[string]any{"rows": rows, "next": next, "truncated": truncated})
}

// admitStore reads a store-wide read's store and site and checks the site is
// the store's own (siteForStore), before anything keyed by the store is read.
func (p *Provider) admitStore(ctx context.Context, args map[string]any) (storeID, siteID string, err error) {
	storeID, siteID = trimmed(args["storeId"]), trimmed(args["siteId"])
	if storeID == "" {
		return "", "", fmt.Errorf("wholesale: storeId is required")
	}
	if _, err := p.siteForStore(ctx, siteID, storeID); err != nil {
		return "", "", err
	}
	return storeID, siteID, nil
}

// details is a client's own fields on one application, read under ctx.
//
// none for an application the store's people added (no shopper filled in a
// client's fields) or a store whose form no client extends; unreadable when the
// caller reads no row there and the deployment does; shown otherwise.
func (p *Provider) details(ctx context.Context, app map[string]any) (map[string]any, error) {
	fields := []map[string]any{}
	if sourceOf(app) == SourceStaff || p.extensions == nil || !p.extensionDeclared() {
		return map[string]any{"state": "none", "fields": fields}, nil
	}
	id := bareID(trimmed(app["id"]))
	row, ok, err := p.extensions.ShopperExtensionRow(ctx, Domain, extensionForm, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		_, exists, err := p.extensions.ShopperExtensionRow(auth.ContextWithSystemActor(ctx, provisionActor), Domain, extensionForm, id)
		if err != nil {
			return nil, err
		}
		if exists {
			return map[string]any{"state": "unreadable", "fields": fields}, nil
		}
	}
	for _, f := range row.Fields {
		fields = append(fields, map[string]any{"name": f.Name, "label": f.Label, "value": f.Value})
	}
	return map[string]any{"state": "shown", "fields": fields}, nil
}

// displayFacts is the two facts a client's own fields show first for one
// application, read under ctx; empty when there are none to read.
func (p *Provider) displayFacts(ctx context.Context, app map[string]any) (string, string, error) {
	if sourceOf(app) == SourceStaff || p.extensions == nil || !p.extensionDeclared() {
		return "", "", nil
	}
	row, ok, err := p.extensions.ShopperExtensionRow(ctx, Domain, extensionForm, bareID(trimmed(app["id"])))
	if err != nil || !ok {
		return "", "", err
	}
	return row.Primary, row.Secondary, nil
}

// extensionDeclared reports whether a client extends the pack's form.
func (p *Provider) extensionDeclared() bool {
	if p.hasExtension != nil {
		return p.hasExtension()
	}
	return memql.ShopperExtensionFor(Domain, extensionForm) != nil
}

// sourceOf is how an application arrived; one naming none came through the
// storefront.
func sourceOf(app map[string]any) string {
	if trimmed(app["source"]) == SourceStaff {
		return SourceStaff
	}
	return SourceStorefront
}

// peopleNames answers people's names for one call, each read once.
type peopleNames struct {
	p     *Provider
	ctx   context.Context
	names map[string]string
}

// people is the names reader of one call. Its caller has been admitted.
func (p *Provider) people(ctx context.Context) *peopleNames {
	return &peopleNames{p: p, ctx: auth.ContextWithSystemActor(ctx, provisionActor), names: map[string]string{}}
}

// name is what a person is called, or empty when they cannot be found -- never
// their id.
func (n *peopleNames) name(userID string) string {
	userID = strings.TrimSpace(userID)
	// Only a person's id names a person: bare, or in the user concept's form.
	if userID == "" || (strings.Contains(userID, ":") && !strings.HasPrefix(userID, userIDPrefix)) {
		return ""
	}
	bare := bareID(userID)
	if name, seen := n.names[bare]; seen {
		return name
	}
	name := ""
	rows, err := n.p.rowsFor(auth.ContextWithInternalOrigin(n.ctx), "userByIdSystem", map[string]string{"userId": bare})
	if err == nil && len(rows) > 0 {
		name = strings.TrimSpace(asString(rows[0]["displayName"]))
		if name == "" {
			name = strings.TrimSpace(strings.TrimSpace(asString(rows[0]["firstName"])) + " " + strings.TrimSpace(asString(rows[0]["lastName"])))
		}
	}
	n.names[bare] = name
	return name
}

func knownStage(stage string) bool {
	for _, s := range Stages {
		if s == stage {
			return true
		}
	}
	return false
}

func searchOf(args map[string]any) string { return strings.ToLower(trimmed(args["search"])) }

// searchMatches reports whether search is a part of any one of values.
func searchMatches(search string, values ...any) bool {
	if search == "" {
		return true
	}
	for _, v := range values {
		if strings.Contains(strings.ToLower(asString(v)), search) {
			return true
		}
	}
	return false
}

// limitOf is a page size: defaultReadLimit when absent or not positive, and
// maxReadLimit for anything larger, however large.
func limitOf(args map[string]any) int {
	var n float64
	switch v := args["limit"].(type) {
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case float64:
		n = v
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return defaultReadLimit
		}
		n = f
	default:
		return defaultReadLimit
	}
	switch {
	case math.IsNaN(n) || n < 1:
		return defaultReadLimit
	case n > maxReadLimit:
		return maxReadLimit
	}
	return int(n)
}

// timeText is a row time as RFC 3339 text, or empty.
func timeText(v any) string {
	at := rowTime(v)
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// listCursor is a keyset position in a list, bound to the list it was issued
// for. A deliberate copy of packs/reviewspack/directory_cursor.go's cursor (a
// pack may not import another pack), without its offset form.
type listCursor struct {
	Version int       `json:"v"`
	Key     string    `json:"key"`
	At      time.Time `json:"at"`
	ID      string    `json:"id"`
}

// readKey binds a cursor to the list and the filters it was issued for.
func readKey(kind, store, site, stage, search string) string {
	raw, _ := json.Marshal([]string{kind, store, site, stage, search})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func readCursor(after, key string) (*listCursor, error) {
	if after == "" {
		return nil, nil
	}
	fail := fmt.Errorf("wholesale: this page cursor does not match the list")
	if len(after) > 2048 {
		return nil, fail
	}
	raw, err := base64.RawURLEncoding.DecodeString(after)
	var c listCursor
	if err != nil || json.Unmarshal(raw, &c) != nil || c.Version != 1 || c.Key != key || c.ID == "" {
		return nil, fail
	}
	return &c, nil
}

// pageOf sorts entries, starts after cursor and takes limit of them, answering
// the cursor of the next page ("" when this one is the last).
func pageOf(entries []staged, cursor *listCursor, limit int, oldestFirst bool, key string) ([]staged, string) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].at.Equal(entries[j].at) {
			return entries[i].id < entries[j].id
		}
		if oldestFirst {
			return entries[i].at.Before(entries[j].at)
		}
		return entries[i].at.After(entries[j].at)
	})
	if cursor != nil {
		rest := entries[:0:0]
		for _, e := range entries {
			if e.at.Equal(cursor.At) && e.id > cursor.ID ||
				(oldestFirst && e.at.After(cursor.At)) || (!oldestFirst && e.at.Before(cursor.At)) {
				rest = append(rest, e)
			}
		}
		entries = rest
	}
	if len(entries) <= limit {
		return entries, ""
	}
	last := entries[limit-1]
	raw, _ := json.Marshal(listCursor{Version: 1, Key: key, At: last.at, ID: last.id})
	return entries[:limit], base64.RawURLEncoding.EncodeToString(raw)
}
