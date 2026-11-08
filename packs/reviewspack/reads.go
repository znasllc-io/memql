package reviewspack

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
// exception is who can moderate the store -- the organization's roster under
// reviewsPeople, and the one person a settings write names as removal
// approver -- read as the deployment once the caller is admitted (moderatorsIn
// says why).

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

// maxPeople bounds how many of the people who can moderate a store are listed:
// the bound integrations/groups/people.go puts on its roster, put here on the
// LIST, once every member has been judged and the list sorted by what each is
// called (moderatorsIn) -- never on the members before, where it kept the
// first ids whoever they were. reviewsPeople says when it cut (truncated).
//
// The number is also prose: reviewsPeople's description in dsl/builtins.memql
// says "at most 100 of them", and changes with it.
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

// called is what a person is called on a list: their name, else their address,
// as the admin panel names them (personCalled).
func (m Moderator) called() string {
	if m.DisplayName != "" {
		return m.DisplayName
	}
	return m.Email
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
// reviews, each saying whether they are its removal approver, by what each is
// called and at most maxPeople of them (moderatorsIn). The v1:reviews:people
// receipt always carries truncated: true when more people can moderate the
// store than are listed, so a client never takes a cut list for everyone. A
// person left off can still be named approver; the write asks about them
// directly.
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
	people, truncated, err := p.moderatorsIn(ctx, storeOrganization(settings, asString(site["accountId"])))
	if err != nil {
		return nil, err
	}
	for i := range people {
		people[i].MayRemove = namesApprover(settings, people[i].UserID)
	}
	return receipt("v1:reviews:people", "people-"+slugOf(storeID), map[string]any{
		"storeId":   storeID,
		"people":    people,
		"truncated": truncated,
	})
}

// storeOrganization is the organization a store's reviews belong to: the one
// its settings record, else its site's. siteForStore has already refused a
// site of another organization than the settings name.
//
// ONE RULE FOR THE PICKER AND THE WRITE. reviewsPeople offers the people of
// this organization, and writeSettings asks of a named removal approver whether
// they can moderate in this same one (moderatorNamed) -- so a settings row
// written before the organization was stamped (stampOrganization), which
// carries none, takes its site's for both, and the picker never offers
// somebody the write refuses.
func storeOrganization(settings map[string]any, siteAccount string) string {
	if acct := bareAccount(asString(settings["accountId"])); acct != "" {
		return acct
	}
	return bareAccount(siteAccount)
}

// moderatorsIn answers the people who can moderate a store's reviews in its
// organization: every active member of any of the organization's groups on
// whom canModerate's verdict is yes, at most maxPeople of them, and whether
// there were more (truncated). A store of no organization has nobody.
//
// BY WHAT THEY ARE CALLED: their name, else their address (called), whatever
// its case, then by id. A key that put the name before the address sorted
// everybody with no name ahead of everybody with one, and broke a tie between
// two people called the same on their addresses.
//
// THE LIST IS CUT, NOT THE ORGANIZATION. Every member is judged, and only the
// list of those who can moderate is sorted and cut, so the people kept are the
// first as a person reads the list. Cut before the judging, the bound kept the
// first ids whoever they were: operators, stand-ins and people without the
// part could fill it and leave nobody on the list, with nothing to say so.
//
// THE ROSTER IS THE DEPLOYMENT'S READ, not the caller's. Who can moderate a
// store is a fact about the store, and answering it from what this caller may
// see would let a moderator and an operator get different answers -- and the
// approver check different verdicts. So the organization's groups, their
// members and each member's user row are read as the deployment (peopleReads),
// the way integrations/groups/people.go reads an organization's people, and
// only after the organization gate admitted the caller to this store
// (reviewsPeople). The approver check reads the one person it is asked about
// the same way (moderatorNamed).
//
// ANSWERED IN FULL OR NOT AT ALL. The engine's readers answer from what they
// could read when a read fails, which a gate wants; a list of who can moderate
// drawn from part of the grants could name somebody a deny it never read has
// barred. Any failed read under the answer refuses it.
func (p *Provider) moderatorsIn(ctx context.Context, account string) ([]Moderator, bool, error) {
	account = bareAccount(account)
	if account == "" {
		return []Moderator{}, false, nil
	}
	resolver, err := p.organizationResolver()
	if err != nil {
		return nil, false, err
	}
	sys, resolving, failures := peopleReads(ctx)
	members, err := p.organizationMembers(sys, account)
	if err != nil {
		return nil, false, err
	}
	out := []Moderator{}
	for _, userID := range members {
		m, moderates, err := p.canModerate(sys, resolving, resolver, account, userID)
		if err != nil {
			return nil, false, fmt.Errorf("reviews: reading the organization's people: %w", err)
		}
		if moderates {
			out = append(out, m)
		}
	}
	if err := failures.Err(); err != nil {
		return nil, false, fmt.Errorf("reviews: a read behind who can moderate this store failed, so it cannot be answered: %w", err)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].called()), strings.ToLower(out[j].called())
		if a != b {
			return a < b
		}
		return out[i].UserID < out[j].UserID
	})
	if len(out) > maxPeople {
		return out[:maxPeople], true, nil
	}
	return out, false, nil
}

// organizationMembers is every active member of every active group of the
// organization, bare (bareUser) and sorted: the people
// integrations/groups/people.go collects. All of them, with no bound: who
// among them can moderate is canModerate's to say, and the list moderatorsIn
// draws from them is what maxPeople bounds.
func (p *Provider) organizationMembers(sys context.Context, account string) ([]string, error) {
	groups, err := p.organizationGroups(sys, account)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, group := range groups {
		members, err := p.rowsFor(sys, "membersOfGroup", map[string]string{"groupId": group})
		if err != nil {
			return nil, fmt.Errorf("reviews: reading the organization's members: %w", err)
		}
		for _, member := range members {
			if strings.TrimSpace(asString(member["status"])) != "active" {
				continue
			}
			if id := bareUser(asString(member["userId"])); id != "" {
				ids[id] = true
			}
		}
	}
	out := make([]string, 0, len(ids))
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// organizationGroups is the organization's active groups, by bare id: the
// groups whose members are its people, for the list and the approver check
// alike.
func (p *Provider) organizationGroups(sys context.Context, account string) ([]string, error) {
	groups, err := p.rowsFor(sys, "groupsForAccount", map[string]string{"accountId": account})
	if err != nil {
		return nil, fmt.Errorf("reviews: reading the organization's groups: %w", err)
	}
	out := make([]string, 0, len(groups))
	for _, group := range groups {
		if strings.TrimSpace(asString(group["status"])) != "active" {
			continue
		}
		if id := memql.BareShortId(asString(group["id"])); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// peopleReads is what an answer about who can moderate reads on: the
// deployment's identity for the rows (sys), and the caller's own context for
// the engine, which resolves each person as themselves (resolving) -- both
// under one holder of the reads that failed, so a failure under either is
// heard (ANSWERED IN FULL OR NOT AT ALL, moderatorsIn). The list and the
// approver check open their reads here, and read alike.
func peopleReads(ctx context.Context) (sys, resolving context.Context, failures *auth.ReadFailures) {
	resolving, failures = auth.ContextWithReadFailures(ctx)
	return auth.ContextWithSystemActor(resolving, peopleActor), resolving, failures
}

// canModerate is the verdict on one member of the organization: whether they
// can moderate a store's reviews in account, and who they are when they can --
// their id, name and address, and nothing else of their user row. ONE VERDICT
// FOR THE PICKER AND THE WRITE: moderatorsIn asks it of every member and
// moderatorNamed of the one person a settings write names, so the list and
// the write cannot disagree on whether somebody can moderate.
//
// WHO THEY ARE IS ASKED FIRST, before the engine is: an operator hosts the
// reviews and decides none (moderatorOf), a role test's stand-in is a member
// of the organization's group only while an organization role's test is live
// -- a 30-minute identity that is nobody (ruling R3) -- and a person with no
// active user row is nobody this cluster can name.
//
// CAN MODERATE IS THE ORGANIZATION GATE'S QUESTION, answered by the engine's
// own resolver for them: the three things it asks before any reviews builtin
// runs for them (component/memql, refuseOrganizationActionCapability) -- the
// organization's data read, its Deployables door and the reviews part -- each
// held in THIS organization. The part alone would list a person whose every
// act on the store the gate then refuses.
//
// The user row is @serverOnly: the internal stamp is on that one read, made as
// the deployment (sys). A read that fails is returned as it failed, and each
// caller says what it was reading: the organization's people, or the approver
// named.
func (p *Provider) canModerate(sys, resolving context.Context, resolver organizationDecisions, account, userID string) (Moderator, bool, error) {
	if auth.IsRoleTestUser(userID) {
		return Moderator{}, false, nil
	}
	rows, err := p.rowsFor(auth.ContextWithInternalOrigin(sys), "userByIdSystem", map[string]string{"userId": userID})
	if err != nil {
		return Moderator{}, false, err
	}
	if len(rows) == 0 {
		return Moderator{}, false, nil
	}
	row := rows[0]
	if active, _ := row["active"].(bool); !active {
		return Moderator{}, false, nil
	}
	role := auth.Role(strings.ToLower(strings.TrimSpace(asString(row["role"]))))
	if auth.IsClusterOperator(role) {
		return Moderator{}, false, nil
	}
	known := []auth.Decision{{Verb: auth.VerbExecute, Resource: reviewsPart}}
	if !moderatesIn(resolver.ResolveOrganizationDecisionsFor(resolving, userID, role, known), account) {
		return Moderator{}, false, nil
	}
	return Moderator{UserID: userID, DisplayName: displayNameOf(row), Email: strings.TrimSpace(asString(row["primaryEmail"]))}, true, nil
}

// moderatorNamed answers whether the one person named can moderate a store's
// reviews in account, and who they are when they can: writeSettings' check on
// a removal approver. It resolves that person alone -- their own memberships
// (groupsForUser), one of them active in an active group of the organization,
// which is the set organizationMembers draws the list from, read from the
// person's side; then canModerate's verdict, the list's own. Nobody else's
// membership, user row or decisions is read, so naming an approver costs one
// person's reads however many people the organization has.
//
// Read as the list is read (peopleReads), and only once the organization gate
// admitted the caller to the store and writeSettings' own rule admitted them to
// name an approver. ANSWERED IN FULL OR NOT AT ALL in the same way: any failed
// read under it refuses the answer (moderatorsIn).
func (p *Provider) moderatorNamed(ctx context.Context, account, userID string) (Moderator, bool, error) {
	account, userID = bareAccount(account), bareUser(userID)
	if account == "" || userID == "" {
		return Moderator{}, false, nil
	}
	resolver, err := p.organizationResolver()
	if err != nil {
		return Moderator{}, false, err
	}
	sys, resolving, failures := peopleReads(ctx)
	member, err := p.inOrganization(sys, account, userID)
	if err != nil {
		return Moderator{}, false, err
	}
	m, moderates := Moderator{}, false
	if member {
		if m, moderates, err = p.canModerate(sys, resolving, resolver, account, userID); err != nil {
			return Moderator{}, false, fmt.Errorf("reviews: reading the approver named: %w", err)
		}
	}
	if err := failures.Err(); err != nil {
		return Moderator{}, false, fmt.Errorf("reviews: a read behind whether this person can moderate the store failed, so it cannot be answered: %w", err)
	}
	return m, moderates, nil
}

// inOrganization reports whether one person is an active member of an active
// group of the organization, from their own memberships: the read
// organizationMembers makes from the groups' side, made from theirs.
func (p *Provider) inOrganization(sys context.Context, account, userID string) (bool, error) {
	groups, err := p.organizationGroups(sys, account)
	if err != nil {
		return false, err
	}
	memberships, err := p.rowsFor(sys, "groupsForUser", map[string]string{"userId": userID})
	if err != nil {
		return false, fmt.Errorf("reviews: reading the person's memberships: %w", err)
	}
	for _, m := range memberships {
		// The membership must be the named person's own, as this package
		// spells people (bareUser): a row that names them under another
		// concept's prefix, or the user's twice, is nobody's here, as it is
		// on the list (organizationMembers).
		if strings.TrimSpace(asString(m["status"])) == "active" && bareUser(asString(m["userId"])) == userID &&
			slices.Contains(groups, memql.BareShortId(asString(m["groupId"]))) {
			return true, nil
		}
	}
	return false, nil
}

// moderatesIn reports whether the engine's decisions for one person admit them
// to a store's reviews in account (canModerate says which three).
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
// as clients write ids, bare. Both are made bare by bareUser, so an id of
// another concept, or one with the user's prefix twice, matches no person's.
// Empty names nobody.
func sameUser(a, b string) bool {
	a, b = bareUser(a), bareUser(b)
	return a != "" && a == b
}

// userIDPrefix is a person's canonical id prefix.
const userIDPrefix = "v1:identity:user:"

// bareUser is a user id as clients write it: the user concept's prefix
// stripped, and no other. memql.BareShortId strips ANY concept's prefix, which
// read `v1:accounts:account:mod-1` as the person mod-1; here it stays what it
// is, an id of another concept, and names nobody. An id with the user's prefix
// twice is "": stripped once it is still canonical, which no person is spelled
// as here, so it names nobody either -- never somebody the list offers and the
// write refuses. Every user id this package's people code makes bare goes
// through it.
func bareUser(id string) string {
	bare := bareID(id, userIDPrefix)
	if strings.HasPrefix(bare, userIDPrefix) {
		return ""
	}
	return bare
}
