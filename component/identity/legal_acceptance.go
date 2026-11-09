package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// legal_acceptance.go -- which legal-document versions a person accepted.
//
// The Terms of Service and the Privacy Notice say a person accepts them by
// signing in, and that the version in force is shown on the page. This file
// is the record of that: on every sign-in that creates a session, one entry
// per document whose version in force the account has not yet accepted is
// appended to v1:identity:user.legalAcceptance, as {documentType, version,
// acceptedAt, sourceIP}. Once per VERSION, never per session: a person who
// signs in a hundred times under version 2.0.0 has one entry for it, and the
// day the operator publishes 2.1.0 their next sign-in appends one more.
//
// The record is evidence, so what makes it trustworthy is spelled out here
// rather than left to the reader:
//
//   - earlier entries are never edited or dropped. recordLegalAcceptance
//     replaces the whole array (update{} has no append), so the caller reads,
//     appends and writes everything back; a stale read would therefore ERASE
//     a sibling's entry, which is the next point;
//   - two identity replicas can finish one person's sign-ins within
//     milliseconds (a browser retrying, a passkey login beside a device-code
//     redemption). The recorder holds a per-user Postgres advisory lock
//     across its read and its write, reads FRESH (never from this node's
//     result cache, which may not yet have seen the sibling's write), and
//     writes under a row-version fence stamped after the version it read
//     (memql#5431). A row that moved in between is read AGAIN and only what
//     is still due is appended -- never the write that was prepared, which
//     would duplicate the sibling's entry;
//   - the only writer is the identity service's session seam
//     (component/identity/http.createSessionRow). The mutation is
//     @serverOnly, so no wire caller can author anybody's history, their own
//     included.
//
// Tests: legal_acceptance_test.go (the protocol against a fake engine) and
// legal_acceptance_db_test.go (the lock against Postgres). The operator's
// account is docs/public/operate/auth/legal-pages.md.

// LegalDocumentVersion names the version in force of one legal document, as
// the identity web server reads it from the document's front matter.
type LegalDocumentVersion struct {
	// DocumentType is "tos" or "privacy", the two values the user concept
	// declares for an entry.
	DocumentType string
	// Version is the front matter's `version`.
	Version string
}

// LegalAcceptanceEntry is one recorded acceptance, in the shape stored on
// v1:identity:user.legalAcceptance. AcceptedAt is RFC 3339 in UTC.
type LegalAcceptanceEntry struct {
	DocumentType string `json:"documentType"`
	Version      string `json:"version"`
	AcceptedAt   string `json:"acceptedAt"`
	SourceIP     string `json:"sourceIP"`
}

// legalAcceptanceConcept is the row the history lives on.
const legalAcceptanceConcept = "v1:identity:user"

// legalAcceptanceLockClass namespaces the per-user advisory lock the recorder
// holds across its read and write. Two-key space, like the magic-link gate;
// 0x4C474C41 spells "LGLA" and collides with none of the classes in use
// (magic_link_gate.go lists them).
const legalAcceptanceLockClass int32 = 0x4C474C41

// legalAcceptanceLockTimeout bounds the wait for the lock. Past it the
// recorder proceeds unlocked: the row-version fence still refuses a lost
// update, so the cost of an unlocked run is a re-read, not a wrong record.
const legalAcceptanceLockTimeout = 5 * time.Second

// legalAcceptanceWriteAttempts bounds the re-read-and-retry loop. The row is
// also written by sign-in bookkeeping and admin edits; three attempts cover
// a burst of those without letting a sign-in spin on a row that is being
// written continuously.
const legalAcceptanceWriteAttempts = 3

// errLegalAcceptanceNoUser says the read found no active user row.
var errLegalAcceptanceNoUser = errors.New("identity.store: legal acceptance: no such user")

// LegalAcceptanceDue returns the documents in force whose version differs
// from the NEWEST entry recorded for that document -- the last one in array
// order, since the array is append-only. A document in force with no version
// (an override file without front matter reads as "unknown") is never due: a
// record saying somebody accepted version "unknown" is not evidence.
//
// The comparison is "differs", not "is newer": an operator who rolls a
// document back records the rolled-back version at the next sign-in, which
// is what the person accepted.
func LegalAcceptanceDue(recorded []LegalAcceptanceEntry, inForce []LegalDocumentVersion) []LegalDocumentVersion {
	newest := make(map[string]string, 2)
	for _, e := range recorded {
		newest[strings.TrimSpace(e.DocumentType)] = strings.TrimSpace(e.Version)
	}
	var due []LegalDocumentVersion
	for _, d := range inForce {
		kind := strings.TrimSpace(d.DocumentType)
		version := strings.TrimSpace(d.Version)
		if kind == "" || version == "" || version == "unknown" {
			continue
		}
		if current, ok := newest[kind]; ok && current == version {
			continue
		}
		due = append(due, LegalDocumentVersion{DocumentType: kind, Version: version})
	}
	return due
}

// LegalAcceptanceForUser reads one user's acceptance history and the row
// version it was read at (row.createdAt of the newest version), FRESH -- the
// caller is either about to write a version derived from it or is showing a
// person their own history, and neither wants this node's cache.
//
// userLegalAcceptanceById is @serverOnly and projects the history beside the
// row's id and version, nothing else; the caller is scoped by the HANDLER
// (the session's subject), which is what keeps a caller-supplied id off the
// wire. errLegalAcceptanceNoUser when no active row exists.
func (s *Store) LegalAcceptanceForUser(ctx context.Context, userId string) ([]LegalAcceptanceEntry, time.Time, error) {
	userId = strings.TrimSpace(userId)
	if userId == "" {
		return nil, time.Time{}, errors.New("identity.store: legal acceptance: user id required")
	}
	query := fmt.Sprintf(`query userLegalAcceptanceById(userId: %s)`, dslJSONString(userId))
	nodes, err := s.executeAndExtractInternal(memqlengine.ContextWithFreshRead(ctx), query)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("identity.store: read legal acceptance: %w", err)
	}
	if len(nodes) == 0 || nodes[0] == nil {
		return nil, time.Time{}, errLegalAcceptanceNoUser
	}
	entries, err := legalAcceptanceFromNode(nodes[0])
	if err != nil {
		return nil, time.Time{}, err
	}
	return entries, rowVersion(nodes[0]), nil
}

// legalAcceptanceFromNode decodes the stored array. An entry the schema
// accepted is a plain object, so JSON is the one decoder that needs no
// knowledge of structpb's value shapes.
func legalAcceptanceFromNode(node *memqlv1.MemoryNode) ([]LegalAcceptanceEntry, error) {
	if node == nil || node.Payload == nil {
		return nil, nil
	}
	raw, ok := node.Payload.AsMap()["legalAcceptance"]
	if !ok || raw == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("identity.store: legal acceptance: encode stored history: %w", err)
	}
	var entries []LegalAcceptanceEntry
	if err := json.Unmarshal(encoded, &entries); err != nil {
		return nil, fmt.Errorf("identity.store: legal acceptance: stored history is not a list of entries: %w", err)
	}
	return entries, nil
}

// rowVersion is the version a row was read at: its createdAt, from the
// projection when the shape carries row.createdAt, else from the node.
func rowVersion(node *memqlv1.MemoryNode) time.Time {
	if at := newFieldGetter(node).time("createdAt"); !at.IsZero() {
		return at
	}
	if node != nil && node.GetCreatedAt() != nil {
		return node.GetCreatedAt().AsTime()
	}
	return time.Time{}
}

// RecordLegalAcceptance appends, for the user, one entry per document in
// force whose version the account has not yet accepted, stamped with `now`
// and the sign-in's source IP. It returns the entries it appended, nil when
// nothing was due.
//
// The protocol is the one memql#5431 asks of every cross-replica
// read-modify-write: the per-user advisory lock around the whole thing, a
// fresh read, and the write under a row-version fence on the version read.
// A fenced write the engine refuses (the row moved) is answered by reading
// again, up to legalAcceptanceWriteAttempts times; the error surfaces after
// that, and the caller treats it as bookkeeping to log, never as a reason
// to refuse the sign-in.
func (s *Store) RecordLegalAcceptance(ctx context.Context, userId string, inForce []LegalDocumentVersion, sourceIP string, now time.Time) ([]LegalAcceptanceEntry, error) {
	if s == nil || s.Engine == nil {
		return nil, errNoEngine
	}
	userId = strings.TrimSpace(userId)
	if userId == "" {
		return nil, errors.New("identity.store: legal acceptance: user id required")
	}
	// Nothing with a version in force: nothing to read, nothing to write.
	if len(LegalAcceptanceDue(nil, inForce)) == 0 {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	acceptedAt := now.UTC().Format(time.RFC3339Nano)

	var recorded []LegalAcceptanceEntry
	err := s.withLegalAcceptanceGate(ctx, userId, func(ctx context.Context) error {
		for attempt := 1; ; attempt++ {
			entries, version, err := s.LegalAcceptanceForUser(ctx, userId)
			if err != nil {
				return err
			}
			if version.IsZero() {
				// The fence compares against this; with nothing to compare,
				// the write could erase a sibling's entry unnoticed.
				return fmt.Errorf("identity.store: legal acceptance: user %s carries no row version to fence on", userId)
			}
			due := LegalAcceptanceDue(entries, inForce)
			if len(due) == 0 {
				recorded = nil
				return nil
			}
			appended := make([]LegalAcceptanceEntry, 0, len(due))
			for _, d := range due {
				appended = append(appended, LegalAcceptanceEntry{
					DocumentType: d.DocumentType,
					Version:      d.Version,
					AcceptedAt:   acceptedAt,
					SourceIP:     strings.TrimSpace(sourceIP),
				})
			}
			all := make([]LegalAcceptanceEntry, 0, len(entries)+len(appended))
			all = append(all, entries...)
			all = append(all, appended...)
			stmt, err := langparser.RenderCall("mutation recordLegalAcceptance", map[string]any{
				"userId":          userId,
				"legalAcceptance": all,
			})
			if err != nil {
				return fmt.Errorf("identity.store: record legal acceptance: %w", err)
			}
			// The fence pins the version read above (ContextWithRowVersionFence
			// also marks the write's own reads fresh); internal origin because
			// the mutation is @serverOnly and this seam is its one caller.
			writeCtx := auth.ContextWithInternalOrigin(
				memqlengine.ContextWithRowVersionFence(ctx, legalAcceptanceConcept, userId, version))
			if _, err := s.Engine.Execute(writeCtx, stmt); err != nil {
				if errors.Is(err, memqlengine.ErrRowVersionChanged) && attempt < legalAcceptanceWriteAttempts {
					continue
				}
				return fmt.Errorf("identity.store: record legal acceptance: %w", err)
			}
			recorded = appended
			return nil
		}
	})
	return recorded, err
}

// legalAcceptanceLockKey derives the advisory objid for one user id. FNV-32a,
// as the magic-link gate derives its key.
func legalAcceptanceLockKey(userId string) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(userId))
	return int32(h.Sum32())
}

// withLegalAcceptanceGate runs fn while holding the per-user advisory lock.
//
// The lock is taken on the DIRECT connection, for the reason magic_link_gate.go
// gives: a transaction-mode pooler would drop a session-scoped lock silently.
// The degradation policy is the gate's too: no getter, no database, a
// connection or lock error, or the timeout all run fn UNLOCKED and warn. The
// lock serializes the read and the write so siblings do not spend their
// attempts refusing each other; the row-version fence inside fn is what keeps
// an unlocked run from losing an entry.
func (s *Store) withLegalAcceptanceGate(ctx context.Context, userId string, fn func(context.Context) error) error {
	if s == nil || s.DirectDB == nil {
		return fn(ctx)
	}
	db := s.DirectDB()
	if db == nil {
		return fn(ctx)
	}
	lockCtx, cancel := context.WithTimeout(ctx, legalAcceptanceLockTimeout)
	defer cancel()
	conn, err := db.Conn(lockCtx)
	if err != nil {
		s.warn("identity.store: legal acceptance gate connection unavailable; proceeding unlocked", "error", err.Error())
		return fn(ctx)
	}
	defer func() { _ = conn.Close() }()

	objid := legalAcceptanceLockKey(userId)
	if _, err := conn.ExecContext(lockCtx, "SELECT pg_advisory_lock($1, $2)", legalAcceptanceLockClass, objid); err != nil {
		s.warn("identity.store: legal acceptance gate not acquired; proceeding unlocked", "error", err.Error())
		return fn(ctx)
	}
	defer func() {
		// Unlock on a background context so a client disconnect mid-flight
		// cannot leave the lock held for the life of the pooled connection.
		uctx, ucancel := context.WithTimeout(context.Background(), legalAcceptanceLockTimeout)
		defer ucancel()
		if _, err := conn.ExecContext(uctx, "SELECT pg_advisory_unlock($1, $2)", legalAcceptanceLockClass, objid); err != nil {
			s.warn("identity.store: legal acceptance gate unlock failed", "error", err.Error())
		}
	}()
	return fn(ctx)
}

// unusedSQL keeps database/sql imported for the DirectDB signature above.
var _ *sql.DB
