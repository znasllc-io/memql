package memql

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/znasllc-io/memql/component/auth"
)

type accountAccessEntry struct {
	once  sync.Once
	state *accountAccessState
	err   error
}

func (e *MemQLEngine) accountAccessFor(ctx context.Context, account string) (*accountAccessState, error) {
	account = BareShortId(account)
	entry := &accountAccessEntry{}
	if memo, _ := ctx.Value(accountScopeMemoKey{}).(*accountScopeMemo); memo != nil {
		cached, _ := memo.access.LoadOrStore(account, entry)
		entry = cached.(*accountAccessEntry)
	}
	entry.once.Do(func() {
		if e == nil || e.database() == nil {
			entry.err = errors.New("account authority is unavailable")
			return
		}
		entry.state, entry.err = loadAccountAccess(ctx, e.database(), account)
	})
	return entry.state, entry.err
}

// accountHierarchyDecision narrows every app decision to the current account
// ceiling. Per-account roles can supply organization authority, never cluster
// authority. Explicit user/group denials remain denials.
func (e *MemQLEngine) accountHierarchyDecision(ctx context.Context, account string, subject auth.Subject, d auth.Decision) auth.Decision {
	if account == "" || BareShortId(account) == "self" || auth.IsClusterOperator(subject.Role) {
		return d
	}
	s, err := e.accountAccessFor(ctx, account)
	if err != nil {
		d.Held = false
		d.Barred = true
		return d
	}
	role := s.role(subject.UserId)

	denied := !d.Held && (d.Source == auth.SourceGroup || d.Source == auth.SourceUser)
	if strings.HasPrefix(d.Resource, "app:") {
		if _, err := normalizeAccessApps([]string{d.Resource}); err != nil {
			d.Held, d.Barred = false, true
			return d
		}
		management := auth.AccountRoleRank(role) >= auth.AccountRoleRank(auth.AccountAdmin) && (d.Resource == "app:users" || d.Resource == "app:accounts") && d.Verb == auth.VerbRead
		if management {
			d.Held = !denied
			return d
		}
		if !accessAppMatches(s.ceiling(), d.Resource) {
			d.Held = false
			return d
		}
		if apps, explicit := s.assignedApps(subject.UserId); explicit {
			d.Held = !denied && !d.Barred && accessAppMatches(apps, d.Resource) && (d.Verb == auth.VerbRead || role != auth.AccountViewer)
		}
		return d
	}
	if role == "" || denied || d.Barred {
		return d
	}
	switch d.Resource {
	case auth.ResourceData:
		d.Held = d.Verb == auth.VerbRead || role != auth.AccountViewer
	case "group":
		d.Held = auth.AccountRoleRank(role) >= auth.AccountRoleRank(auth.AccountAdmin)
	}
	return d
}

// Only implicit rank/account admission consults this set. An explicit sharing
// predicate remains independent. Resolve once per request, so the SQL predicate
// and subscription row gate agree and pagination cannot leak a short page.
func (e *MemQLEngine) developerPeerOwners(ctx context.Context) (map[string]struct{}, bool) {
	peers := map[string]struct{}{}
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil || ac.Synthetic || ac.UserId == "" {
		return peers, true
	}
	if ac.Unranked {
		role, found := e.organizationUserRole(ctx, ac.UserId)
		if !found {
			return peers, false
		}
		resolved := *ac
		resolved.Role, resolved.Unranked = role, false
		ac = &resolved
	}
	if memo, _ := ctx.Value(accountScopeMemoKey{}).(*accountScopeMemo); memo != nil {
		key := "peers:" + ac.UserId + ":" + string(ac.Role)
		entry := &developerPeersEntry{}
		cached, _ := memo.access.LoadOrStore(key, entry)
		entry = cached.(*developerPeersEntry)
		entry.once.Do(func() { entry.peers, entry.valid = e.resolveDeveloperPeers(ctx, ac) })
		return entry.peers, entry.valid
	}
	return e.resolveDeveloperPeers(ctx, ac)
}

type developerPeersEntry struct {
	once  sync.Once
	peers map[string]struct{}
	valid bool
}

func (e *MemQLEngine) resolveDeveloperPeers(ctx context.Context, ac *auth.AccessContext) (map[string]struct{}, bool) {
	peers := map[string]struct{}{}
	if ac.Role == auth.RoleDeveloper {
		for uid, role := range e.principalRoles(ctx) {
			if role == "developer" && !sameRowAuthzOwner(uid, ac.UserId) {
				addOwnerSpellings(peers, uid)
			}
		}
	}
	if ac.Role == auth.RoleOwner || e == nil || e.database() == nil {
		return peers, true
	}
	memberships, err := authorityRows(ctx, e.database(), conceptIdentityGroupMembership)
	if err != nil {
		return peers, false
	}
	accounts := map[string]bool{}
	for _, row := range memberships {
		p := accountRowPayload(row)
		group := BareShortId(stringFromAny(p["groupId"]))
		if p["status"] == "active" && p["organizationRole"] == "developer" && sameRowAuthzOwner(stringFromAny(p["userId"]), ac.UserId) && strings.HasPrefix(group, "acct-") {
			accounts[strings.TrimPrefix(group, "acct-")] = true
		}
	}
	for account := range accounts {
		s, err := e.accountAccessFor(ctx, account)
		if err != nil {
			return peers, false
		}
		if s.role(ac.UserId) != auth.AccountDeveloper {
			continue
		}
		for uid := range s.users {
			if s.role(uid) == auth.AccountDeveloper && !sameRowAuthzOwner(uid, ac.UserId) {
				addOwnerSpellings(peers, uid)
			}
		}
	}
	return peers, true
}

// A person can own one account while developing in another. Peer privacy
// withdraws implicit developer access, but must not withdraw that person's
// explicit responsibility for the account they own.
func (e *MemQLEngine) ownedAccountScope(ctx context.Context) *accountScope {
	entry := &accountScopeMemo{}
	if memo, _ := ctx.Value(accountScopeMemoKey{}).(*accountScopeMemo); memo != nil {
		cached, _ := memo.access.LoadOrStore("owned-accounts", entry)
		entry = cached.(*accountScopeMemo)
	}
	entry.once.Do(func() {
		entry.scope = &accountScope{accounts: map[string]struct{}{}}
		ac, _ := auth.AccessFromContext(ctx)
		if ac == nil || e == nil || e.database() == nil {
			return
		}
		rows, err := authorityRows(ctx, e.database(), conceptAccount)
		if err != nil {
			auth.NoteReadFailure(ctx, err)
			return
		}
		for _, row := range rows {
			p := accountRowPayload(row)
			if p["status"] == "active" && sameRowAuthzOwner(stringFromAny(p["ownerUserId"]), ac.UserId) {
				addAccountSpellings(entry.scope.accounts, row.ID)
			}
		}
	})
	return entry.scope
}

func (e *MemQLEngine) privateAccountPredicate(ctx context.Context, owner, accountField string, base ExpressionNode) ExpressionNode {
	if owner == "" {
		return base
	}
	peers, valid := e.developerPeerOwners(ctx)
	if !valid {
		return &constantBoolExpression{value: false}
	}
	if len(peers) == 0 {
		return base
	}
	values := []any{}
	for uid := range peers {
		values = append(values, uid)
	}
	sortAnyStrings(values)
	denied := &ComparisonExpression{Field: FieldReference{Raw: "payload." + owner, Parts: []string{"payload", owner}}, Operator: OpIn, Value: values}
	var visible ExpressionNode = &NotExpression{Target: denied}
	if accountField != "" {
		accounts := []string{}
		for account := range e.ownedAccountScope(ctx).accounts {
			accounts = append(accounts, account)
		}
		if len(accounts) > 0 {
			sort.Strings(accounts)
			visible = &LogicalExpression{Op: LogicalOr, Left: visible, Right: &accountScopeMatch{field: accountField, accounts: accounts}}
		}
	}
	return &LogicalExpression{Op: LogicalAnd, Left: base, Right: visible}
}

// Access edits affect membership, role and app decisions without changing the
// account-id set. Include the access revision and peer set in cached plans.
func (e *MemQLEngine) accountAccessFingerprint(ctx context.Context) string {
	ac, _ := auth.AccessFromContext(ctx)
	if ac == nil {
		return "anonymous"
	}
	if ac.Synthetic {
		return "system"
	}
	facts := map[string]struct{}{}
	peers, valid := e.developerPeerOwners(ctx)
	if !valid {
		return "unresolved"
	}
	for peer := range peers {
		facts["peer:"+peer] = struct{}{}
	}
	if len(peers) > 0 {
		for account := range e.ownedAccountScope(ctx).accounts {
			facts["owns:"+account] = struct{}{}
		}
	}
	for _, account := range e.membershipsFor(ctx, ac.UserId).accountIds {
		state, err := e.accountAccessFor(ctx, account)
		if err != nil {
			facts["unresolved:"+account] = struct{}{}
			continue
		}
		facts[account+":"+stringFromAny(accountRowPayload(state.account)["accessRevision"])] = struct{}{}
	}
	return fingerprintAccountSet(ac.UserId, facts)
}
