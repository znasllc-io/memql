package reviewspack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// reads.go -- WHAT THE ADMIN PANEL READS: ONE REVIEW (reviewsGet), THE
// STORE'S DECISION LOG (reviewsDecisionLog), THE SETTINGS (reviewsSettings),
// WHO CAN MODERATE THE STORE (reviewsPeople).
//
// The moderation list answers the queue. A moderator who opens one review
// needs its whole decision log and the acts they may take on it now; the
// store's log answers who decided what across every review, newest first;
// the settings answer what is in force with the defaults applied; and the
// store's people answer who can moderate it, the people its removal approver
// is chosen from.
//
// EACH READ SAYS WHAT THE VIEWER MAY DO, worked out here with the rules the
// writes enforce (ValidateDecision, moderatorOf, writeSettings), so the panel
// offers no act the engine would then refuse. Every read runs under the
// caller's own actor: a caller outside the store reads nothing. The one
// exception is the organization's roster under reviewsPeople, read as the
// deployment once the caller is admitted (moderatorsIn says why).

// LoggedDecision is one entry of a decision log.
type LoggedDecision struct {
	ID        string `json:"id"`
	ReviewID  string `json:"reviewId"`
	Action    string `json:"action"`
	Reason    string `json:"reason,omitempty"`
	Note      string `json:"note,omitempty"`
	DecidedBy string `json:"decidedBy"`
	At        string `json:"at"`
	Revision  int    `json:"revision"`
}

func loggedFrom(d Decision) LoggedDecision {
	return LoggedDecision{
		ID: d.ID, ReviewID: d.ReviewID, Action: d.Action, Reason: d.Reason, Note: d.Note,
		DecidedBy: d.DecidedBy, At: d.At.UTC().Format(time.RFC3339Nano), Revision: max(d.Revision, 1),
	}
}

// newestFirst orders decisions by time, newest first; a tie by id, so a page
// boundary never moves between two reads.
func newestFirst(ds []Decision) []Decision {
	out := append([]Decision(nil), ds...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].At.Equal(out[j].At) {
			return out[i].ID > out[j].ID
		}
		return out[i].At.After(out[j].At)
	})
	return out
}

// viewerOf is who is reading: their id, and whether they host the reviews
// (the cluster's operators) rather than moderate them.
func viewerOf(ctx context.Context) (userID string, operator bool) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok {
		return "", false
	}
	return strings.TrimSpace(ac.UserId), auth.IsClusterOperator(ac.Role)
}

// actsFor are the decisions this viewer may record on a review in this state:
// the ones ValidateDecision accepts. A reason the act needs is assumed given,
// since the panel asks for one before it sends anything.
func actsFor(state State, moderates, approver bool) []string {
	acts := []string{}
	if !moderates {
		return acts
	}
	for _, action := range Actions {
		reason := ""
		switch action {
		case ActionDecline, ActionAskToFix, ActionRemove:
			reason = Reasons[0]
		}
		if ValidateDecision(action, reason, state, approver) == nil {
			acts = append(acts, action)
		}
	}
	return acts
}

// get answers reviewsGet: one review as the list shows it, its whole decision
// log newest first, and the acts the viewer may take on it now.
func (p *Provider) get(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	reviewID := strings.TrimSpace(asString(args["reviewId"]))
	if reviewID == "" {
		return nil, fmt.Errorf("reviews: reviewId is required")
	}
	row, err := p.readableReview(ctx, reviewID)
	if err != nil {
		return nil, err
	}
	decisions, err := p.decisionsForReview(ctx, reviewID)
	if err != nil {
		return nil, err
	}
	state := StateOf(VersionOf(row), decisions)
	review := map[string]any{}
	for k, v := range row {
		review[k] = v
	}
	review["state"] = string(state)
	review["contentHash"] = contentHashOf(row)
	if t := timeText(row["createdAt"]); t != "" {
		review["createdAt"] = t
	}
	log := make([]LoggedDecision, 0, len(decisions))
	for _, d := range newestFirst(decisions) {
		log = append(log, loggedFrom(d))
	}
	userID, operator := viewerOf(ctx)
	approver := false
	if !operator && userID != "" {
		if approver, err = p.isRemovalApprover(ctx, strings.TrimSpace(asString(row["storeId"])), userID); err != nil {
			return nil, err
		}
	}
	return receipt("v1:reviews:reviewRead", slugOf(canonicalReviewID(reviewID)), map[string]any{
		"review":    review,
		"decisions": log,
		"acts":      actsFor(state, !operator && userID != "", approver),
		"moderates": !operator && userID != "",
	})
}

// decisionLog answers reviewsDecisionLog: every decision on the store's
// reviews, newest first, paged. Each entry names the product and headline of
// its review and the state the review is in now -- never the writer, whose
// name and address are the list's and the review's to show.
func (p *Provider) decisionLog(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := strings.TrimSpace(asString(args["storeId"]))
	if storeID == "" {
		return nil, fmt.Errorf("reviews: storeId is required")
	}
	if err := p.requireSiteForStore(ctx, asString(args["siteId"]), storeID); err != nil {
		return nil, err
	}
	limit := asInt(args["limit"])
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	search := strings.ToLower(strings.TrimSpace(asString(args["search"])))
	key := reviewDirectoryKey("decisions", storeID, asString(args["siteId"]), "", search)
	cursor, offset, err := readDirectoryCursor(strings.TrimSpace(asString(args["after"])), key)
	if err != nil {
		return nil, err
	}

	rows, err := p.rowsFor(ctx, "reviewsInStore", map[string]string{"storeId": storeID})
	if err != nil {
		return nil, err
	}
	byID := map[string]map[string]any{}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		id := canonicalReviewID(asString(row["id"]))
		if id == "" {
			continue
		}
		byID[id] = row
		ids = append(ids, id)
	}
	decisions, err := p.decisionsByReview(ctx, storeID, ids)
	if err != nil {
		return nil, err
	}
	all := []Decision{}
	states := map[string]State{}
	for id, ds := range decisions {
		row, known := byID[id]
		if !known {
			continue
		}
		states[id] = StateOf(VersionOf(row), ds)
		all = append(all, ds...)
	}
	all = newestFirst(all)
	matched := all[:0]
	for _, d := range all {
		row := byID[canonicalReviewID(d.ReviewID)]
		if reviewSearchMatches(search, string(d.Action), d.Reason, d.Note, d.DecidedBy, asString(row["title"]), asString(row["productHandle"])) {
			matched = append(matched, d)
		}
	}
	all = matched
	total := len(all)
	if cursor != nil {
		remaining := all[:0]
		for _, d := range all {
			if afterDirectoryCursor(d.At, d.ID, cursor, false, true) {
				remaining = append(remaining, d)
			}
		}
		all = remaining
	}
	available := len(all)
	if offset > available {
		offset = available
	}
	end := min(offset+limit, available)
	entries := make([]map[string]any, 0, end-offset)
	for _, d := range all[offset:end] {
		row := byID[canonicalReviewID(d.ReviewID)]
		logged := loggedFrom(d)
		raw, _ := json.Marshal(logged)
		entry := map[string]any{}
		_ = json.Unmarshal(raw, &entry)
		entry["reviewId"] = canonicalReviewID(d.ReviewID)
		entry["reviewState"] = string(states[canonicalReviewID(d.ReviewID)])
		setIfPresent(entry, "productHandle", strings.TrimSpace(asString(row["productHandle"])))
		setIfPresent(entry, "title", strings.TrimSpace(asString(row["title"])))
		entries = append(entries, entry)
	}
	next := ""
	if end < available {
		last := all[end-1]
		next = writeDirectoryCursor(key, last.At, last.ID)
	}
	return receipt("v1:reviews:decisionLog", fmt.Sprintf("log-%s-%d", slugOf(storeID), offset), map[string]any{
		"storeId":   storeID,
		"decisions": entries,
		"count":     len(entries),
		"total":     total,
		"next":      next,
	})
}

// readSettings answers reviewsSettings: the store's settings with every
// default applied, whether the store has a settings row at all, and what this
// viewer may change -- writeSettings' two rules, read rather than tried.
func (p *Provider) readSettings(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := strings.TrimSpace(asString(args["storeId"]))
	if storeID == "" {
		return nil, fmt.Errorf("reviews: storeId is required; settings are per store")
	}
	if err := p.requireSiteForStore(ctx, asString(args["siteId"]), storeID); err != nil {
		return nil, err
	}
	row, err := p.settingsFor(ctx, storeID)
	if err != nil {
		return nil, err
	}
	eff := EffectiveSettings(row)
	userID, operator := viewerOf(ctx)
	exists := len(row) > 0
	approver := !operator && sameUser(eff.RemovalApproverUserID, userID)
	return receipt("v1:reviews:settingsRead", SettingsRowID(storeID), map[string]any{
		"storeId": storeID,
		"exists":  exists,
		"settings": map[string]any{
			"publicDisplay":           eff.PublicDisplay,
			"linkLifetimeDays":        eff.LinkLifetimeDays,
			"requestDelayDays":        eff.RequestDelayDays,
			"requestsEnabled":         eff.RequestsEnabled,
			"requestTemplateId":       eff.RequestTemplateID,
			"requestSenderIdentityId": eff.RequestSenderIdentityID,
			"notifyEmail":             eff.NotifyEmail,
			"removalApproverUserId":   eff.RemovalApproverUserID,
			"criteriaText":            strings.TrimSpace(asString(row["criteriaText"])),
		},
		"may": map[string]any{
			"edit":         userID != "" && (exists || operator),
			"nameApprover": userID != "" && (operator || approver),
			"nameSender":   userID != "" && operator,
		},
		"viewer": map[string]any{
			"moderates":        !operator && userID != "",
			"approvesRemovals": approver,
		},
	})
}

// ---------------------------------------------------------------------------
// Who can moderate (reviewsPeople)
// ---------------------------------------------------------------------------

// reviewsPart is the Deployables part a store's moderation is, and
// reviewsDoor the app it is a part of.
const (
	reviewsPart = "app:deployables/reviews"
	reviewsDoor = "app:deployables"
)

// peopleActor is the identity an organization's roster is read under: the
// deployment, once the organization gate has admitted the caller.
const peopleActor = "reviews-people"

// maxPeople bounds how many of an organization's people are considered, the
// bound integrations/groups/people.go puts on its roster.
const maxPeople = 100

// Moderator is one person who can moderate a store's reviews. Nothing else of
// their user row leaves this package.
type Moderator struct {
	UserID      string `json:"userId"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	// MayRemove: they are the store's removal approver (isRemovalApprover).
	MayRemove bool `json:"mayRemove"`
}

// organizationDecisions is what the engine answers about a NAMED person in
// each of their organizations (memql.MemQLEngine.ResolveOrganizationDecisionsFor):
// the action guards' own resolver, run for them -- their role, their groups
// filtered to the organization answered, the grants on both. Declared here, as
// integrations/rbac declares its own, so this package's doubles need not
// stand up an engine.
type organizationDecisions interface {
	ResolveOrganizationDecisionsFor(ctx context.Context, userId string, role auth.Role, known []auth.Decision) []memql.OrganizationDecision
}

// organizationResolver is the Provider's, or the engine's.
func (p *Provider) organizationResolver() (organizationDecisions, error) {
	if p.organizations != nil {
		return p.organizations, nil
	}
	if r, ok := p.engine.(organizationDecisions); ok {
		return r, nil
	}
	return nil, fmt.Errorf("reviews: this node cannot resolve who can moderate a store's reviews")
}

// people answers reviewsPeople: the people who can moderate one store's
// reviews, each saying whether they are its removal approver.
func (p *Provider) people(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	storeID := strings.TrimSpace(asString(args["storeId"]))
	if storeID == "" {
		return nil, fmt.Errorf("reviews: storeId is required")
	}
	site, err := p.siteForStore(ctx, asString(args["siteId"]), storeID)
	if err != nil {
		return nil, err
	}
	settings, err := p.settingsFor(ctx, storeID)
	if err != nil {
		return nil, err
	}
	people, err := p.moderatorsIn(ctx, storeOrganization(settings, asString(site["accountId"])))
	if err != nil {
		return nil, err
	}
	for i := range people {
		people[i].MayRemove = namesApprover(settings, people[i].UserID)
	}
	return receipt("v1:reviews:people", "people-"+slugOf(storeID), map[string]any{
		"storeId": storeID,
		"people":  people,
	})
}

// storeOrganization is the organization a store's reviews belong to: the one
// its settings record, else its site's. siteForStore has already refused a
// site of another organization than the settings name.
//
// ONE RULE FOR THE PICKER AND THE WRITE. reviewsPeople offers the people of
// this organization, and writeSettings checks a named removal approver against
// the people of this same one -- so a settings row written before the
// organization was stamped (stampOrganization), which carries none, takes its
// site's for both, and the picker never offers somebody the write refuses.
func storeOrganization(settings map[string]any, siteAccount string) string {
	if acct := bareAccount(asString(settings["accountId"])); acct != "" {
		return acct
	}
	return bareAccount(siteAccount)
}

// moderatorsIn answers the people who can moderate a store's reviews in its
// organization, by name: the active members of any of the organization's
// groups whom the engine answers able to moderate there, never one of the
// cluster's operators and never a role test's stand-in. A store of no
// organization has nobody.
//
// CAN MODERATE IS THE ORGANIZATION GATE'S QUESTION, answered by the engine's
// own resolver for each person: the three things it asks before any reviews
// builtin runs for them (component/memql, refuseOrganizationActionCapability)
// -- the organization's data read, its Deployables door and the reviews part
// -- each held in THIS organization. The part alone would list a person whose
// every act on the store the gate then refuses.
//
// WHO THEY ARE IS DROPPED FIRST, before the engine is asked: an operator hosts
// the reviews and decides none (moderatorOf), and a role test's stand-in is a
// member of the organization's group only while an organization role's test is
// live -- a 30-minute identity that is nobody (ruling R3).
//
// THE ROSTER IS THE DEPLOYMENT'S READ, not the caller's. Who can moderate a
// store is a fact about the store, and answering it from what this caller may
// see would let a moderator and an operator get different answers -- and the
// approver check different verdicts. So the organization's groups, their
// members and each member's user row are read as the deployment, the way
// integrations/groups/people.go reads an organization's people, and only after
// the organization gate admitted the caller to this store (reviewsPeople) or
// writeSettings' own rule admitted them to name an approver. The user row is
// @serverOnly; the internal stamp is on that one read, and nothing of the row
// but the person's id, name and address leaves.
//
// ANSWERED IN FULL OR NOT AT ALL. The engine's readers answer from what they
// could read when a read fails, which a gate wants; a list of who can moderate
// drawn from part of the grants could name somebody a deny it never read has
// barred. Any failed read under the answer refuses it.
func (p *Provider) moderatorsIn(ctx context.Context, account string) ([]Moderator, error) {
	account = bareAccount(account)
	if account == "" {
		return []Moderator{}, nil
	}
	resolver, err := p.organizationResolver()
	if err != nil {
		return nil, err
	}
	sys := auth.ContextWithSystemActor(ctx, peopleActor)
	members, err := p.organizationMembers(sys, account)
	if err != nil {
		return nil, err
	}
	resolving, failures := auth.ContextWithReadFailures(ctx)
	known := []auth.Decision{{Verb: auth.VerbExecute, Resource: reviewsPart}}
	out := []Moderator{}
	for _, userID := range members {
		if auth.IsRoleTestUser(userID) {
			continue
		}
		rows, err := p.rowsFor(auth.ContextWithInternalOrigin(sys), "userByIdSystem", map[string]string{"userId": userID})
		if err != nil {
			return nil, fmt.Errorf("reviews: reading the organization's people: %w", err)
		}
		if len(rows) == 0 {
			continue
		}
		row := rows[0]
		if active, _ := row["active"].(bool); !active {
			continue
		}
		role := auth.Role(strings.ToLower(strings.TrimSpace(asString(row["role"]))))
		if auth.IsClusterOperator(role) {
			continue
		}
		if !moderatesIn(resolver.ResolveOrganizationDecisionsFor(resolving, userID, role, known), account) {
			continue
		}
		out = append(out, Moderator{UserID: userID, DisplayName: displayNameOf(row), Email: strings.TrimSpace(asString(row["primaryEmail"]))})
	}
	if err := failures.Err(); err != nil {
		return nil, fmt.Errorf("reviews: a read behind who can moderate this store failed, so it cannot be answered: %w", err)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].DisplayName+" "+out[i].Email), strings.ToLower(out[j].DisplayName+" "+out[j].Email)
		if a != b {
			return a < b
		}
		return out[i].UserID < out[j].UserID
	})
	return out, nil
}

// organizationMembers is every active member of every active group of the
// organization, bare and sorted, at most maxPeople of them: the people
// integrations/groups/people.go collects.
func (p *Provider) organizationMembers(sys context.Context, account string) ([]string, error) {
	groups, err := p.rowsFor(sys, "groupsForAccount", map[string]string{"accountId": account})
	if err != nil {
		return nil, fmt.Errorf("reviews: reading the organization's groups: %w", err)
	}
	ids := map[string]bool{}
	for _, group := range groups {
		if strings.TrimSpace(asString(group["status"])) != "active" {
			continue
		}
		members, err := p.rowsFor(sys, "membersOfGroup", map[string]string{"groupId": memql.BareShortId(asString(group["id"]))})
		if err != nil {
			return nil, fmt.Errorf("reviews: reading the organization's members: %w", err)
		}
		for _, member := range members {
			if strings.TrimSpace(asString(member["status"])) != "active" {
				continue
			}
			if id := memql.BareShortId(asString(member["userId"])); id != "" {
				ids[id] = true
			}
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	if len(out) > maxPeople {
		out = out[:maxPeople]
	}
	return out, nil
}

// moderatesIn reports whether the engine's decisions for one person admit them
// to a store's reviews in account (moderatorsIn says which three).
func moderatesIn(decisions []memql.OrganizationDecision, account string) bool {
	held := map[auth.VerbResource]bool{}
	for _, d := range decisions {
		if d.Held && bareAccount(d.AccountID) == account {
			held[auth.VerbResource{Verb: d.Verb, Resource: d.Resource}] = true
		}
	}
	return held[auth.VerbResource{Verb: auth.VerbRead, Resource: auth.ResourceData}] &&
		held[auth.VerbResource{Verb: auth.VerbRead, Resource: reviewsDoor}] &&
		held[auth.VerbResource{Verb: auth.VerbExecute, Resource: reviewsPart}]
}

// displayNameOf is what a person is called: their display name, else their
// first and last names, else nothing (a client names them by their address).
func displayNameOf(row map[string]any) string {
	if name := strings.TrimSpace(asString(row["displayName"])); name != "" {
		return name
	}
	return strings.TrimSpace(strings.TrimSpace(asString(row["firstName"])) + " " + strings.TrimSpace(asString(row["lastName"])))
}

// sameUser reports whether two user ids name one person. They arrive in two
// spellings: a signed-in caller's as the token carried it -- canonical,
// `v1:identity:user:<id>`, on a deployed cluster -- and a chosen or stored one
// as clients write ids, bare. Empty names nobody.
func sameUser(a, b string) bool {
	a, b = memql.BareShortId(a), memql.BareShortId(b)
	return a != "" && a == b
}
