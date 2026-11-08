// Package roletest issues bounded sessions for real, separately owned test
// principals. It never borrows the operator's row ownership or login factors.
package roletest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/verifier"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memql "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/num"
)

const Lifetime = 30 * time.Minute
const TokenPrefix = "mql_test_"

// originRoleTest marks the one membership an organization role's persona holds
// (v1:identity:groupMembership.origin), so it is never read as a person having
// been placed in that organization.
const originRoleTest = "role-test"

var ErrDenied = errors.New("role testing is unavailable for this identity or role")
var ErrInvalid = errors.New("invalid or expired role-test session")

type Session struct {
	ID        string
	UserID    string
	Role      string
	ExpiresAt time.Time
	// Token is returned once by Start. It is never persisted or audited.
	Token string
}

type Service struct {
	Store *identity.Store
	Audit identity.AuditLogger
	Now   func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
func (s *Service) db() (*sql.DB, error) {
	if s.Store == nil || s.Store.Engine == nil || s.Store.DirectDB == nil || s.Store.DirectDB() == nil || s.Audit == nil {
		return nil, errors.New("role testing is not configured")
	}
	return s.Store.DirectDB(), nil
}
func fresh(ctx context.Context) context.Context {
	return memql.ContextWithFreshRead(auth.ContextForRoleTestValidation(ctx))
}
func (s *Service) actor(ctx context.Context) (*identity.UserRow, error) {
	ac, ok := auth.AccessFromContext(ctx)
	claims, _ := auth.ClaimsFromContext(ctx)
	class, _ := claims["class"].(string)
	if claims["pat_id"] != nil {
		return nil, ErrDenied
	}
	if _, delegated := auth.DelegationFromContext(ctx); delegated {
		return nil, ErrDenied
	}
	if !ok || ac == nil || claims["sub"] != ac.UserId || ac.Synthetic || ac.Unranked || auth.IsRoleTestUser(ac.UserId) || (class != "" && class != verifier.ClassUser) {
		return nil, ErrDenied
	}
	u, err := s.Store.LookupUserById(fresh(ctx), ac.UserId)
	if err != nil || u == nil || !u.Active || (u.Role != string(auth.RoleOwner) && u.Role != string(auth.RoleDeveloper)) {
		return nil, ErrDenied
	}
	epoch := int64(0)
	if value, exists := claims["revocation_epoch"]; exists {
		epoch, ok = integerClaim(value)
		if !ok {
			return nil, ErrDenied
		}
	}
	expires, ok := integerClaim(claims["exp"])
	if !ok || epoch != u.RevocationEpoch || !s.now().Before(time.Unix(expires, 0)) {
		return nil, ErrDenied
	}
	return u, nil
}

// JWT MapClaims uses float64 for JSON numbers, which fmt prints in exponent
// notation for ordinary Unix timestamps. Read the numeric value directly and
// refuse malformed, fractional or out-of-range authority claims.
func integerClaim(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return num.WholeInt64(v)
	case int64:
		return v, true
	case int:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

// mayTest is whether this operator may test this role: one role asked of the
// gate the list answer asks about every role.
func (s *Service) mayTest(ctx context.Context, operator *identity.UserRow, role string) bool {
	if !auth.MayTestRole(auth.Role(operator.Role), auth.Role(role)) {
		return false
	}
	return s.testingGate(ctx, operator).allows(ctx, role)
}

// testingGate is what deciding an operator's tests needs to know about the
// operator, read once however many roles are asked: whether a restriction on
// them, or a read about them that failed, refuses every test, and what they
// hold. Developer sessions cannot restore capabilities denied to the operator,
// so the grant source is read explicitly and a failed read refuses instead of
// falling back to a role default.
type testingGate struct {
	operator *identity.UserRow
	owner    bool
	refused  bool
	holds    map[auth.VerbResource]bool
}

func (s *Service) testingGate(ctx context.Context, operator *identity.UserRow) *testingGate {
	g := &testingGate{operator: operator, owner: operator.Role == string(auth.RoleOwner)}
	if g.owner {
		return g
	}
	// Refused until every read about the operator has answered.
	g.refused = true
	src := auth.InstalledGrantSource()
	if src == nil {
		return g
	}
	grants, err := src.ActiveGrantsForUser(fresh(ctx), operator.ID)
	if err != nil {
		return g
	}
	// A restriction on a developer is intentionally a reason to use their
	// real account, not to mint an unrestricted copy of their base role.
	for _, grant := range grants {
		if !grant.Inactive && grant.Effect == auth.GrantDeny {
			return g
		}
	}
	if ms := auth.InstalledMembershipSource(); ms != nil {
		checked, ok := ms.(auth.CheckedMembershipSource)
		if !ok {
			return g
		}
		groups, err := checked.ActiveGroupIdsForUserChecked(fresh(ctx), operator.ID)
		if err != nil {
			return g
		}
		groupGrants, err := src.ActiveGrantsForGroups(fresh(ctx), groups)
		if err != nil {
			return g
		}
		for _, grant := range groupGrants {
			if !grant.Inactive && grant.Effect == auth.GrantDeny {
				return g
			}
		}
	}
	// WHAT THE OPERATOR HOLDS, resolved once by the resolver every gate uses.
	// A pair outside this set is one no role and no grant of theirs gives
	// them, which is exactly the pairs CapableFor answers false for.
	readCtx, failures := auth.ContextWithReadFailures(fresh(ctx))
	subject, ok := auth.SubjectFromContext(auth.ContextWithAccess(readCtx, &auth.AccessContext{UserId: operator.ID, Role: auth.Role(operator.Role)}))
	if !ok {
		return g
	}
	g.holds = map[auth.VerbResource]bool{}
	for _, d := range auth.EffectiveCapabilities(readCtx, subject) {
		if d.Held {
			g.holds[auth.VerbResource{Verb: d.Verb, Resource: d.Resource}] = true
		}
	}
	g.refused = failures.Err() != nil
	return g
}

// allows decides one role. A persona holds no grant of its own (verification
// refuses one); an organization role's persona holds its organization group's
// grants, so the operator must hold what the role AND that group give. A read
// that fails while deciding the role leaves it refused.
func (g *testingGate) allows(ctx context.Context, role string) bool {
	if !auth.MayTestRole(auth.Role(g.operator.Role), auth.Role(role)) {
		return false
	}
	if g.owner {
		return true
	}
	if g.refused {
		return false
	}
	// EVERYTHING THE PERSONA WILL HOLD: its role's capabilities and, for an
	// organization role, what that organization's group grants it -- the
	// membership Start places.
	readCtx, failures := auth.ContextWithReadFailures(fresh(ctx))
	persona := auth.Subject{Role: auth.Role(role)}
	if groupID, _ := organizationGroupID(role); groupID != "" {
		persona.GroupIds = []string{groupID}
	}
	for _, d := range auth.EffectiveCapabilities(readCtx, persona) {
		// Viewer/Member can read their own groups; Developer has no such
		// catalog grant. A persona's only possible group is the organization
		// group its test placed -- verification refuses any other -- and an
		// operator at developer rank already reads that group through the
		// groups' unowned="admin" tier, so this reaches nothing they cannot.
		if d.Verb == auth.VerbRead && d.Resource == auth.ResourceGroup {
			continue
		}
		if d.Held && !g.holds[auth.VerbResource{Verb: d.Verb, Resource: d.Resource}] {
			return false
		}
	}
	return failures.Err() == nil
}

// testableRoles is every role this operator may test now, decided by the
// gates Start applies -- the gate above, and an organization role's
// organization having a group to join -- in the order a client draws them:
// the predefined roles, then the custom ones strongest first. A role whose
// decision a failed read left open is refused, never offered. (A persona
// somebody tampered with -- deactivated, or re-roled -- still refuses at Start;
// that is about one identity, not about whether the role may be tested.)
func (s *Service) testableRoles(ctx context.Context, operator *identity.UserRow) []Role {
	gate := s.testingGate(ctx, operator)
	out := []Role{}
	for _, slug := range candidateRoles() {
		if !gate.allows(ctx, slug) {
			continue
		}
		r := Role{Slug: slug, Name: roleName(slug)}
		if groupID, _ := organizationGroupID(slug); groupID != "" {
			group, err := s.organizationGroup(ctx, slug)
			if err != nil || group == nil {
				continue
			}
			r.AccountID, r.Organization = group.accountID, group.name
		}
		out = append(out, r)
	}
	return out
}

// candidateRoles is every role a test could name: the predefined four, then
// the catalog's other slugs strongest first. Only a list of names: retired,
// base and out-of-rank slugs among them are refused by the gate like any other.
func candidateRoles() []string {
	out := []string{string(auth.RoleReader), string(auth.RoleWriter), string(auth.RoleDeveloper), string(auth.RoleAdmin)}
	cat := auth.InstalledCapabilityCatalog()
	lister, ok := cat.(interface{ Slugs() []string })
	if cat == nil || !ok {
		return out
	}
	seen := map[string]bool{}
	for _, slug := range out {
		seen[slug] = true
	}
	var custom []string
	for _, slug := range lister.Slugs() {
		if !seen[slug] {
			seen[slug] = true
			custom = append(custom, slug)
		}
	}
	sort.SliceStable(custom, func(i, j int) bool {
		ri, _ := cat.Rank(custom[i])
		rj, _ := cat.Rank(custom[j])
		if ri != rj {
			return ri > rj
		}
		return custom[i] < custom[j]
	})
	return append(out, custom...)
}

// organizationGroupID is the group an organization role's persona joins -- the
// account group ensureAccountGroup writes for every organization, at the id it
// derives, acct-<accountShortId> (dsl/identity/concepts.memql, group.kind) --
// and that organization. Both empty for a global role. The scope is the
// installed role's configuration, never anything a caller supplies.
func organizationGroupID(role string) (groupID, accountID string) {
	accountID = memql.BareShortId(auth.RoleAccountScope(auth.Role(role)))
	if accountID == "" {
		return "", ""
	}
	return "acct-" + accountID, accountID
}

func (s *Service) auditRoleTestSession(ctx context.Context, action, operator, target, role, accountID string, err error) {
	if s.Audit == nil {
		return
	}
	outcome := identity.AuditOutcomeSuccess
	reason := ""
	if err != nil {
		outcome = identity.AuditOutcomeFailure
		reason = "refused_or_failed"
	}
	s.Audit.Log(fresh(ctx), identity.AuditEvent{OccurredAt: s.now(), Category: identity.AuditCategoryAdmin, Action: action,
		ActorUserId: operator, TargetType: "session", TargetId: target,
		Detail: map[string]any{"role": role, "accountId": accountID}, Outcome: outcome, FailureReason: reason})
}

func (s *Service) Start(ctx context.Context, role string) (out Session, err error) {
	var actorID string
	// The organization a test of this role runs in is the installed role's
	// alone, so it is known before anything can refuse, and every Start audits
	// it, a refused one too.
	_, accountID := organizationGroupID(role)
	defer func() { s.auditRoleTestSession(ctx, "role_test_started", actorID, out.ID, role, accountID, err) }()
	db, err := s.db()
	if err != nil {
		return out, err
	}
	operator, err := s.actor(ctx)
	if err != nil {
		return out, err
	}
	actorID = operator.ID
	if !s.mayTest(ctx, operator, role) {
		return out, ErrDenied
	}
	group, err := s.organizationGroup(ctx, role)
	if err != nil {
		return out, err
	}
	// The test runs in the organization whose group its persona joins: that is
	// what the session records for End, and what this Start audits.
	accountID = ""
	if group != nil {
		accountID = group.accountID
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	// Serialize provisioning across BFF replicas without holding an engine
	// transaction open. A failed commit can leave an inert, unmapped user;
	// its reserved id cannot authenticate without a committed test session.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, personaLock(actorID, role)); err != nil {
		return out, err
	}
	userID := "v1:identity:user:" + auth.RoleTestUserPrefix + uuid.NewString()
	if _, err = tx.ExecContext(ctx, `INSERT INTO identity_role_test_personas(operator_id,role,user_id) VALUES($1,$2,$3) ON CONFLICT(operator_id,role) DO NOTHING`, actorID, role, userID); err != nil {
		return out, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT user_id FROM identity_role_test_personas WHERE operator_id=$1 AND role=$2`, actorID, role).Scan(&userID); err != nil {
		return out, err
	}
	u, err := s.Store.LookupUserById(fresh(ctx), userID)
	if err != nil {
		return out, err
	}
	// The session this Start issues is named before anything is written for
	// it, so a warning on the way can say which one.
	sessionID := uuid.NewString()
	name := personaName(role, group)
	if u == nil {
		// The preceding actor/mayTest gates authorize exactly this bounded
		// persona. Its reserved ID is server-generated, the actor is unchanged,
		// and this inline stamp opens only the server-only user-creation write.
		// Never return the stamped context or accept a caller-selected user ID.
		err = s.Store.CreateUserOnFirstLogin(auth.ContextWithInternalOrigin(ctx), userID, name,
			strings.TrimPrefix(userID, "v1:identity:user:")+"@role-test.invalid", role, false, identity.UserProfileSeed{})
		if err != nil {
			return out, err
		}
	} else if !u.Active || u.Role != role {
		return out, ErrDenied
	} else if u.DisplayName != name {
		// The role or its organization's group was renamed, or the role moved
		// to another organization, since the persona was last named.
		s.rename(ctx, userID, role, sessionID, name)
	}
	var entropy [32]byte
	if _, err = rand.Read(entropy[:]); err != nil {
		return out, err
	}
	out = Session{ID: sessionID, UserID: userID, Role: role, ExpiresAt: s.now().Add(Lifetime), Token: TokenPrefix + hex.EncodeToString(entropy[:])}
	// One active run per operator/role. Restarting a test also ends any old
	// editor tabs for that persona instead of accumulating live credentials.
	if _, err = tx.ExecContext(ctx, `UPDATE identity_role_test_sessions SET revoked_at=$2 WHERE user_id=$1 AND revoked_at IS NULL`, userID, s.now()); err != nil {
		return Session{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO identity_role_test_sessions(id,token_hash,operator_id,operator_epoch,user_id,role,created_at,expires_at,account_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, out.ID, hash(out.Token), actorID, operator.RevocationEpoch, userID, role, s.now(), out.ExpiresAt, accountID); err != nil {
		return Session{}, err
	}
	// The persona's membership, under its lock and before the session commits,
	// so a token is handed out only once the persona stands where its role
	// does. A commit that fails after this leaves a membership with no live
	// test, which this operator's next Start or End removes.
	if err = s.placeMemberships(ctx, actorID, userID, group); err != nil {
		return Session{}, err
	}
	if err = tx.Commit(); err != nil {
		return Session{}, err
	}
	s.sweep(ctx, db, actorID, role)
	return out, nil
}

// personaLock names the advisory lock that serializes one persona's
// provisioning, sessions and membership across BFF replicas.
func personaLock(operatorID, role string) string { return "role-test:" + operatorID + ":" + role }

// roleName is a role's display name: the catalog's, or the predefined role's
// own before the catalog is readable.
func roleName(role string) string {
	name := map[string]string{"reader": "Viewer", "writer": "Member", "developer": "Developer", "admin": "Admin"}[role]
	if cat := auth.InstalledCapabilityCatalog(); cat != nil {
		if catalogName := strings.TrimSpace(cat.Name(role)); catalogName != "" {
			name = catalogName
		}
	}
	if name == "" {
		name = role
	}
	return name
}

// personaName is the persona's display name: the role's name and, for an
// organization role, its organization's, which the organization's group
// carries as its own name.
func personaName(role string, group *organizationGroup) string {
	name := roleName(role)
	if group != nil && group.name != "" {
		name += " (" + group.name + ")"
	}
	return "Test " + name
}

// rename gives an existing persona the name its test now calls for, before
// Start issues the session. It writes that one field of the persona's own row:
// updateUser merges a payload of displayName alone over the stored row, so the
// role, the address, active and everything else stay as they were. The same
// gates as the persona's creation precede it, on the same server-reserved ID
// under the unchanged actor, and this inline stamp opens only that @serverOnly
// write; the stamped context is never returned. Best effort: the name is what
// the persona is called, not what it may do, so a failure is logged -- with the
// persona, its role and the session the Start is issuing -- and the test starts
// all the same.
func (s *Service) rename(ctx context.Context, userID, role, sessionID, name string) {
	_, err := s.Store.Engine.Execute(auth.ContextWithInternalOrigin(ctx), fmt.Sprintf(`mutation updateUser(userId: %s, payload: {displayName: %s})`,
		langparser.QuoteString(userID), langparser.QuoteString(name)))
	if err != nil {
		s.warn("role test: renaming a persona failed", err, "persona", userID, "role", role, "session", sessionID)
	}
}

// ---------------------------------------------------------------------
// The organization membership
// ---------------------------------------------------------------------
//
// An organization role's persona is a member of exactly that organization's
// group, so it sees what that organization's people see. The membership is
// marked origin role-test and placed by Start before the session commits. End
// removes it once no test of the persona is live, and every Start and End also
// removes it from this operator's other personas whose tests are no longer live
// (R14): a test that expired without an End, or whose End failed to remove it,
// keeps its membership until then. Reads -- List, verification -- write
// nothing, so it stays through them; it admits nobody, because nothing can
// authenticate as a persona without a live session.

type organizationGroup struct{ id, accountID, name string }

type membership struct{ id, groupID, accountID, userID, origin string }

// deployment is the identity membership rows are read and written under. A
// membership is the deployment's row, not a person's -- unowned, written only
// by @serverOnly mutations -- and the gates before every call here decided
// which one this persona holds, so the deployment places it, as
// integrations/groups places every other one. Built on fresh(), this
// service's own trusted context, because an End from the test's own tab
// removes the membership after its session is already revoked.
func deployment(ctx context.Context) context.Context {
	return auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(fresh(ctx), "role-test"))
}

func (s *Service) rows(ctx context.Context, query string) ([]map[string]any, error) {
	res, err := s.Store.Engine.Execute(deployment(ctx), query)
	if err != nil || res == nil {
		return nil, err
	}
	return memql.MaterializeRows(res), nil
}

func text(row map[string]any, key string) string {
	v, _ := row[key].(string)
	return strings.TrimSpace(v)
}

// organizationGroup reads the group an organization role's persona joins: nil
// for a global role, and a refusal when the organization has no active group
// to join -- an archived organization, or one whose group was never made.
func (s *Service) organizationGroup(ctx context.Context, role string) (*organizationGroup, error) {
	groupID, accountID := organizationGroupID(role)
	if groupID == "" {
		return nil, nil
	}
	rows, err := s.rows(ctx, fmt.Sprintf(`query groupById(groupId: %s)`, langparser.QuoteString(groupID)))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if text(row, "status") == "active" && text(row, "kind") == "account" && memql.BareShortId(text(row, "accountId")) == accountID {
			return &organizationGroup{id: groupID, accountID: accountID, name: text(row, "name")}, nil
		}
	}
	return nil, fmt.Errorf("%w: its organization has no active group to test in", ErrDenied)
}

// membershipsOf reads a persona's active memberships, whatever their origin.
func (s *Service) membershipsOf(ctx context.Context, userID string) ([]membership, error) {
	userID = memql.BareShortId(userID)
	rows, err := s.rows(ctx, fmt.Sprintf(`query groupsForUser(userId: %s)`, langparser.QuoteString(userID)))
	if err != nil {
		return nil, err
	}
	out := make([]membership, 0, len(rows))
	for _, row := range rows {
		if text(row, "status") != "active" {
			continue
		}
		m := membership{id: memql.BareShortId(text(row, "id")), groupID: memql.BareShortId(text(row, "groupId")),
			accountID: memql.BareShortId(text(row, "accountId")), userID: userID, origin: text(row, "origin")}
		if m.id == "" {
			m.id = m.groupID + "-" + userID
		}
		out = append(out, m)
	}
	return out, nil
}

// writeMembership writes one version of a persona's membership: placed by its
// operator's test, or removed once no test of it is live.
func (s *Service) writeMembership(ctx context.Context, m membership, active bool, operatorID string) error {
	q := fmt.Sprintf(`mutation writeGroupMembership(membershipId: %s, groupId: %s, accountId: %s, userId: %s, origin: %s`,
		langparser.QuoteString(m.id), langparser.QuoteString(m.groupID), langparser.QuoteString(m.accountID),
		langparser.QuoteString(m.userID), langparser.QuoteString(m.origin))
	operator := langparser.QuoteString(memql.BareShortId(operatorID))
	if active {
		q += `, status: "active", addedBy: ` + operator + `)`
	} else {
		q += `, status: "removed", removedAt: ` + langparser.QuoteString(s.now().Format(time.RFC3339)) + `, removedBy: ` + operator + `)`
	}
	_, err := s.Store.Engine.Execute(deployment(ctx), q)
	return err
}

// placeMemberships makes a persona's role-test memberships exactly its
// organization group's, or none when group is nil. It writes role-test rows
// only: a membership that arrived any other way is not this service's to
// remove, and verification refuses the persona while it holds one.
func (s *Service) placeMemberships(ctx context.Context, operatorID, userID string, group *organizationGroup) error {
	current, err := s.membershipsOf(ctx, userID)
	if err != nil {
		return err
	}
	placed := false
	for _, m := range current {
		if group != nil && m.groupID == group.id {
			placed = true
			continue
		}
		if m.origin != originRoleTest {
			continue
		}
		if err := s.writeMembership(ctx, m, false, operatorID); err != nil {
			return err
		}
	}
	if group == nil || placed {
		return nil
	}
	userID = memql.BareShortId(userID)
	return s.writeMembership(ctx, membership{id: group.id + "-" + userID, groupID: group.id, accountID: group.accountID,
		userID: userID, origin: originRoleTest}, true, operatorID)
}

// release removes a persona's role-test memberships once none of its tests is
// live. It takes the persona's lock, which Start holds while it places a
// membership and commits a session, so it never removes what a Start of the
// same persona is placing. With wait false a persona whose lock is held is
// skipped: whoever holds it is settling that persona.
func (s *Service) release(ctx context.Context, db *sql.DB, operatorID, role, userID string, wait bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	locked := true
	if wait {
		_, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, personaLock(operatorID, role))
	} else {
		err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`, personaLock(operatorID, role)).Scan(&locked)
	}
	if err != nil || !locked {
		return err
	}
	var live bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM identity_role_test_sessions WHERE user_id=$1 AND revoked_at IS NULL AND expires_at>$2)`, userID, s.now()).Scan(&live); err != nil || live {
		return err
	}
	if err = s.placeMemberships(ctx, operatorID, userID, nil); err != nil {
		return err
	}
	return tx.Commit()
}

// sweep releases this operator's other personas whose tests are no longer
// live, so a test that expired without End, or whose End failed to remove its
// membership, keeps it only until the operator's next Start or End. Best
// effort: a failure is the next call's to retry and never fails the call it
// rides on. The predefined roles are never organization roles, so their
// personas hold no membership to release.
func (s *Service) sweep(ctx context.Context, db *sql.DB, operatorID, exceptRole string) {
	rows, err := db.QueryContext(ctx, `SELECT p.role, p.user_id FROM identity_role_test_personas p
		WHERE p.operator_id=$1 AND p.role<>$2 AND p.role NOT IN ('reader','writer','developer','admin')
		AND NOT EXISTS (SELECT 1 FROM identity_role_test_sessions t WHERE t.user_id=p.user_id AND t.revoked_at IS NULL AND t.expires_at>$3)`,
		operatorID, exceptRole, s.now())
	if err != nil {
		s.warn("role test: listing idle personas failed", err)
		return
	}
	var idle [][2]string
	var scanErr error
	for rows.Next() {
		var role, userID string
		if scanErr = rows.Scan(&role, &userID); scanErr != nil {
			break
		}
		idle = append(idle, [2]string{role, userID})
	}
	if err := errors.Join(scanErr, rows.Err(), rows.Close()); err != nil {
		s.warn("role test: listing idle personas failed", err)
		return
	}
	for _, p := range idle {
		if err := s.release(ctx, db, operatorID, p[0], p[1], false); err != nil {
			s.warn("role test: removing an idle persona's membership failed", err)
		}
	}
}

// warn logs a failure the call it rode on survives, with what it was about:
// attrs are slog's alternating keys and values.
func (s *Service) warn(msg string, err error, attrs ...any) {
	if s.Store != nil && s.Store.Logger != nil {
		s.Store.Logger.Warn(msg, append([]any{"error", err}, attrs...)...)
	}
}

// holdsOnlyItsTestMembership: a persona holds exactly what its test placed --
// no membership for a global role, and for an organization role the one
// role-test membership of that organization's group. A second group, or the
// right group joined some other way, is access its operator never tested.
// `groups` is the authorization read's answer; the origin is the row's.
func (s *Service) holdsOnlyItsTestMembership(ctx context.Context, userID, role string, groups []string) bool {
	groupID, _ := organizationGroupID(role)
	if groupID == "" {
		return len(groups) == 0
	}
	if len(groups) != 1 || memql.BareShortId(groups[0]) != groupID {
		return false
	}
	current, err := s.membershipsOf(ctx, userID)
	return err == nil && len(current) == 1 && current[0].groupID == groupID && current[0].origin == originRoleTest
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Service) VerifyRoleTest(ctx context.Context, token string) (*verifier.VerifiedClaims, error) {
	if len(token) != len(TokenPrefix)+64 || !strings.HasPrefix(token, TokenPrefix) {
		return nil, ErrInvalid
	}
	db, err := s.db()
	if err != nil {
		return nil, err
	}
	var row Session
	var operatorID string
	var epoch int64
	err = db.QueryRowContext(ctx, `SELECT id,operator_id,operator_epoch,user_id,role,expires_at FROM identity_role_test_sessions WHERE token_hash=$1 AND revoked_at IS NULL AND expires_at>$2`, hash(token), s.now()).Scan(&row.ID, &operatorID, &epoch, &row.UserID, &row.Role, &row.ExpiresAt)
	if err != nil {
		return nil, ErrInvalid
	}
	u, err := s.validateRow(ctx, row, operatorID, epoch)
	if err != nil {
		return nil, err
	}
	cm := map[string]any{"sub": u.ID, "email": u.PrimaryEmail, "name": u.DisplayName, "role": row.Role, "class": auth.RoleTestClass, "exp": row.ExpiresAt.Unix(), "role_test_session": row.ID, "role_test_operator": operatorID}
	return &verifier.VerifiedClaims{UserId: u.ID, Email: u.PrimaryEmail, Name: u.DisplayName, Role: row.Role, Class: auth.RoleTestClass, ExpiresAt: row.ExpiresAt, Source: verifier.SourceRoleTest, ClaimsMap: cm}, nil
}

func (s *Service) End(ctx context.Context, sessionID string) (err error) {
	var actorID, role, accountID string
	defer func() { s.auditRoleTestSession(ctx, "role_test_ended", actorID, sessionID, role, accountID, err) }()
	db, err := s.db()
	if err != nil {
		return err
	}
	claims, _ := auth.ClaimsFromContext(ctx)
	if claims["class"] == auth.RoleTestClass {
		own, _ := claims["role_test_session"].(string)
		actorID, _ = claims["role_test_operator"].(string)
		if own == "" || sessionID != own {
			return ErrDenied
		}
	} else {
		u, e := s.actor(ctx)
		if e != nil {
			return e
		}
		actorID = u.ID
	}
	// The session ends first, on a statement of its own, so nothing that
	// follows can leave a test running.
	var userID string
	var ranIn sql.NullString
	err = db.QueryRowContext(ctx, `UPDATE identity_role_test_sessions SET revoked_at=COALESCE(revoked_at,$3) WHERE id=$1 AND operator_id=$2 RETURNING user_id, role, account_id`, sessionID, actorID, s.now()).Scan(&userID, &role, &ranIn)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	// The organization the test ran in, as its Start recorded it -- not where
	// the role points now, which may have moved since. A session from before
	// Start recorded one (NULL) has only the role's current organization.
	accountID = ranIn.String
	if !ranIn.Valid {
		_, accountID = organizationGroupID(role)
	}
	// Then its membership, once no test of the persona is live: ending a run
	// that a newer Start already replaced leaves the live run's in place. The
	// test has ended whatever happens here, so a membership that fails to go
	// is logged rather than returned: it admits nobody without a live session,
	// and this operator's next Start or End removes it -- the sweep, or this
	// role's own next End. Its own name, so End's err, which the deferred audit
	// reads, is never shadowed here.
	if releaseErr := s.release(ctx, db, actorID, role, userID, true); releaseErr != nil {
		s.warn("role test: removing an ended test's membership failed", releaseErr, "persona", userID, "role", role, "session", sessionID)
	}
	s.sweep(ctx, db, actorID, role)
	return nil
}

// Role is one role an operator may test now, with what a client draws it by.
type Role struct {
	Slug         string // what a start names
	Name         string // the role's display name
	AccountID    string // an organization role's organization; empty for a global role
	Organization string // that organization's name
}

// List is the operator's role-test answer: their live tests, and the roles
// they may start now (testableRoles). Reads only.
func (s *Service) List(ctx context.Context) ([]Session, []Role, error) {
	db, err := s.db()
	if err != nil {
		return nil, nil, err
	}
	u, err := s.actor(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT id,user_id,role,expires_at FROM identity_role_test_sessions WHERE operator_id=$1 AND revoked_at IS NULL AND expires_at>$2 ORDER BY created_at DESC`, u.ID, s.now())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var result []Session
	for rows.Next() {
		var r Session
		if err := rows.Scan(&r.ID, &r.UserID, &r.Role, &r.ExpiresAt); err != nil {
			return nil, nil, err
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return result, s.testableRoles(ctx, u), nil
}

func (r Session) String() string { return fmt.Sprintf("role test %s (%s)", r.ID, r.Role) }

func (s *Service) validateRow(ctx context.Context, row Session, operatorID string, epoch int64) (*identity.UserRow, error) {
	operator, err := s.Store.LookupUserById(fresh(ctx), operatorID)
	if err != nil || operator == nil || !operator.Active || operator.RevocationEpoch != epoch || !s.mayTest(ctx, operator, row.Role) {
		return nil, ErrInvalid
	}
	u, err := s.Store.LookupUserById(fresh(ctx), row.UserID)
	if err != nil || u == nil || !u.Active || u.Role != row.Role || !auth.IsRoleTestUser(u.ID) {
		return nil, ErrInvalid
	}
	if src := auth.InstalledGrantSource(); src != nil {
		grants, err := src.ActiveGrantsForUser(fresh(ctx), u.ID)
		if err != nil {
			return nil, ErrInvalid
		}
		for _, g := range grants {
			if !g.Inactive {
				return nil, ErrInvalid
			}
		}
	}
	if ms := auth.InstalledMembershipSource(); ms != nil {
		checked, ok := ms.(auth.CheckedMembershipSource)
		if !ok {
			return nil, ErrInvalid
		}
		groups, err := checked.ActiveGroupIdsForUserChecked(fresh(ctx), u.ID)
		if err != nil || !s.holdsOnlyItsTestMembership(ctx, u.ID, row.Role, groups) {
			return nil, ErrInvalid
		}
	}
	return u, nil
}

func (s *Service) ValidateRoleTest(ctx context.Context, sessionID, subject string, role auth.Role) error {
	db, err := s.db()
	if err != nil {
		return err
	}
	var row Session
	var operatorID string
	var epoch int64
	err = db.QueryRowContext(ctx, `SELECT id,operator_id,operator_epoch,user_id,role,expires_at FROM identity_role_test_sessions WHERE id=$1 AND revoked_at IS NULL AND expires_at>$2`, sessionID, s.now()).Scan(&row.ID, &operatorID, &epoch, &row.UserID, &row.Role, &row.ExpiresAt)
	if err != nil || row.UserID != subject || row.Role != string(role) {
		return ErrInvalid
	}
	_, err = s.validateRow(ctx, row, operatorID, epoch)
	return err
}
