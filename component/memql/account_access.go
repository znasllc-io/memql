package memql

// Account access is a transaction boundary, not an orchestration workflow.
// Every decision and append uses the same connection and lock as ordinary
// identity writes. A stale screen cannot overwrite a newer access decision.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/provenance"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

const conceptAccount = "v1:accounts:account"

type accountAccessState struct {
	account                memorynodes.MemoryNode
	users, groups, members map[string]memorynodes.MemoryNode
	grants                 []memorynodes.MemoryNode
	invitations            map[string]memorynodes.MemoryNode
}

// Only the reduced, authorized roster is returned by accountAccessBuiltin.
func loadAccountAccess(ctx context.Context, db bun.IDB, accountID string) (*accountAccessState, error) {
	account, found, err := authorityRow(ctx, db, conceptAccount, accountID)
	if err != nil {
		return nil, err
	}
	if !found || stringFromAny(accountRowPayload(account)["status"]) != "active" {
		return nil, errors.New("account_unavailable: this account is not active")
	}
	s := &accountAccessState{account: account, users: map[string]memorynodes.MemoryNode{}, groups: map[string]memorynodes.MemoryNode{}, members: map[string]memorynodes.MemoryNode{}, invitations: map[string]memorynodes.MemoryNode{}}
	for _, entry := range []struct {
		concept string
		into    map[string]memorynodes.MemoryNode
	}{
		{conceptIdentityUser, s.users}, {conceptIdentityGroup, s.groups}, {conceptIdentityGroupMembership, s.members}, {"v1:identity:invitation", s.invitations},
	} {
		rows, err := authorityRows(ctx, db, entry.concept)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			entry.into[BareShortId(row.ID)] = row
		}
	}
	s.grants, err = authorityRows(ctx, db, "v1:rbac:grant")
	return s, err
}

func (s *accountAccessState) owner() string {
	owner := BareShortId(stringFromAny(accountRowPayload(s.account)["ownerUserId"]))
	if owner != "" || BareShortId(s.account.ID) != "self" {
		return owner
	}
	// Pre-hierarchy self rows had no owner attribution. Resolve only one
	// unambiguous active owner; never guess from an email/contact/name.
	for uid, user := range s.users {
		p := accountRowPayload(user)
		if activeAuthorityUser(p) && stringFromAny(p["role"]) == "owner" {
			if owner != "" {
				return ""
			}
			owner = uid
		}
	}
	return owner
}

func (s *accountAccessState) member(userID string) (memorynodes.MemoryNode, bool) {
	userID = BareShortId(userID)
	group := accountRowPayload(s.groups["acct-"+BareShortId(s.account.ID)])
	if group["status"] != "active" || group["kind"] != "account" || BareShortId(stringFromAny(group["accountId"])) != BareShortId(s.account.ID) {
		return memorynodes.MemoryNode{}, false
	}
	for _, row := range s.members {
		p := accountRowPayload(row)
		if BareShortId(stringFromAny(p["groupId"])) == "acct-"+BareShortId(s.account.ID) && BareShortId(stringFromAny(p["userId"])) == userID && p["status"] == "active" {
			return row, true
		}
	}
	return memorynodes.MemoryNode{}, false
}

func accountRoleForClusterRole(role string) auth.AccountRole {
	switch role {
	case "owner":
		return auth.AccountOwner
	case "developer":
		return auth.AccountDeveloper
	case "admin":
		return auth.AccountAdmin
	case "user", "writer", "member":
		return auth.AccountMember
	default:
		return auth.AccountViewer
	}
}

func (s *accountAccessState) role(userID string) auth.AccountRole {
	userID = BareShortId(userID)
	user, exists := s.users[userID]
	if !exists || !activeAuthorityUser(accountRowPayload(user)) {
		return ""
	}
	if userID == s.owner() {
		return auth.AccountOwner
	}
	if BareShortId(s.account.ID) == "self" {
		// Primary roles keep their existing cluster meaning. Client roles
		// never change identity:user.role or acquire cluster capabilities.
		if role := stringFromAny(accountRowPayload(user)["role"]); auth.IsClusterOperator(auth.Role(role)) {
			return accountRoleForClusterRole(role)
		}
	}
	if row, ok := s.member(userID); ok {
		role := auth.AccountRole(stringFromAny(accountRowPayload(row)["organizationRole"]))
		if auth.ValidAccountRole(role) && role != auth.AccountOwner {
			return role
		}
		if BareShortId(s.account.ID) == "self" {
			return accountRoleForClusterRole(stringFromAny(accountRowPayload(user)["role"]))
		}
		return auth.AccountViewer
	}
	return ""
}

func (s *accountAccessState) clusterOwner(userID string) bool {
	u, ok := s.users[BareShortId(userID)]
	p := accountRowPayload(u)
	return ok && activeAuthorityUser(p) && p["role"] == "owner"
}

func (s *accountAccessState) manager(userID string) auth.AccountRole {
	if s.clusterOwner(userID) {
		return auth.AccountOwner
	}
	return s.role(userID)
}

func accessStrings(value any) []string {
	out := []string{}
	switch list := value.(type) {
	case []string:
		out = append(out, list...)
	case []any:
		for _, v := range list {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func accessAppMatches(apps []string, resource string) bool {
	for _, app := range apps {
		if resource == app || strings.HasPrefix(resource, app+"/") {
			return true
		}
	}
	return false
}

func normalizeAccessApps(value any) ([]string, error) {
	apps := accessStrings(value)
	if len(apps) > 100 {
		return nil, errors.New("account_apps_invalid: select at most 100 apps")
	}
	set := map[string]bool{}
	for _, app := range apps {
		if !strings.HasPrefix(app, "app:") || strings.ContainsAny(app, "* \t\r\n") || len(app) < 5 {
			return nil, errors.New("account_apps_invalid: name an app explicitly")
		}
		for _, barred := range []string{"app:cluster", "app:concepts", "app:logs", "app:setup", "app:fleet", "app:settings"} {
			if app == barred || strings.HasPrefix(app, barred+"/") {
				return nil, errors.New("account_apps_invalid: cluster administration cannot be delegated to a client")
			}
		}
		set[app] = true
	}
	apps = apps[:0]
	for app := range set {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	return apps, nil
}

func (s *accountAccessState) ceiling() []string {
	p := accountRowPayload(s.account)
	if apps, present := p["allowedApps"]; present {
		return accessStrings(apps)
	}
	// Existing accounts retain only their explicit canonical-group grants
	// until the primary owner configures a ceiling. Absence grants nothing.
	apps := []string{}
	for _, row := range s.grants {
		p := accountRowPayload(row)
		if p["subjectKind"] == "group" && BareShortId(stringFromAny(p["subjectId"])) == "acct-"+BareShortId(s.account.ID) && p["effect"] == "allow" && boolFromAny(p["active"]) {
			resource := stringFromAny(p["resourceType"])
			if strings.HasPrefix(resource, "app:") {
				apps = append(apps, resource)
			}
		}
	}
	return apps
}

func (s *accountAccessState) assignedApps(userID string) ([]string, bool) {
	if auth.AccountRoleRank(s.role(userID)) >= auth.AccountRoleRank(auth.AccountAdmin) {
		return s.ceiling(), true
	}
	member, found := s.member(userID)
	if !found {
		return nil, false
	}
	p := accountRowPayload(member)
	apps, explicit := p["allowedApps"]
	out := accessStrings(apps)
	for _, row := range s.members {
		m := accountRowPayload(row)
		if m["status"] != "active" || BareShortId(stringFromAny(m["userId"])) != BareShortId(userID) {
			continue
		}
		group := accountRowPayload(s.groups[BareShortId(stringFromAny(m["groupId"]))])
		if group["status"] == "active" && BareShortId(stringFromAny(group["accountId"])) == BareShortId(s.account.ID) {
			if list, present := group["allowedApps"]; present {
				out = append(out, accessStrings(list)...)
				explicit = true
			}
		}
	}
	return out, explicit
}

// The builtin returns a scoped roster, never raw identity/security records.
func (e *MemQLEngine) accountAccessBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return e.accountAccessOperation(ctx, args, false)
}
func (e *MemQLEngine) accountAccessUpdateBuiltin(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return e.accountAccessOperation(ctx, args, true)
}

func (e *MemQLEngine) accountAccessOperation(ctx context.Context, args map[string]any, write bool) ([]memorynodes.MemoryNode, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.Synthetic || ac.Unranked || ac.UserId == "" {
		return nil, errors.New("account_access_refused: sign in as an account manager")
	}
	if e == nil || e.database() == nil {
		return nil, errors.New("account_access_unavailable")
	}
	accountID := BareShortId(strings.TrimSpace(stringFromAny(args["accountId"])))
	if accountID == "" {
		return nil, errors.New("account_required")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var answer map[string]any
	var written []memorynodes.Node
	err := e.database().RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, accountAuthorityLock); err != nil {
			return err
		}
		s, err := loadAccountAccess(ctx, tx, accountID)
		if err != nil {
			return err
		}
		actor := BareShortId(ac.UserId)
		if !activeAuthorityUser(accountRowPayload(s.users[actor])) {
			return errors.New("account_access_refused: this person is not active")
		}
		role := s.manager(actor)
		leaving := write && stringFromAny(args["operation"]) == "leave"
		if !leaving && auth.AccountRoleRank(role) < auth.AccountRoleRank(auth.AccountAdmin) {
			return errors.New("account_access_refused: this account requires an owner, developer or admin")
		}
		if write {
			if _, present := args["revision"]; !leaving && (!present || stringFromAny(args["revision"]) != stringFromAny(accountRowPayload(s.account)["accessRevision"])) {
				return errors.New("account_access_changed: refresh before saving this change")
			}
			written, err = e.changeAccountAccess(ctx, tx, s, actor, role, args)
			if err != nil {
				return err
			}
		}
		answer = s.roster(actor)
		if leaving {
			answer = map[string]any{"accountId": accountID, "left": true}
		}
		for _, node := range written {
			if node.Concept == conceptIdentityGroup {
				answer["groupId"] = BareShortId(node.ID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, node := range written {
		e.InvalidateCacheForConcept(node.Concept)
		common.NotifyWrite(ctx, node.Concept, node.ID)
		if !e.withholdGraphWriteEvent(node.Concept) {
			var payload map[string]any
			_ = json.Unmarshal(node.Payload, &payload)
			envelope := map[string]any{"id": node.ID, "nodeId": node.ID, "concept": node.Concept, "actor": ac.UserId, "nodeType": node.Type, "payload": payload, "createdAt": eventVersionTime(node.CreatedAt), "firstVersion": false}
			for key, value := range payload {
				if _, reserved := envelope[key]; !reserved {
					envelope[key] = value
				}
			}
			e.publishGraphWriteEvent(ctx, events.BuildTopicWithConcept(events.TopicGraphNodeCreated, node.Concept), events.KindNodeCreated, envelope, ac.UserId)
		}
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "account-access:" + accountID, Concept: "account:access", Type: memorynodes.NodeTypeObject, CreatedAt: time.Now().UTC(), Payload: encoded}}, nil
}

func (s *accountAccessState) roster(actor string) map[string]any {
	invitations := []map[string]any{}
	for iid, row := range s.invitations {
		if s.invitationBelongs(row) {
			p := accountRowPayload(row)
			invitations = append(invitations, map[string]any{"id": iid, "email": p["inviteeEmail"], "expiresAt": p["expiresAt"], "groupIds": p["groupIds"]})
		}
	}
	sort.Slice(invitations, func(i, j int) bool { return fmt.Sprint(invitations[i]["email"]) < fmt.Sprint(invitations[j]["email"]) })
	groups := []map[string]any{}
	for gid, row := range s.groups {
		p := accountRowPayload(row)
		if p["status"] == "active" && BareShortId(stringFromAny(p["accountId"])) == BareShortId(s.account.ID) {
			ids := []string{}
			for _, m := range s.members {
				mp := accountRowPayload(m)
				if mp["status"] == "active" && BareShortId(stringFromAny(mp["groupId"])) == gid {
					ids = append(ids, BareShortId(stringFromAny(mp["userId"])))
				}
			}
			groups = append(groups, map[string]any{"id": gid, "name": p["name"], "kind": p["kind"], "apps": accessStrings(p["allowedApps"]), "userIds": ids})
		}
	}
	people := []map[string]any{}
	candidates := []map[string]any{}
	for uid, row := range s.users {
		p := accountRowPayload(row)
		if !activeAuthorityUser(p) {
			continue
		}
		role := s.role(uid)
		person := map[string]any{"id": uid, "name": p["displayName"], "email": p["primaryEmail"], "role": role}
		if role != "" {
			apps, _ := s.assignedApps(uid)
			person["apps"] = apps
			people = append(people, person)
		} else if s.clusterOwner(actor) {
			candidates = append(candidates, person)
		}
	}
	sort.Slice(people, func(i, j int) bool {
		return fmt.Sprint(people[i]["name"], people[i]["id"]) < fmt.Sprint(people[j]["name"], people[j]["id"])
	})
	sort.Slice(candidates, func(i, j int) bool {
		return fmt.Sprint(candidates[i]["name"], candidates[i]["id"]) < fmt.Sprint(candidates[j]["name"], candidates[j]["id"])
	})
	sort.Slice(groups, func(i, j int) bool {
		return fmt.Sprint(groups[i]["name"], groups[i]["id"]) < fmt.Sprint(groups[j]["name"], groups[j]["id"])
	})
	return map[string]any{"accountId": BareShortId(s.account.ID), "ownerUserId": s.owner(), "primary": BareShortId(s.account.ID) == "self", "actorRole": s.manager(actor), "clusterOwner": s.clusterOwner(actor), "revision": stringFromAny(accountRowPayload(s.account)["accessRevision"]), "apps": s.ceiling(), "people": people, "candidates": candidates, "groups": groups, "invitations": invitations}
}

// Only active Viewer invitations wholly confined to this account can be
// delegated. The roster never returns token hashes or unrelated invitations.
func (s *accountAccessState) invitationBelongs(row memorynodes.MemoryNode) bool {
	p := accountRowPayload(row)
	if !boolFromAny(p["active"]) || p["kind"] != "user" || p["status"] != "pending" {
		return false
	}
	if role := stringFromAny(p["inviteeRole"]); role != "viewer" && role != "reader" {
		return false
	}
	if expiry, err := time.Parse(time.RFC3339Nano, stringFromAny(p["expiresAt"])); err != nil || !expiry.After(time.Now()) {
		return false
	}
	groups := accessStrings(p["groupIds"])
	if len(groups) == 0 {
		return false
	}
	for _, gid := range groups {
		g := accountRowPayload(s.groups[BareShortId(gid)])
		if BareShortId(stringFromAny(g["accountId"])) != BareShortId(s.account.ID) {
			return false
		}
	}
	return true
}

func (e *MemQLEngine) changeAccountAccess(ctx context.Context, tx bun.Tx, s *accountAccessState, actor string, role auth.AccountRole, args map[string]any) ([]memorynodes.Node, error) {
	op := stringFromAny(args["operation"])
	accountID := BareShortId(s.account.ID)
	change := accountAuthorityMutation{}
	if op == "transfer" && accountID == "self" {
		change.primaryOwner = BareShortId(stringFromAny(args["userId"]))
	}
	ctx = context.WithValue(ctx, accountAuthorityMutationKey{}, change)
	origin := provenance.FromContext(ctx)
	if origin.IsZero() {
		origin = provenance.Direct("accountAccessUpdate")
	}
	ctx = provenance.ContextWithProvenance(ctx, origin.WithVia("accountAccessUpdate"))
	written := []memorynodes.Node{}
	appendRow := func(concept, id string, payload map[string]any) error {
		// This is a closed transaction writer. It cannot re-enter executeWrite
		// on another connection without losing atomic ownership transfer and
		// batch rollback. Operations below compose these payloads after current
		// account authority checks; no caller can choose a concept or raw payload.
		switch concept {
		case conceptAccount, conceptIdentityUser, conceptIdentityGroup, conceptIdentityGroupMembership, "v1:identity:invitation":
		default:
			return errors.New("account_access_concept_refused")
		}
		meta, err := e.concepts.Get(concept)
		if err != nil {
			return err
		}
		prior, _, err := authorityRow(ctx, tx, concept, id)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if !now.After(prior.CreatedAt) {
			now = prior.CreatedAt.Add(time.Microsecond)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		var normalized map[string]any
		if err := json.Unmarshal(encoded, &normalized); err != nil {
			return err
		}
		params := memorynodes.CreateParams{Actor: actor, ID: BareShortId(id), Payload: normalized, Clock: func() time.Time { return now }}
		if e.metadataCollector != nil {
			params.Metadata = e.metadataCollector.Collect(ctx)
		}
		node, err := meta.Create(ctx, &bunStore{db: tx}, params)
		if err != nil {
			return err
		}
		written = append(written, node)
		row := memorynodes.MemoryNode{ID: node.ID, Concept: node.Concept, CreatedAt: node.CreatedAt, Payload: node.Payload}
		switch concept {
		case conceptAccount:
			s.account = row
		case conceptIdentityUser:
			s.users[BareShortId(id)] = row
		case conceptIdentityGroupMembership:
			s.members[BareShortId(id)] = row
		case conceptIdentityGroup:
			s.groups[BareShortId(id)] = row
		case "v1:identity:invitation":
			s.invitations[BareShortId(id)] = row
		}
		return nil
	}
	writeMember := func(groupID, userID string, next auth.AccountRole, remove bool, apps any) error {
		memberID := groupID + "-" + userID
		p := accountRowPayload(s.members[memberID])
		if p == nil {
			p = map[string]any{}
		}
		p["groupId"], p["userId"], p["accountId"] = conceptIdentityGroup+":"+groupID, conceptIdentityUser+":"+userID, conceptAccount+":"+accountID
		p["ownerUserId"], p["origin"], p["status"], p["addedBy"] = "", "added", "active", actor
		if remove {
			p["status"], p["removedAt"], p["removedBy"] = "removed", time.Now().UTC().Format(time.RFC3339Nano), actor
		} else {
			delete(p, "removedAt")
			delete(p, "removedBy")
		}
		if groupID == "acct-"+accountID && next != "" {
			p["organizationRole"] = string(next)
		}
		if apps != nil {
			p["allowedApps"] = apps
		}
		return appendRow(conceptIdentityGroupMembership, memberID, p)
	}
	account := accountRowPayload(s.account)
	// Repair legacy attribution on the first authorized access change.
	if s.owner() == "" {
		return nil, errors.New("account_owner_required: this account has no unambiguous owner")
	}
	account["ownerUserId"] = conceptIdentityUser + ":" + s.owner()
	switch op {
	case "revokeInvitation":
		iid := BareShortId(stringFromAny(args["invitationId"]))
		row, exists := s.invitations[iid]
		if !exists || !s.invitationBelongs(row) {
			return nil, errors.New("account_invitation_refused")
		}
		p := accountRowPayload(row)
		p["active"], p["status"], p["respondedAt"] = false, "dismissed", time.Now().UTC().Format(time.RFC3339Nano)
		if err := appendRow("v1:identity:invitation", iid, p); err != nil {
			return nil, err
		}
	case "leave":
		groupID := BareShortId(stringFromAny(args["groupId"]))
		g := accountRowPayload(s.groups[groupID])
		if g["status"] != "active" || BareShortId(stringFromAny(g["accountId"])) != accountID {
			return nil, errors.New("account_group_refused")
		}
		if groupID == "acct-"+accountID && s.owner() == actor {
			return nil, errors.New("account_owner_required: transfer ownership before leaving the account")
		}
		member := accountRowPayload(s.members[groupID+"-"+actor])
		if member["status"] != "active" || !sameRowAuthzOwner(stringFromAny(member["userId"]), actor) {
			return nil, errors.New("account_membership_required: this person does not belong to the group")
		}
		for _, row := range s.members {
			p := accountRowPayload(row)
			gid := BareShortId(stringFromAny(p["groupId"]))
			group := accountRowPayload(s.groups[gid])
			if p["status"] == "active" && sameRowAuthzOwner(stringFromAny(p["userId"]), actor) && (gid == groupID || (groupID == "acct-"+accountID && BareShortId(stringFromAny(group["accountId"])) == accountID)) {
				if err := writeMember(gid, actor, "", true, nil); err != nil {
					return nil, err
				}
			}
		}
	case "createGroup":
		name := strings.TrimSpace(stringFromAny(args["name"]))
		if name == "" {
			return nil, errors.New("account_group_name_required")
		}
		groupID := id.NewShortId()
		if err := appendRow(conceptIdentityGroup, groupID, map[string]any{"ownerUserId": "", "accountId": conceptAccount + ":" + accountID, "name": name, "description": stringFromAny(args["description"]), "kind": "custom", "status": "active"}); err != nil {
			return nil, err
		}
	case "updateGroup", "archiveGroup":
		groupID := BareShortId(stringFromAny(args["groupId"]))
		g := accountRowPayload(s.groups[groupID])
		if g["status"] != "active" || g["kind"] != "custom" || BareShortId(stringFromAny(g["accountId"])) != accountID {
			return nil, errors.New("account_group_refused")
		}
		if op == "archiveGroup" {
			g["status"] = "archived"
			for _, row := range s.members {
				p := accountRowPayload(row)
				if p["status"] == "active" && BareShortId(stringFromAny(p["groupId"])) == groupID {
					if err := writeMember(groupID, BareShortId(stringFromAny(p["userId"])), "", true, nil); err != nil {
						return nil, err
					}
				}
			}
		} else {
			if name, present := args["name"]; present && strings.TrimSpace(stringFromAny(name)) != "" {
				g["name"] = strings.TrimSpace(stringFromAny(name))
			}
			if desc, present := args["description"]; present {
				g["description"] = stringFromAny(desc)
			}
		}
		if err := appendRow(conceptIdentityGroup, groupID, g); err != nil {
			return nil, err
		}
	case "apps":
		if !s.clusterOwner(actor) || accountID == "self" {
			return nil, errors.New("account_apps_refused: only the primary owner assigns client apps")
		}
		apps, err := normalizeAccessApps(args["apps"])
		if err != nil {
			return nil, err
		}
		account["allowedApps"] = apps
	case "transfer":
		if actor != s.owner() && !s.clusterOwner(actor) {
			return nil, errors.New("account_transfer_refused: only an owner can transfer ownership")
		}
		target := BareShortId(stringFromAny(args["userId"]))
		if target == s.owner() || s.role(target) == "" {
			return nil, errors.New("account_transfer_refused: choose an active member of this account")
		}
		oldOwner := s.owner()
		if accountID == "self" {
			for uid, newRole := range map[string]string{oldOwner: "developer", target: "owner"} {
				p := accountRowPayload(s.users[uid])
				p["role"] = newRole
				if err := appendRow(conceptIdentityUser, uid, p); err != nil {
					return nil, err
				}
			}
		}
		if err := writeMember("acct-"+accountID, oldOwner, auth.AccountDeveloper, false, nil); err != nil {
			return nil, err
		}
		if err := writeMember("acct-"+accountID, target, auth.AccountDeveloper, false, nil); err != nil {
			return nil, err
		}
		account["ownerUserId"] = conceptIdentityUser + ":" + target
	case "members", "groupMembers":
		users := accessStrings(args["userIds"])
		if len(users) == 0 || len(users) > 100 {
			return nil, errors.New("account_members_invalid: select between 1 and 100 people")
		}
		next := auth.AccountRole(stringFromAny(args["role"]))
		if next == "" {
			next = auth.AccountViewer
		}
		remove := boolFromAny(args["remove"])
		if remove {
			next = ""
		} else if !auth.ValidAccountRole(next) || next == auth.AccountOwner {
			return nil, errors.New("account_role_invalid: choose Developer, Admin, Member or Viewer")
		}
		groupID := "acct-" + accountID
		if op == "groupMembers" {
			groupID = BareShortId(stringFromAny(args["groupId"]))
			g := accountRowPayload(s.groups[groupID])
			if g["status"] != "active" || BareShortId(stringFromAny(g["accountId"])) != accountID || g["kind"] != "custom" {
				return nil, errors.New("account_group_refused: choose a custom group in this account")
			}
		}
		var apps any
		if list, present := args["apps"]; present {
			selected, err := normalizeAccessApps(list)
			if err != nil {
				return nil, err
			}
			for _, app := range selected {
				if accountID != "self" && !accessAppMatches(s.ceiling(), app) {
					return nil, errors.New("account_apps_refused: an app exceeds this account's access")
				}
			}
			apps = selected
		}
		seen := map[string]bool{}
		for _, uid := range users {
			uid = BareShortId(uid)
			if seen[uid] {
				continue
			}
			seen[uid] = true
			u, exists := s.users[uid]
			if !exists || !activeAuthorityUser(accountRowPayload(u)) {
				return nil, errors.New("account_member_unavailable: select an active person")
			}
			current := s.role(uid)
			memberNext := next
			if op == "groupMembers" || (!remove && stringFromAny(args["role"]) == "" && current != "") {
				memberNext = current
			}
			if uid == actor || !auth.MayManageAccountMember(role, current, memberNext) {
				return nil, errors.New("account_member_refused: you can manage only roles below your own; transfer ownership separately")
			}
			if current == "" && !s.clusterOwner(actor) {
				return nil, errors.New("account_member_refused: add people through an account invitation first")
			}
			if op == "groupMembers" && current == "" {
				return nil, errors.New("account_member_refused: the person must belong to this account")
			}
			if accountID == "self" && op == "members" {
				p := accountRowPayload(u)
				global := string(memberNext)
				if memberNext == auth.AccountMember {
					global = "user"
				}
				if remove {
					global = "viewer"
				}
				p["role"] = global
				if err := appendRow(conceptIdentityUser, uid, p); err != nil {
					return nil, err
				}
			}
			if err := writeMember(groupID, uid, memberNext, remove, apps); err != nil {
				return nil, err
			}
			if remove && op == "members" {
				// Removing the canonical membership revokes custom groups too,
				// otherwise an old group would silently keep admitting the person.
				for _, m := range s.members {
					p := accountRowPayload(m)
					gid := BareShortId(stringFromAny(p["groupId"]))
					g := accountRowPayload(s.groups[gid])
					if gid != groupID && p["status"] == "active" && BareShortId(stringFromAny(p["userId"])) == uid && BareShortId(stringFromAny(g["accountId"])) == accountID {
						if err := writeMember(gid, uid, "", true, nil); err != nil {
							return nil, err
						}
					}
				}
			}
		}
	case "groupApps":
		groupID := BareShortId(stringFromAny(args["groupId"]))
		g := accountRowPayload(s.groups[groupID])
		if g["status"] != "active" || g["kind"] != "custom" || BareShortId(stringFromAny(g["accountId"])) != accountID {
			return nil, errors.New("account_group_refused")
		}
		apps, err := normalizeAccessApps(args["apps"])
		if err != nil {
			return nil, err
		}
		for _, app := range apps {
			if !accessAppMatches(s.ceiling(), app) {
				return nil, errors.New("account_apps_refused: an app exceeds this account's access")
			}
		}
		g["allowedApps"] = apps
		if err := appendRow(conceptIdentityGroup, groupID, g); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("account_operation_invalid")
	}
	account["accessRevision"] = id.NewShortId()
	if err := appendRow(conceptAccount, accountID, account); err != nil {
		return nil, err
	}
	return written, nil
}
