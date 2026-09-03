package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/id"
)

// store_githubconnect.go -- the two rows GitHub Connect writes, and the
// exactly-once discipline the second of them needs (epic memql#4912, issue
// memql#4913).
//
// # Why the writes live in THIS package rather than beside the handler
//
// Every construct below is @serverOnly, so each write has to stamp internal
// origin, and `component/identity` is on the internal-origin allowlist in the
// repo-root call_origin_conformance_test.go while `component/identity/http`
// and `component/identity/githubconnect` are not. The stamps are written
// INLINE at each Execute rather than routed through a helper, because
// TestEveryGoCallerOfAServerOnlyConstructStampsInternalOrigin reads FILES: a
// stamp it cannot see in the same file as the construct name is a stamp the
// next reader cannot see either.
//
// # The two rows
//
// v1:identity:githubConnectState is this domain's -- a short-lived,
// digest-keyed, single-use credential, the enrolment-token shape.
// v1:platform:sourceCredential is the PLATFORM's, and the callback is the only
// thing on an identity node that writes one; it does so under the STATE ROW's
// user rather than under an actor of its own, because a browser redirect from
// GitHub carries no MemQL bearer at all.

// -----------------------------------------------------------------------------
// The gate
// -----------------------------------------------------------------------------

// githubConnectGateLockClass namespaces the connect-state advisory locks.
//
// Postgres two-key advisory locks (pg_advisory_lock(classid, objid)) occupy a
// DIFFERENT lock space from the single-key bigint form, so this cannot collide
// with the cron-leader lease, the topology reconciler, the schema lock or the
// recovery-key mint (all single-key). Within the two-key space the class byte
// keeps clear of the magic link (0x4D4C4E4B "MLNK"), cognition's dispatch
// (0x434F474E "COGN"), greeting (0x47524554 "GRET") and feedback-announce
// (0x464E4452 "FNDR") gates, and the planner's admission lock (0x504C414E
// "PLAN"). 0x47484342 spells "GHCB". A seventh gate picks the next free
// four-character constant and repeats this list.
const githubConnectGateLockClass int32 = 0x47484342

// githubConnectGateTimeout bounds the wait for the lock. A holder that wedges
// must not hang a browser parked on GitHub's redirect: past this the caller
// proceeds unlocked, which reduces the flow to a race rather than to a
// failure. The critical section is two engine round-trips, so a wait this long
// already means something is wrong elsewhere. Same value the magic-link gate
// uses, for the same reason.
const githubConnectGateTimeout = 5 * time.Second

// githubConnectGateKey derives the advisory objid for one state digest.
// FNV-32a, matching the magic-link and cognition gates. A hash collision costs
// two unrelated callbacks serialising against each other for the length of one
// critical section -- invisible, and never a correctness problem, because the
// re-read inside the section is keyed on the digest itself.
func githubConnectGateKey(stateHash string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(stateHash))
	return int32(h.Sum32())
}

// withGithubConnectGate runs fn while holding the advisory lock for stateHash.
//
// A gate-infrastructure failure (no getter, DB not ready, connection error,
// lock query error, timeout) never fails the call: fn runs anyway, unlocked.
// That is the pre-gate read-then-write -- no worse than not having the gate,
// and better than refusing a legitimate connect because a lock query errored.
// It is not the shipped configuration: app/integrations_identity.go wires
// DirectDB from the same directDBGetter the magic-link gate uses, and a
// db-gated test asserts the property against a real Postgres.
//
// The lock has to be taken on the DIRECT connection: a transaction-mode
// PgBouncer recycles the server backend between statements and would silently
// drop a session-scoped lock (epic memql#1925). Silently is the operative
// word -- the code reads as correct and the mutual exclusion is simply absent.
func (s *Store) withGithubConnectGate(ctx context.Context, stateHash string, fn func(context.Context) error) error {
	if s == nil || s.DirectDB == nil {
		return fn(ctx)
	}
	db := s.DirectDB()
	if db == nil {
		return fn(ctx)
	}

	lockCtx, cancel := context.WithTimeout(ctx, githubConnectGateTimeout)
	defer cancel()

	conn, err := db.Conn(lockCtx)
	if err != nil {
		s.warn("identity.store: github-connect gate connection unavailable; proceeding unlocked", "error", err.Error())
		return fn(ctx)
	}
	defer func() { _ = conn.Close() }()

	objid := githubConnectGateKey(stateHash)
	// BLOCKING, not pg_try_advisory_lock. The loser of this race must not
	// bail -- it must WAIT, re-read, and discover that it lost, so it can
	// redirect with connect_state_invalid instead of silently doing nothing
	// or, worse, landing a second grant.
	if _, err := conn.ExecContext(lockCtx, "SELECT pg_advisory_lock($1, $2)", githubConnectGateLockClass, objid); err != nil {
		s.warn("identity.store: github-connect gate not acquired; proceeding unlocked", "error", err.Error())
		return fn(ctx)
	}
	defer func() {
		// Unlock on a background context so a client disconnect mid-flight
		// cannot leave the lock held for the life of the pooled connection.
		uctx, ucancel := context.WithTimeout(context.Background(), githubConnectGateTimeout)
		defer ucancel()
		if _, err := conn.ExecContext(uctx, "SELECT pg_advisory_unlock($1, $2)", githubConnectGateLockClass, objid); err != nil {
			s.warn("identity.store: github-connect gate unlock failed", "error", err.Error())
		}
	}()

	return fn(ctx)
}

// -----------------------------------------------------------------------------
// The connect-state row
// -----------------------------------------------------------------------------

// Sentinel outcomes of a consume. Callers branch on these rather than on an
// error string: all three are ordinary results of a browser doing something
// ordinary (clicking twice, coming back tomorrow, following a stale link), not
// failures of this cluster.
var (
	// ErrGithubConnectStateNotFound means no row carries that digest -- an
	// unknown or forged state.
	ErrGithubConnectStateNotFound = errors.New("identity.store: github connect state not found")
	// ErrGithubConnectStateAlreadyConsumed means another caller won. Exactly
	// one consumer of a given row gets nil; every other one gets this.
	ErrGithubConnectStateAlreadyConsumed = errors.New("identity.store: github connect state already consumed")
	// ErrGithubConnectStateExpired means the row was never consumed and its
	// TTL has passed. Distinct from already-consumed so the audit trail can
	// tell a slow person from a replayed link.
	ErrGithubConnectStateExpired = errors.New("identity.store: github connect state expired")
)

// GithubConnectStateRow is the projection of v1:identity:githubConnectState
// the callback needs.
type GithubConnectStateRow struct {
	ID             string
	UserId         string
	StateHash      string
	ReturnPath     string
	ExpiresAt      time.Time
	ConsumedAt     time.Time // zero = not consumed
	ConsumedFromIP string
	SourceIP       string
}

// HashConnectState digests the plaintext state value the way every writer and
// reader of githubConnectState.stateHash does.
//
// SHA-256 hex, the convention shared with every other credential row in this
// domain (pat, workerToken, workerPairingCode, invitation, magicLinkRequest,
// enrolmentToken). Stated once, here, because a divergence between the writer
// and the reader would not fail loudly -- it would make every callback report
// an unknown state, which reads as "GitHub Connect is broken".
func HashConnectState(plain string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plain)))
	return hex.EncodeToString(sum[:])
}

// GithubConnectStateSeed is one begin.
type GithubConnectStateSeed struct {
	UserId     string
	StateHash  string
	ReturnPath string
	SourceIP   string
	ExpiresAt  time.Time
}

// CreateGithubConnectState writes the row a later callback will consume.
//
// The plaintext state never reaches here: the caller hashes it and keeps the
// plaintext only long enough to put it in the authorize URL it answers with.
func (s *Store) CreateGithubConnectState(ctx context.Context, seed GithubConnectStateSeed) (string, error) {
	if s == nil || s.Engine == nil {
		return "", errNoEngine
	}
	userId := strings.TrimSpace(seed.UserId)
	if userId == "" {
		// REFUSED rather than written blank. This field names the only
		// account the callback may land a grant on; a row naming nobody is a
		// connect that either lands a grant owned by nobody -- readable by
		// nobody, including the person who pressed the button -- or lands one
		// on whichever account the callback can be talked into.
		return "", fmt.Errorf("identity.store: a GitHub connect state names the person who began it, and this call carries no user")
	}
	if strings.TrimSpace(seed.StateHash) == "" {
		return "", fmt.Errorf("identity.store: a GitHub connect state needs a state digest")
	}
	if seed.ExpiresAt.IsZero() {
		return "", fmt.Errorf("identity.store: a GitHub connect state needs an expiry")
	}

	stateId := "v1:identity:githubConnectState:" + id.NewShortId()
	query := fmt.Sprintf(
		`mutation createGithubConnectState(stateId: %s, userId: %s, stateHash: %s, expiresAt: %s, returnPath: %s, sourceIP: %s)`,
		langparser.QuoteString(stateId),
		langparser.QuoteString(userId),
		langparser.QuoteString(seed.StateHash),
		langparser.QuoteString(seed.ExpiresAt.UTC().Format(time.RFC3339)),
		langparser.QuoteString(SafeRelativeRedirect(seed.ReturnPath)),
		langparser.QuoteString(seed.SourceIP),
	)
	// INTERNAL ORIGIN: createGithubConnectState is @serverOnly, and the engine
	// refuses such a construct unless the context carries it -- so without
	// this the call cannot succeed on any cluster, ever. Stamped inline as the
	// argument to the one Execute so the marked context dies at this call.
	if _, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), query); err != nil {
		return "", fmt.Errorf("identity.store: create github connect state: %w", err)
	}
	return stateId, nil
}

// LookupGithubConnectState resolves the row carrying one state digest, or nil.
//
// Returns the row in EVERY state -- expired and already-consumed included --
// because that is what lets the consume tell those apart in the audit trail
// instead of reporting both as "unknown state".
func (s *Store) LookupGithubConnectState(ctx context.Context, stateHash string) (*GithubConnectStateRow, error) {
	if s == nil || s.Engine == nil {
		return nil, errNoEngine
	}
	query := fmt.Sprintf(`query githubConnectStateByHash(stateHash: %s)`, langparser.QuoteString(stateHash))
	// INTERNAL ORIGIN: githubConnectStateByHash is @serverOnly. The caller is
	// the identity service resolving a state value GitHub handed back, before
	// any actor exists -- exactly the pre-actor, server-initiated work the
	// annotation admits.
	nodes, err := s.executeAndExtract(auth.ContextWithInternalOrigin(ctx), query)
	if err != nil {
		return nil, fmt.Errorf("identity.store: lookup github connect state: %w", err)
	}
	return firstGithubConnectStateRow(nodes), nil
}

// ConsumeGithubConnectState spends a connect state EXACTLY ONCE and returns
// the row it spent.
//
// The compare and the swap both happen inside the advisory-lock critical
// section: re-read the row, refuse if it is gone, spent or expired, otherwise
// write. The winner's write is committed (the engine autocommits) before the
// lock is released, so the next holder re-reads and sees consumedAt set. N
// concurrent callers therefore produce exactly one nil and N-1 sentinels.
//
// This is not a theoretical interleaving. A browser redirect is replayable by
// construction -- the URL sits in history, in a referrer, and in whatever the
// person pasted it into -- and the two identity replicas that would both serve
// it share nothing but the database.
func (s *Store) ConsumeGithubConnectState(ctx context.Context, stateHash, consumedFromIP string) (*GithubConnectStateRow, error) {
	var row *GithubConnectStateRow
	err := s.withGithubConnectGate(ctx, stateHash, func(ctx context.Context) error {
		found, err := s.LookupGithubConnectState(ctx, stateHash)
		if err != nil {
			return fmt.Errorf("identity.store: consume github connect state: re-read: %w", err)
		}
		if found == nil {
			return ErrGithubConnectStateNotFound
		}
		if !found.ConsumedAt.IsZero() {
			return ErrGithubConnectStateAlreadyConsumed
		}
		if !found.ExpiresAt.IsZero() && time.Now().UTC().After(found.ExpiresAt) {
			return ErrGithubConnectStateExpired
		}
		query := fmt.Sprintf(
			`mutation consumeGithubConnectState(stateId: %s, consumedAt: %s, consumedFromIP: %s)`,
			langparser.QuoteString(found.ID),
			langparser.QuoteString(time.Now().UTC().Format(time.RFC3339Nano)),
			langparser.QuoteString(consumedFromIP),
		)
		// INTERNAL ORIGIN: consumeGithubConnectState is @serverOnly. Stamped
		// inline, in this file, for the reason the header gives.
		if _, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), query); err != nil {
			return fmt.Errorf("identity.store: consume github connect state: %w", err)
		}
		row = found
		return nil
	})
	if err != nil {
		return nil, err
	}
	return row, nil
}

func firstGithubConnectStateRow(nodes []*memqlv1.MemoryNode) *GithubConnectStateRow {
	if len(nodes) == 0 || nodes[0] == nil {
		return nil
	}
	g := newFieldGetter(nodes[0])
	return &GithubConnectStateRow{
		ID:             firstNonEmpty(g.str("id"), nodes[0].GetId()),
		UserId:         g.str("userId"),
		StateHash:      g.str("stateHash"),
		ReturnPath:     g.str("returnPath"),
		ExpiresAt:      g.time("expiresAt"),
		ConsumedAt:     g.time("consumedAt"),
		ConsumedFromIP: g.str("consumedFromIP"),
		SourceIP:       g.str("sourceIP"),
	}
}

// -----------------------------------------------------------------------------
// The grant row
// -----------------------------------------------------------------------------

// githubAppGrantConcept is the row id prefix a new grant is minted under. The
// row belongs to the PLATFORM domain, not this one: the identity node writes
// it because the callback lands here, and nothing else about it is identity's.
const githubAppGrantConcept = "v1:platform:sourceCredential"

// GithubAppGrant is one connect's worth of sealed authority.
//
// Neither token appears in this struct in the clear: the caller seals both
// with component/secret.Encrypt before building it, so a value that reaches a
// log line through this struct is ciphertext.
type GithubAppGrant struct {
	OwnerUserId string
	Host        string
	Label       string
	// EncryptedValue and RefreshToken are already sealed.
	EncryptedValue  string
	Fingerprint     string
	RefreshToken    string
	ExpiresAt       time.Time
	Login           string
	ExternalId      string
	InstallationIds []string
}

// UpsertGithubAppGrant lands the callback's grant: one row per (owner, GitHub
// account), created the first time and updated in place on every reconnect.
//
// Returns the credential row id and whether it was newly created, which is the
// only thing separating the `github_connected` audit action from
// `github_reconnected`.
//
// THE ACTOR IS BORROWED, and both halves matter. The mutations stamp
// `ownerUserId: actor.userId`, and this call has no actor of its own -- a
// browser redirect from GitHub carries no MemQL bearer -- so the state row's
// user is threaded in with auth.ContextWithUserActor and the write runs as
// that person. Internal origin is stamped on top because the constructs are
// @serverOnly. An EMPTY owner is refused BEFORE either context is built:
// ContextWithUserActor returns the context unchanged on a blank id, so the row
// would land owned by nobody -- readable by nobody, including the person who
// just connected, who would see a success and a grant that resolves for no
// package.
func (s *Store) UpsertGithubAppGrant(ctx context.Context, grant GithubAppGrant) (string, bool, error) {
	if s == nil || s.Engine == nil {
		return "", false, errNoEngine
	}
	owner := strings.TrimSpace(grant.OwnerUserId)
	if owner == "" {
		return "", false, fmt.Errorf("identity.store: a GitHub App grant belongs to the person who connected, and this call carries no user")
	}
	if strings.TrimSpace(grant.ExternalId) == "" {
		// The stable key. Without it a reconnect cannot find the existing row
		// and every connect mints another grant, which is exactly the
		// duplication externalId exists to prevent.
		return "", false, fmt.Errorf("identity.store: a GitHub App grant needs the GitHub account's numeric id")
	}

	// Under the owner's actor for BOTH the compare and the swap: the read is
	// @actor-bound and filters ownerUserId==actor.userId, so an unborrowed
	// read would match nothing and every reconnect would mint a second row.
	owned := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, owner))

	existing, err := s.lookupGithubAppGrant(owned, grant.ExternalId)
	if err != nil {
		return "", false, err
	}

	expiresAt := ""
	if !grant.ExpiresAt.IsZero() {
		expiresAt = grant.ExpiresAt.UTC().Format(time.RFC3339)
	}

	if existing != "" {
		args := []string{
			kv("credentialId", existing),
			kv("encryptedValue", grant.EncryptedValue),
			kv("fingerprint", grant.Fingerprint),
			kv("login", grant.Login),
			"installationIds: " + memqlStringList(grant.InstallationIds),
		}
		// OMITTED RATHER THAN BLANKED. update{} is a read-merge, so an
		// argument this writer has nothing to say about must not be sent: an
		// app with user-token expiry disabled returns no refresh token and no
		// expiry, and writing "" for either would erase values a previous
		// connect legitimately stored.
		args = appendOpt(args, "refreshToken", grant.RefreshToken)
		args = appendOpt(args, "expiresAt", expiresAt)
		query := "mutation updateGithubAppGrant(" + strings.Join(args, ", ") + ")"
		if _, err := s.Engine.Execute(owned, query); err != nil {
			return "", false, fmt.Errorf("identity.store: update github app grant: %w", err)
		}
		return existing, false, nil
	}

	credentialId := githubAppGrantConcept + ":" + id.NewShortId()
	args := []string{
		kv("credentialId", credentialId),
		kv("host", grant.Host),
		kv("label", grant.Label),
		kv("encryptedValue", grant.EncryptedValue),
		kv("fingerprint", grant.Fingerprint),
		kv("login", grant.Login),
		kv("externalId", grant.ExternalId),
		"installationIds: " + memqlStringList(grant.InstallationIds),
	}
	args = appendOpt(args, "refreshToken", grant.RefreshToken)
	args = appendOpt(args, "expiresAt", expiresAt)
	query := "mutation createGithubAppGrant(" + strings.Join(args, ", ") + ")"
	if _, err := s.Engine.Execute(owned, query); err != nil {
		return "", false, fmt.Errorf("identity.store: create github app grant: %w", err)
	}
	return credentialId, true, nil
}

// kv renders one `name: "value"` argument.
//
// langparser.QuoteString rather than a hand-written `"%s"`: it supplies its
// own delimiters and escapes the whole C0 range through encoding/json, where
// interpolating a value between two literal quote characters makes the safety
// depend on a sanitizer a reader has to go and find. Every value below is
// either ciphertext, a GitHub-supplied string or an id this node minted, and
// none of them is a reason to hand-roll quoting.
func kv(name, value string) string {
	return name + ": " + langparser.QuoteString(value)
}

// appendOpt adds the argument only when it carries a value, so an omitted one
// is dropped from the payload rather than written as an empty string.
func appendOpt(args []string, name, value string) []string {
	if strings.TrimSpace(value) == "" {
		return args
	}
	return append(args, kv(name, value))
}

// lookupGithubAppGrant is the compare half of the insert-versus-update
// decision: the caller's existing grant for one GitHub account, by row id, or
// "" when there is none.
//
// It takes an ALREADY-BORROWED context rather than an owner argument, so the
// read and the write it decides between cannot run as two different actors.
func (s *Store) lookupGithubAppGrant(owned context.Context, externalId string) (string, error) {
	query := fmt.Sprintf(`query githubAppGrantByExternalId(externalId: %s)`, langparser.QuoteString(externalId))
	nodes, err := s.executeAndExtract(owned, query)
	if err != nil {
		return "", fmt.Errorf("identity.store: lookup github app grant: %w", err)
	}
	if len(nodes) == 0 || nodes[0] == nil {
		return "", nil
	}
	g := newFieldGetter(nodes[0])
	return firstNonEmpty(g.str("id"), nodes[0].GetId()), nil
}

// memqlStringList renders a []string as a MemQL list literal.
//
// Each element goes through langparser.QuoteString for the reason
// LookupMagicLinkById's comment gives: it supplies its own delimiters and
// escapes the whole C0 range, where a hand-written `"%s"` depends on a
// sanitizer a reader has to go and find. An empty or nil slice renders `[]`,
// which is a deliberate CLEAR rather than a no-op -- an installation the
// person removed must leave the list, and a merge could never take one out.
func memqlStringList(values []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(langparser.QuoteString(v))
	}
	b.WriteByte(']')
	return b.String()
}
