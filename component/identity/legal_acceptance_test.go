package identity

// legal_acceptance_test.go -- recording which legal-document versions a
// person accepted, in-process against a fake engine.
//
// The record is evidence, so the tests pin what makes it trustworthy rather
// than only that something was written: one entry per document per version
// (a second sign-in on the same version writes nothing), earlier entries kept
// verbatim, and the cross-replica read-modify-write protocol of memql#5431 --
// a fresh read, a row-version fence on the write, and a re-read after a
// concurrent write instead of a blind retry. The advisory lock half, which
// needs Postgres, is legal_acceptance_db_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

var (
	legalReadRe   = regexp.MustCompile(`^query userLegalAcceptanceById\(userId: "([^"]*)"\)$`)
	legalWriteRe  = regexp.MustCompile(`^mutation recordLegalAcceptance\(`)
	legalArrayRe  = regexp.MustCompile(`legalAcceptance: (\[.*?\])(?:,|\))`)
	legalUserIdRe = regexp.MustCompile(`userId: "([^"]*)"`)
)

// legalFakeEngine holds ONE user's acceptance history and answers the two
// constructs the recorder issues. Anything else lands in `unknown`, so a
// construct the store starts issuing without the fake modelling it fails the
// test instead of reading as "no rows".
type legalFakeEngine struct {
	mu      sync.Mutex
	userId  string
	entries []map[string]any
	version time.Time // zero = the row carries no createdAt
	missing bool      // true = no such user row

	// readWriteGap is slept on the READ so it lands between the recorder's
	// decision and its write -- the window the advisory lock closes.
	readWriteGap time.Duration
	// writeErrs are returned one per write, in order; a nil entry means the
	// write succeeds. onConflict runs when a write is refused with
	// ErrRowVersionChanged, standing in for the sibling replica whose write
	// moved the row.
	writeErrs  []error
	onConflict func(f *legalFakeEngine)

	reads   []context.Context
	writes  []legalFakeWrite
	unknown []string
}

type legalFakeWrite struct {
	ctx     context.Context
	stmt    string
	userId  string
	entries []map[string]any
}

func (f *legalFakeEngine) Execute(ctx context.Context, q string) (*memqlengine.ExecuteResult, error) {
	if m := legalReadRe.FindStringSubmatch(q); m != nil {
		f.mu.Lock()
		f.reads = append(f.reads, ctx)
		var node *memqlv1.MemoryNode
		if !f.missing && m[1] == f.userId {
			node = f.node()
		}
		gap := f.readWriteGap
		f.mu.Unlock()
		if gap > 0 {
			time.Sleep(gap)
		}
		bundle := &memqlv1.GraphBundle{}
		if node != nil {
			bundle.Nodes = []*memqlv1.MemoryNode{node}
		}
		return &memqlengine.ExecuteResult{Bundle: bundle}, nil
	}
	if legalWriteRe.MatchString(q) {
		f.mu.Lock()
		defer f.mu.Unlock()
		arr := legalArrayRe.FindStringSubmatch(q)
		uid := legalUserIdRe.FindStringSubmatch(q)
		if arr == nil || uid == nil {
			return nil, fmt.Errorf("fake: cannot read the write: %s", q)
		}
		var entries []map[string]any
		if err := json.Unmarshal([]byte(arr[1]), &entries); err != nil {
			return nil, fmt.Errorf("fake: legalAcceptance is not a JSON array: %v (%s)", err, q)
		}
		f.writes = append(f.writes, legalFakeWrite{ctx: ctx, stmt: q, userId: uid[1], entries: entries})
		if len(f.writeErrs) > 0 {
			err := f.writeErrs[0]
			f.writeErrs = f.writeErrs[1:]
			if err != nil {
				if errors.Is(err, memqlengine.ErrRowVersionChanged) && f.onConflict != nil {
					f.onConflict(f)
				}
				return nil, err
			}
		}
		if uid[1] != f.userId {
			return nil, fmt.Errorf("fake: write names user %q, have %q", uid[1], f.userId)
		}
		f.entries = entries
		f.version = f.version.Add(time.Millisecond)
		return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
	}
	f.mu.Lock()
	f.unknown = append(f.unknown, q)
	f.mu.Unlock()
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{}}, nil
}

// node renders the row the way the engine returns userLegalAcceptance:
// the id, the entries, and row.createdAt as the version.
func (f *legalFakeEngine) node() *memqlv1.MemoryNode {
	list := make([]any, 0, len(f.entries))
	for _, e := range f.entries {
		list = append(list, e)
	}
	payload := map[string]any{"id": f.userId, "legalAcceptance": list}
	if !f.version.IsZero() {
		payload["createdAt"] = f.version.UTC().Format(time.RFC3339Nano)
	}
	st, err := structpb.NewStruct(payload)
	if err != nil {
		panic(err)
	}
	return &memqlv1.MemoryNode{Id: f.userId, Concept: "v1:identity:user", Payload: st}
}

func (f *legalFakeEngine) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func entry(doc, version, at, ip string) map[string]any {
	return map[string]any{"documentType": doc, "version": version, "acceptedAt": at, "sourceIP": ip, "sessionId": "v1:identity:authSession:earlier"}
}

const legalSession = "v1:identity:authSession:this-sign-in"

var (
	legalNow     = time.Date(2026, 11, 9, 16, 10, 0, 0, time.UTC)
	legalInForce = []LegalDocumentVersion{{DocumentType: "tos", Version: "2.0.0"}, {DocumentType: "privacy", Version: "2.0.0"}}
)

func newLegalStore(f *legalFakeEngine) *Store {
	return &Store{Engine: f, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestLegalAcceptanceDueComparesEachDocumentWithItsNewestEntry(t *testing.T) {
	tos2 := LegalDocumentVersion{DocumentType: "tos", Version: "2.0.0"}
	priv2 := LegalDocumentVersion{DocumentType: "privacy", Version: "2.0.0"}
	cases := []struct {
		name     string
		recorded []LegalAcceptanceEntry
		want     []LegalDocumentVersion
	}{
		{"nothing recorded: every document is due", nil, []LegalDocumentVersion{tos2, priv2}},
		{"both current: nothing is due", []LegalAcceptanceEntry{
			{DocumentType: "tos", Version: "2.0.0"}, {DocumentType: "privacy", Version: "2.0.0"},
		}, nil},
		{"one document changed: only it is due", []LegalAcceptanceEntry{
			{DocumentType: "tos", Version: "1.0.0"}, {DocumentType: "privacy", Version: "2.0.0"},
		}, []LegalDocumentVersion{tos2}},
		{"the NEWEST entry decides, not any earlier one", []LegalAcceptanceEntry{
			{DocumentType: "tos", Version: "2.0.0"}, {DocumentType: "tos", Version: "1.0.0"}, {DocumentType: "privacy", Version: "2.0.0"},
		}, []LegalDocumentVersion{tos2}},
		{"a document never recorded is due beside current ones", []LegalAcceptanceEntry{
			{DocumentType: "tos", Version: "2.0.0"},
		}, []LegalDocumentVersion{priv2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := LegalAcceptanceDue(tc.recorded, legalInForce)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("due = %v, want %v", got, tc.want)
			}
		})
	}
	t.Run("a document with no version is never due", func(t *testing.T) {
		got := LegalAcceptanceDue(nil, []LegalDocumentVersion{{DocumentType: "tos"}, {DocumentType: "privacy", Version: "unknown"}})
		if len(got) != 0 {
			t.Fatalf("due = %v, want nothing: a version the front matter did not declare is not something a person can be said to have accepted", got)
		}
	})
}

func TestRecordLegalAcceptanceRecordsEachVersionInForceOnce(t *testing.T) {
	const user = "v1:identity:user:u1"
	eng := &legalFakeEngine{userId: user, version: legalNow.Add(-time.Hour)}
	store := newLegalStore(eng)

	recorded, err := store.RecordLegalAcceptance(context.Background(), user, legalInForce, "203.0.113.5", legalSession, legalNow)
	if err != nil {
		t.Fatalf("RecordLegalAcceptance: %v", err)
	}
	if len(eng.unknown) > 0 {
		t.Fatalf("the recorder issued constructs the fake does not model: %s", strings.Join(eng.unknown, "; "))
	}
	want := []LegalAcceptanceEntry{
		{DocumentType: "tos", Version: "2.0.0", AcceptedAt: "2026-11-09T16:10:00Z", SourceIP: "203.0.113.5", SessionId: legalSession},
		{DocumentType: "privacy", Version: "2.0.0", AcceptedAt: "2026-11-09T16:10:00Z", SourceIP: "203.0.113.5", SessionId: legalSession},
	}
	if fmt.Sprint(recorded) != fmt.Sprint(want) {
		t.Fatalf("recorded = %+v, want %+v", recorded, want)
	}
	if n := eng.writeCount(); n != 1 {
		t.Fatalf("%d writes, want 1 -- both documents land in ONE write of the whole array", n)
	}
	w := eng.writes[0]
	if w.userId != user {
		t.Errorf("write names user %q, want %q", w.userId, user)
	}
	if len(w.entries) != 2 || w.entries[0]["documentType"] != "tos" || w.entries[1]["documentType"] != "privacy" ||
		w.entries[0]["version"] != "2.0.0" || w.entries[1]["acceptedAt"] != "2026-11-09T16:10:00Z" || w.entries[1]["sourceIP"] != "203.0.113.5" ||
		w.entries[0]["sessionId"] != legalSession || w.entries[1]["sessionId"] != legalSession {
		t.Errorf("written array = %v, want the two entries with the version, the instant, the source IP and the session that recorded them", w.entries)
	}

	// The memql#5431 protocol, visible on the contexts the fake saw.
	if len(eng.reads) != 1 || !memqlengine.FreshReadFromContext(eng.reads[0]) {
		t.Errorf("the read before the write was not marked fresh: a sibling replica's write a moment ago would be invisible from this node's cache and then erased")
	}
	if !memqlengine.HasRowVersionFence(w.ctx, "v1:identity:user", user) {
		t.Errorf("the write carries no row-version fence for the user: a row that moved between the read and the write would be overwritten from a stale read")
	}
	if !auth.OriginFromContext(w.ctx).IsInternal() {
		t.Errorf("the write does not carry internal origin; recordLegalAcceptance is @serverOnly and would be refused by name")
	}

	// A second sign-in on the same versions is NOT a second record.
	again, err := store.RecordLegalAcceptance(context.Background(), user, legalInForce, "198.51.100.9", legalSession, legalNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("second RecordLegalAcceptance: %v", err)
	}
	if again != nil {
		t.Errorf("second sign-in recorded %+v, want nothing: the record is per version, not per session", again)
	}
	if n := eng.writeCount(); n != 1 {
		t.Errorf("%d writes after the second sign-in, want still 1", n)
	}
}

func TestRecordLegalAcceptanceKeepsEarlierEntriesAndAppendsOnlyTheChangedDocument(t *testing.T) {
	const user = "v1:identity:user:u2"
	eng := &legalFakeEngine{userId: user, version: legalNow.Add(-time.Hour), entries: []map[string]any{
		entry("tos", "1.0.0", "2026-01-02T03:04:05Z", "192.0.2.1"),
		entry("privacy", "2.0.0", "2026-01-02T03:04:05Z", "192.0.2.1"),
	}}
	store := newLegalStore(eng)

	recorded, err := store.RecordLegalAcceptance(context.Background(), user, legalInForce, "203.0.113.5", legalSession, legalNow)
	if err != nil {
		t.Fatalf("RecordLegalAcceptance: %v", err)
	}
	if len(recorded) != 1 || recorded[0].DocumentType != "tos" || recorded[0].Version != "2.0.0" {
		t.Fatalf("recorded = %+v, want exactly the new tos version", recorded)
	}
	if n := eng.writeCount(); n != 1 {
		t.Fatalf("%d writes, want 1", n)
	}
	got := eng.writes[0].entries
	if len(got) != 3 {
		t.Fatalf("written array has %d entries, want 3: the two earlier ones kept and one appended -- %v", len(got), got)
	}
	if fmt.Sprint(got[0]) != fmt.Sprint(entry("tos", "1.0.0", "2026-01-02T03:04:05Z", "192.0.2.1")) ||
		fmt.Sprint(got[1]) != fmt.Sprint(entry("privacy", "2.0.0", "2026-01-02T03:04:05Z", "192.0.2.1")) {
		t.Errorf("earlier entries were not kept verbatim: %v", got[:2])
	}
	if got[2]["documentType"] != "tos" || got[2]["version"] != "2.0.0" || got[2]["acceptedAt"] != "2026-11-09T16:10:00Z" {
		t.Errorf("appended entry = %v", got[2])
	}
}

// A sibling replica recorded the same sign-in's versions between this node's
// read and its write. The fence refuses the stale write; the recorder must
// READ AGAIN and append only what is still due -- never repeat the write it
// prepared, which would duplicate the sibling's entry.
func TestRecordLegalAcceptanceRereadsAfterAConcurrentWrite(t *testing.T) {
	const user = "v1:identity:user:u3"
	eng := &legalFakeEngine{userId: user, version: legalNow.Add(-time.Hour),
		writeErrs: []error{memqlengine.ErrRowVersionChanged}}
	eng.onConflict = func(f *legalFakeEngine) {
		f.entries = append(f.entries, entry("tos", "2.0.0", "2026-11-09T16:09:59Z", "198.51.100.2"))
		f.version = f.version.Add(time.Second)
	}
	store := newLegalStore(eng)

	recorded, err := store.RecordLegalAcceptance(context.Background(), user, legalInForce, "203.0.113.5", legalSession, legalNow)
	if err != nil {
		t.Fatalf("RecordLegalAcceptance: %v", err)
	}
	if len(recorded) != 1 || recorded[0].DocumentType != "privacy" {
		t.Fatalf("recorded = %+v, want only privacy: the sibling already recorded tos", recorded)
	}
	if len(eng.reads) != 2 {
		t.Errorf("%d reads, want 2 -- the conflict must be answered by a re-read, not a retry of the same write", len(eng.reads))
	}
	if n := eng.writeCount(); n != 2 {
		t.Fatalf("%d writes, want 2 (the refused one and the one that landed)", n)
	}
	final := eng.writes[1].entries
	if len(final) != 2 || final[0]["documentType"] != "tos" || final[0]["sourceIP"] != "198.51.100.2" || final[1]["documentType"] != "privacy" {
		t.Errorf("final array = %v, want the sibling's tos entry kept and privacy appended", final)
	}
}

func TestRecordLegalAcceptanceGivesUpAfterRepeatedConflicts(t *testing.T) {
	const user = "v1:identity:user:u4"
	eng := &legalFakeEngine{userId: user, version: legalNow.Add(-time.Hour),
		writeErrs: []error{memqlengine.ErrRowVersionChanged, memqlengine.ErrRowVersionChanged, memqlengine.ErrRowVersionChanged, memqlengine.ErrRowVersionChanged}}
	store := newLegalStore(eng)
	_, err := store.RecordLegalAcceptance(context.Background(), user, legalInForce, "203.0.113.5", legalSession, legalNow)
	if !errors.Is(err, memqlengine.ErrRowVersionChanged) {
		t.Fatalf("err = %v, want the version conflict surfaced after the attempts run out", err)
	}
	if n := eng.writeCount(); n != 3 {
		t.Errorf("%d writes, want 3 attempts, no more: the row is being written continuously by somebody and a sign-in must not spin on it", n)
	}
}

func TestRecordLegalAcceptanceRefusesWithoutARowOrAVersion(t *testing.T) {
	t.Run("no such user", func(t *testing.T) {
		eng := &legalFakeEngine{userId: "v1:identity:user:gone", missing: true}
		_, err := newLegalStore(eng).RecordLegalAcceptance(context.Background(), "v1:identity:user:gone", legalInForce, "", legalSession, legalNow)
		if err == nil {
			t.Fatal("recording against a missing row succeeded; there is nothing to append to")
		}
		if eng.writeCount() != 0 {
			t.Error("a write was attempted for a missing row")
		}
	})
	t.Run("a row without a version is not written", func(t *testing.T) {
		eng := &legalFakeEngine{userId: "v1:identity:user:u5"}
		_, err := newLegalStore(eng).RecordLegalAcceptance(context.Background(), "v1:identity:user:u5", legalInForce, "", legalSession, legalNow)
		if err == nil {
			t.Fatal("recording without the row's version succeeded; the fence would have nothing to compare and the write could erase a sibling's")
		}
		if eng.writeCount() != 0 {
			t.Error("a write was attempted without a version to fence on")
		}
	})
	t.Run("nothing in force means nothing to do", func(t *testing.T) {
		eng := &legalFakeEngine{userId: "v1:identity:user:u6", version: legalNow}
		recorded, err := newLegalStore(eng).RecordLegalAcceptance(context.Background(), "v1:identity:user:u6", nil, "", legalSession, legalNow)
		if err != nil || recorded != nil {
			t.Fatalf("recorded, err = %v, %v; want nothing and no error", recorded, err)
		}
		if len(eng.reads) != 0 {
			t.Error("a read was made with no document in force")
		}
	})
}

func TestLegalAcceptanceForUserDecodesTheHistoryAndItsVersion(t *testing.T) {
	const user = "v1:identity:user:u7"
	eng := &legalFakeEngine{userId: user, version: legalNow, entries: []map[string]any{
		entry("tos", "1.0.0", "2026-01-02T03:04:05Z", "192.0.2.1"),
		entry("privacy", "1.0.0", "2026-01-02T03:04:06Z", ""),
	}}
	entries, version, err := newLegalStore(eng).LegalAcceptanceForUser(context.Background(), user)
	if err != nil {
		t.Fatalf("LegalAcceptanceForUser: %v", err)
	}
	want := []LegalAcceptanceEntry{
		{DocumentType: "tos", Version: "1.0.0", AcceptedAt: "2026-01-02T03:04:05Z", SourceIP: "192.0.2.1", SessionId: "v1:identity:authSession:earlier"},
		{DocumentType: "privacy", Version: "1.0.0", AcceptedAt: "2026-01-02T03:04:06Z", SessionId: "v1:identity:authSession:earlier"},
	}
	if fmt.Sprint(entries) != fmt.Sprint(want) {
		t.Errorf("entries = %+v, want %+v", entries, want)
	}
	if !version.Equal(legalNow) {
		t.Errorf("version = %v, want the row's createdAt %v", version, legalNow)
	}
	if len(eng.reads) != 1 || !memqlengine.FreshReadFromContext(eng.reads[0]) {
		t.Errorf("the read was not fresh")
	}
}

// Every statement the recorder renders must parse against the real DSL tree.
// A fake engine parses nothing, so a renamed construct or a rendering slip
// would show only here -- or in production. Parse carries client origin and
// cannot be told otherwise: a query's @serverOnly is enforced while the plan
// is built, so the read statement's one admissible refusal is the server-only
// one (which itself proves the name resolved); a mutation's is enforced at
// execution, so the write statement must parse clean, arguments included. The
// full round trip runs against the real engine in
// TestRecordLegalAcceptanceAgainstARealEngine (Postgres-gated).
func TestLegalAcceptanceStatementsParseAgainstTheRealTree(t *testing.T) {
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	real, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	real.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := real.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine init: %v", err)
	}

	const user = "v1:identity:user:u8"
	eng := &legalFakeEngine{userId: user, version: legalNow, entries: []map[string]any{
		entry("tos", "1.0.0", "2026-01-02T03:04:05Z", `203.0.113.7`),
	}}
	store := newLegalStore(eng)
	if _, err := store.RecordLegalAcceptance(context.Background(), user, legalInForce, "203.0.113.5", legalSession, legalNow); err != nil {
		t.Fatalf("RecordLegalAcceptance: %v", err)
	}
	read := fmt.Sprintf(`query userLegalAcceptanceById(userId: %q)`, user)
	switch _, err := real.Parse(read); {
	case err == nil:
		t.Errorf("Parse accepted %s from a CLIENT origin; the query is meant to be @serverOnly", read)
	case !strings.Contains(err.Error(), "is server-only and cannot be called by a client"):
		t.Errorf("the engine refuses the read for a reason other than its origin:\n  %s\n  %v", read, err)
	}
	if len(eng.writes) == 0 {
		t.Fatal("the recorder wrote nothing to parse")
	}
	for _, w := range eng.writes {
		if _, err := real.Parse(w.stmt); err != nil {
			t.Errorf("the engine refuses a write the recorder renders:\n  %s\n  %v", w.stmt, err)
		}
	}
}
