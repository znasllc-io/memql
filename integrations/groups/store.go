package groups

// store.go -- the graph seam (epic memql#5165, D12).
//
// Every read and write this package makes is a NAMED construct rendered as
// MemQL text and handed to the engine, the shape integrations/customdomain,
// component/campaigns and component/identity all use. Nothing here reaches the
// database.
//
// # The actor, and why it is synthetic
//
// v1:identity:group and v1:identity:groupMembership are UNOWNED rows (D2):
// ownerUserId is always empty, so no principal owns them and the
// `unowned="admin"` floor is what makes them readable at all. The writes are
// the DEPLOYMENT placing a row, not a person's, so they run under a synthetic
// cluster-owner identity the way customdomain's sweep does.
//
// The GUARDS are what bound this, and they are checked before anything here is
// called: guards.go asks whether the CALLER may do what they asked, against
// their own AccessContext, and the two account-lifecycle builtins are gated a
// step earlier still, by the `@requiresCapability` the engine's builtin
// executor enforces before their handlers run. Running the write under an
// operator identity after that check is the same division customdomain makes
// -- the caller's authority decides, the engine's identity writes.
//
// SO THE SYNTHETIC ACTOR STAYS, and swapping it for the caller's own is not
// the safer choice it looks like: these rows are UNOWNED, so a write under a
// caller's authority has no owned tier to pass through and fails the write
// guard's owner comparison against an empty stored owner -- surfacing as a
// WARN with the row silently not moving. What makes the synthetic actor safe
// is that every entry point above it has already decided the caller may be
// here; what would make it dangerous is an entry point that has not, which is
// the thing to check when adding one.
//
// # The internal-origin stamp is load-bearing
//
// Call origin defaults to CLIENT. Without auth.ContextWithInternalOrigin the
// writes here would be judged as a client's, and a client cannot write an
// unowned row: the write guard's owner comparison fails against an empty
// stored owner, and there is no owned tier for it to pass through. The failure
// would surface as a WARN with the row silently not moving.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// systemGroupsActor is the synthetic identity the writes run under.
const systemGroupsActor = "system:groups"

// Engine is the narrow engine surface this package needs.
type Engine interface {
	Execute(ctx context.Context, query string) (any, error)
}

// Store is the graph seam.
type Store struct {
	engine Engine
	now    func() time.Time
}

// NewStore builds the seam. `now` is injectable so tests assert exact stamps.
func NewStore(engine Engine) *Store {
	return &Store{engine: engine, now: func() time.Time { return time.Now().UTC() }}
}

// SystemActorContext stamps the three surfaces a write needs.
//
// ALL THREE, because they are read by different things: `createdBy` comes off
// the token info, `actor.userId` off the AccessContext, and claims back the
// token. Role must be RoleOwner because AccessContext.IsClusterOwner() reads
// Role and nothing else -- which is exactly why auth.ContextWithUserActor is
// not a substitute.
func SystemActorContext(ctx context.Context) context.Context {
	claims := map[string]any{"sub": systemGroupsActor, "role": "owner"}
	ctx = auth.ContextWithClaims(ctx, claims)
	ctx = auth.ContextWithToken(ctx, auth.BuildTokenInfo(claims))
	ctx = auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: systemGroupsActor,
		Role:   auth.RoleOwner,
	})
	return auth.ContextWithInternalOrigin(ctx)
}

// Group is one v1:identity:group row, as this package reads it.
type Group struct {
	ID          string
	Name        string
	Description string
	Kind        string
	AccountID   string
	Status      string
}

// Membership is one v1:identity:groupMembership row.
type Membership struct {
	ID      string
	GroupID string
	UserID  string
	Origin  string
	Status  string
}

// rows issues a read under the synthetic operator.
func (s *Store) rows(ctx context.Context, query string) ([]map[string]any, error) {
	if s == nil || s.engine == nil {
		return nil, fmt.Errorf("groups: no engine wired")
	}
	res, err := s.engine.Execute(SystemActorContext(ctx), query)
	if err != nil {
		return nil, fmt.Errorf("groups: %s: %w", firstWord(query), err)
	}
	return memql.MaterializeRows(res), nil
}

// exec issues a write under the synthetic operator.
func (s *Store) exec(ctx context.Context, query string) error {
	if s == nil || s.engine == nil {
		return fmt.Errorf("groups: no engine wired")
	}
	if _, err := s.engine.Execute(SystemActorContext(ctx), query); err != nil {
		return fmt.Errorf("groups: %s: %w", firstWord(query), err)
	}
	return nil
}

// GroupByID reads one group.
func (s *Store) GroupByID(ctx context.Context, groupID string) (*Group, error) {
	rows, err := s.rows(ctx, fmt.Sprintf("query groupById(groupId: %s)", langparser.QuoteString(groupID)))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	g := groupFromRow(rows[0])
	return &g, nil
}

// GroupsForAccount reads an account's active groups.
func (s *Store) GroupsForAccount(ctx context.Context, accountID string) ([]Group, error) {
	rows, err := s.rows(ctx, fmt.Sprintf("query groupsForAccount(accountId: %s)", langparser.QuoteString(accountID)))
	if err != nil {
		return nil, err
	}
	out := make([]Group, 0, len(rows))
	for _, r := range rows {
		out = append(out, groupFromRow(r))
	}
	return out, nil
}

// MembersOfGroup reads a group's active memberships.
func (s *Store) MembersOfGroup(ctx context.Context, groupID string) ([]Membership, error) {
	rows, err := s.rows(ctx, fmt.Sprintf("query membersOfGroup(groupId: %s)", langparser.QuoteString(groupID)))
	if err != nil {
		return nil, err
	}
	out := make([]Membership, 0, len(rows))
	for _, r := range rows {
		out = append(out, membershipFromRow(r))
	}
	return out, nil
}

// AccountStatus reads one account's status, empty when the row is not there.
//
// Reads through the SAME synthetic operator the writes use rather than the
// caller's actor. The question is "does this account exist and is it live",
// which decides whether a group may be tied to it -- a fact about the cluster,
// not about what this caller may see, and answering it from the caller's own
// visibility would let two admins get different answers about one account.
func (s *Store) AccountStatus(ctx context.Context, accountID string) (string, error) {
	rows, err := s.rows(ctx, fmt.Sprintf("query clientAccountById(accountId: %s)", langparser.QuoteString(accountID)))
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rowString(rows[0], "status"), nil
}

// AccountByID reads the fields the domain join and the ensure path need.
func (s *Store) AccountByID(ctx context.Context, accountID string) (map[string]any, error) {
	rows, err := s.rows(ctx, fmt.Sprintf("query clientAccountById(accountId: %s)", langparser.QuoteString(accountID)))
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

// UserRole reads one user's role slug, empty when the row is not there.
func (s *Store) UserRole(ctx context.Context, userID string) (string, error) {
	rows, err := s.rows(ctx, fmt.Sprintf("query userByIdSystem(userId: %s)", langparser.QuoteString(userID)))
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rowString(rows[0], "role"), nil
}

// WriteGroup upserts one group row.
func (s *Store) WriteGroup(ctx context.Context, g Group) error {
	var q strings.Builder
	q.WriteString("mutation writeGroup(groupId: ")
	q.WriteString(langparser.QuoteString(g.ID))
	q.WriteString(", name: ")
	q.WriteString(langparser.QuoteString(g.Name))
	q.WriteString(", description: ")
	q.WriteString(langparser.QuoteString(g.Description))
	q.WriteString(", kind: ")
	q.WriteString(langparser.QuoteString(g.Kind))
	q.WriteString(", accountId: ")
	q.WriteString(langparser.QuoteString(g.AccountID))
	q.WriteString(", status: ")
	q.WriteString(langparser.QuoteString(g.Status))
	if g.Status == StatusArchived {
		q.WriteString(", archivedAt: ")
		q.WriteString(langparser.QuoteString(stamp(s.now())))
	}
	q.WriteString(")")
	return s.exec(ctx, q.String())
}

// WriteMembership upserts one membership row.
func (s *Store) WriteMembership(ctx context.Context, m Membership, addedBy, removedBy string) error {
	var q strings.Builder
	q.WriteString("mutation writeGroupMembership(membershipId: ")
	q.WriteString(langparser.QuoteString(m.ID))
	q.WriteString(", groupId: ")
	q.WriteString(langparser.QuoteString(m.GroupID))
	q.WriteString(", userId: ")
	q.WriteString(langparser.QuoteString(m.UserID))
	q.WriteString(", origin: ")
	q.WriteString(langparser.QuoteString(m.Origin))
	q.WriteString(", addedBy: ")
	q.WriteString(langparser.QuoteString(addedBy))
	q.WriteString(", status: ")
	q.WriteString(langparser.QuoteString(m.Status))
	if m.Status == StatusRemoved {
		q.WriteString(", removedAt: ")
		q.WriteString(langparser.QuoteString(stamp(s.now())))
		q.WriteString(", removedBy: ")
		q.WriteString(langparser.QuoteString(removedBy))
	}
	q.WriteString(")")
	return s.exec(ctx, q.String())
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func groupFromRow(r map[string]any) Group {
	return Group{
		// BARE, like every other named-query reader in this tree: the engine
		// bare-ifies ids on egress and resolves bare arguments on the way
		// back in, so a canonical id here would be a spelling nothing else
		// in the package uses.
		ID:          memql.BareShortId(rowString(r, "id")),
		Name:        rowString(r, "name"),
		Description: rowString(r, "description"),
		Kind:        rowString(r, "kind"),
		AccountID:   memql.BareShortId(rowString(r, "accountId")),
		Status:      rowString(r, "status"),
	}
}

func membershipFromRow(r map[string]any) Membership {
	return Membership{
		ID:      memql.BareShortId(rowString(r, "id")),
		GroupID: memql.BareShortId(rowString(r, "groupId")),
		UserID:  memql.BareShortId(rowString(r, "userId")),
		Origin:  rowString(r, "origin"),
		Status:  rowString(r, "status"),
	}
}

func rowString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

func firstWord(q string) string {
	if i := strings.IndexAny(q, " ("); i > 0 {
		return q[:i]
	}
	return q
}

// AccountByVerifiedJoinDomain answers which active account has PROVEN this
// domain and switched joining on -- empty when none has.
//
// The three conditions are in the QUERY rather than here, so a row that fails
// one of them never reaches this package at all. That is deliberate: a
// condition checked in Go after a broader read is a condition somebody can
// drop while the read keeps working.
func (s *Store) AccountByVerifiedJoinDomain(ctx context.Context, domain string) (string, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return "", nil
	}
	rows, err := s.rows(ctx, fmt.Sprintf(
		"query accountForDomainJoin(domain: %s)", langparser.QuoteString(domain)))
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	// MORE THAN ONE is a cluster where two accounts proved the same domain,
	// which the walk cannot produce (a domain is verified per account, and
	// two accounts CAN name one domain). Taking the first is a stable answer
	// only because the query sorts; the alternative -- joining them to both
	// -- would give one person reach into two clients on an ambiguity nobody
	// declared.
	return memql.BareShortId(rowString(rows[0], "id")), nil
}
