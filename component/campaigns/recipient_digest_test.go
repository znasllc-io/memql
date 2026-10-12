package campaigns

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/database"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// recipient_digest_test.go -- the digest recipient rows carry, so a signup finds
// the rows a mailbox holds by that key instead of walking the roster
// (memql#6022, F9).
//
// One normalization is written four times: here (EmailDigest), in the database
// package (RecipientEmailDigest, which the backfill hashes with and which cannot
// import this package), in the email-rules package (addressDigest, which does not
// import this one either), and in the DSL (addRecipient's
// `hash(lower(trim(args.email)))`). They are held together by tests rather than
// by a shared import, because a disagreement is not an error anywhere: it is a
// signup that fails to find the member who is there, and a second membership
// for a mailbox that already has one.

// TestRecipientDigestBackfillMatchesEmailDigest: the backfill's digest is the
// one this package looks members up by, and what it stamps is what addRecipient
// stamps.
func TestRecipientDigestBackfillMatchesEmailDigest(t *testing.T) {
	usable := 0
	for _, addr := range []string{
		" Foo@Example.COM ", "ÄBC@x.test", "no-at-sign", "a b@c.test",
		"", "   ", "@leading", "trailing@", "line\nbreak@example.test", "tab\tin@example.test",
		"Person+News@Example.Test", `"a@b"@example.test`, "\u00a0nbsp@example.test\u00a0", "in\u00a0side@example.test", `Q"uote\back?slash{brace}'@Example.TEST`,
	} {
		got, want := database.RecipientEmailDigest(addr), EmailDigest(addr)
		if got != want {
			t.Errorf("RecipientEmailDigest(%q) = %q, EmailDigest = %q: the backfill would stamp rows this package never finds, "+
				"or look up rows the backfill never stamped", addr, got, want)
		}
		if want != "" {
			usable++
			// For an address the key it is looked up by is the stamp it is written with.
			if stamp := database.RecipientEmailStamp(addr); stamp != want {
				t.Errorf("RecipientEmailStamp(%q) = %q, EmailDigest = %q: the sweep would stamp a row with a digest its own lookup never asks for", addr, stamp, want)
			}
		}
	}
	// The instrument can move: an implementation answering "" for everything
	// would pass the unusable addresses above on its own.
	if usable < 5 {
		t.Fatalf("only %d of the addresses have a digest; the comparison above proves nothing", usable)
	}
}

// TestAddRecipientStampsTheDigestTheGoFunctionComputes: the DSL stamp and
// EmailDigest agree on every address either can use, evaluated off the
// mutation's own source the way delivery_id_test.go does for the ledger id.
func TestAddRecipientStampsTheDigestTheGoFunctionComputes(t *testing.T) {
	block := mutationBlock(t, campaignsDSL(t, "mutations.memql"), "mutation recipient addRecipient")
	stamp := collapsedExprAt(t, block, "emailDigest:")
	const want = `emailDigest: hash(lower(trim(args.email)))`
	if stamp != want {
		t.Fatalf("addRecipient's digest stamp has changed.\n got: %s\nwant: %s\n\nEmailDigest, database.RecipientEmailDigest and the email-rules "+
			"addressDigest compute the same SHA-256 of the same trimmed, lower-cased address; if this expression stops doing "+
			"that, every signup misses the members the stamp wrote", stamp, want)
	}
	expr, err := languageParser.ParseV1Expression(strings.TrimPrefix(stamp, "emailDigest: "))
	if err != nil {
		t.Fatalf("the stamp expression does not parse: %v", err)
	}
	// Addresses either side can use: the stamp is the lookup key.
	for _, addr := range []string{" Foo@Example.TEST ", "ÄBC@x.test", "person@example.test", "Person+News@Example.Test", "\u00a0nbsp@example.test\u00a0"} {
		v, err := memql.EvalExpr(context.Background(), expr, memql.MapScope{"args": map[string]any{"email": addr}}, memql.EvalOptions{})
		if err != nil {
			t.Fatalf("evaluating the stamp for %q: %v", addr, err)
		}
		if got, want := v, EmailDigest(addr); got != want || want == "" {
			t.Errorf("addRecipient stamps %v for %q, EmailDigest says %q", got, addr, want)
		}
	}
	// Text that cannot be a mailbox is stamped by the mutation with the digest of
	// whatever it was given, and has none in EmailDigest because nothing looks it
	// up. The sweep stamps it the same way (a scrubbed recipient holds "", a
	// malformed import holds whatever it held), so the rows the sweep fills in are
	// the rows addRecipient would have written.
	for _, text := range []string{"", "   ", "no-at-sign", " No-At-Sign\n", "a b@c.test", "@leading", "trailing@", "line\nbreak@example.test", "ÄBC", "\u00a0nbsp\u00a0"} {
		v, err := memql.EvalExpr(context.Background(), expr, memql.MapScope{"args": map[string]any{"email": text}}, memql.EvalOptions{})
		if err != nil {
			t.Fatalf("evaluating the stamp for %q: %v", text, err)
		}
		if got, want := v, database.RecipientEmailStamp(text); got != want {
			t.Errorf("addRecipient stamps %v for the text %q, the sweep would stamp %q", got, text, want)
		}
		if EmailDigest(text) != "" {
			t.Errorf("EmailDigest(%q) = %q: this list is of texts that are not addresses", text, EmailDigest(text))
		}
	}
}

// mailboxEngine answers each of the reads a signup makes for a mailbox with the
// rows it was given, and records what it was asked.
type mailboxEngine struct {
	mu            sync.Mutex
	calls         []string
	byDigest      []map[string]any
	withoutDigest []map[string]any
	roster        []map[string]any
	// rosterPageSize pages the roster, as the engine does; zero answers it in one page.
	rosterPageSize int
	// rosterSize is what the count of the roster says; zero means the roster's length.
	rosterSize int
}

// rosterEnvelope answers one page of the roster for the cursor it arrived with.
func (e *mailboxEngine) rosterEnvelope(cursor string) map[string]any {
	size := e.rosterPageSize
	if size <= 0 {
		size = len(e.roster) + 1
	}
	from := 0
	if cursor != "" {
		_, _ = fmt.Sscanf(cursor, "%d", &from)
	}
	to := min(from+size, len(e.roster))
	out := make([]any, 0, to-from)
	for _, r := range e.roster[from:to] {
		out = append(out, r)
	}
	envelope := map[string]any{"output": out}
	if to < len(e.roster) {
		envelope["cursor"] = fmt.Sprintf("%d", to)
	}
	return envelope
}

func (e *mailboxEngine) Execute(ctx context.Context, q string) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, q)
	rows := []map[string]any{}
	switch {
	case strings.HasPrefix(q, "query recipientInAudienceByDigest"):
		rows = e.byDigest
	case strings.HasPrefix(q, "query recipientsWithoutDigestInAudience"):
		rows = e.withoutDigest
	case strings.HasPrefix(q, "query audienceRosterForSend"):
		return e.rosterEnvelope(memql.CursorFromContext(ctx)), nil
	case strings.HasPrefix(q, "query audienceRosterSize"):
		size := e.rosterSize
		if size == 0 {
			size = len(e.roster)
		}
		return map[string]any{"output": []any{map[string]any{"count": size}}}, nil
	}
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, r)
	}
	return map[string]any{"output": out}, nil
}

func (e *mailboxEngine) OrganizationCapable(context.Context, string, string, string) bool {
	return false
}

func (e *mailboxEngine) called(prefix string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func mailboxRow(short, email, status string) map[string]any {
	return map[string]any{"id": "v1:campaigns:recipient:" + short, "email": email, "subscriptionStatus": status}
}

func ids(rs []Recipient) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

// TestRecipientsInAudienceByDigestReturnsEveryMatchAndNeverAsksForNothing: one
// read, every row of the window in the engine's oldest-first order, and no read at
// all for a digest that is not one.
func TestRecipientsInAudienceByDigestReturnsEveryMatchAndNeverAsksForNothing(t *testing.T) {
	digest := EmailDigest("member@example.test")
	engine := &mailboxEngine{byDigest: []map[string]any{
		{"id": "v1:campaigns:recipient:older", "accountId": "v1:accounts:account:org-1", "email": " Member@Example.test ", "subscriptionStatus": "subscribed"},
		{"id": "v1:campaigns:recipient:newer", "email": "member@example.test", "subscriptionStatus": "unsubscribed"},
	}}
	store := NewStore(engine)

	got, err := store.RecipientsInAudienceByDigest(context.Background(), "aud-1", digest)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "older" || got[0].AccountID != "org-1" || got[1].ID != "newer" || got[1].SubscriptionStatus != "unsubscribed" {
		t.Errorf("got %+v, want both rows, the older first: the second one is the one that opted out", got)
	}
	if len(engine.calls) != 1 || engine.calls[0] != `query recipientInAudienceByDigest(audienceId: "aud-1", emailDigest: "`+digest+`")` {
		t.Errorf("asked %q, want one read of the audience by digest", engine.calls)
	}

	engine.calls = nil
	for _, none := range []string{"", EmailDigest("no-at-sign")} {
		if got, err := store.RecipientsInAudienceByDigest(context.Background(), "aud-1", none); len(got) != 0 || err != nil {
			t.Errorf("digest %q: %v %v", none, got, err)
		}
	}
	if len(engine.calls) != 0 {
		t.Errorf("a digest of nothing was sent to the engine as %q: an empty digest could match every row that has none", engine.calls)
	}
}

// TestMailboxMembersChecksEachRowsOwnAddress: the digest is a key, not proof. A
// row that carries this mailbox's digest and another address is not this mailbox.
func TestMailboxMembersChecksEachRowsOwnAddress(t *testing.T) {
	engine := &mailboxEngine{
		byDigest: []map[string]any{
			mailboxRow("forged", "someone-else@example.test", "unsubscribed"),
			mailboxRow("real", " Member@Example.TEST ", "subscribed"),
		},
		withoutDigest: []map[string]any{
			mailboxRow("other", "other@example.test", "unsubscribed"),
			mailboxRow("bare", "MEMBER@example.test", "bounced"),
		},
	}
	got, err := NewStore(engine).MailboxMembers(context.Background(), "aud-1", "member@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"real", "bare"}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf("members %v, want %v: the row that merely carries the digest is someone else's, and the digest-less row is found by its address", ids(got), want)
	}
	if engine.called("query audienceRosterForSend") != 0 {
		t.Error("a short page of rows with no digest sent the lookup to the roster")
	}
}

// TestMailboxMembersReadsTheRosterWhenThePageOfRowsWithoutADigestIsFull: a full
// page says nothing about the rows beyond it, so the answer comes from the whole
// audience -- including a row the digest lookup also found.
func TestMailboxMembersReadsTheRosterWhenThePageOfRowsWithoutADigestIsFull(t *testing.T) {
	var page []map[string]any
	for i := 0; i < digestLessWindow; i++ {
		page = append(page, recipientRow(fmt.Sprintf("filler-%02d", i), "filler@example.test", "subscribed"))
	}
	engine := &mailboxEngine{
		byDigest:      []map[string]any{mailboxRow("stamped", "member@example.test", "subscribed")},
		withoutDigest: page,
		roster:        append(append([]map[string]any{}, page...), mailboxRow("stamped", "member@example.test", "subscribed"), mailboxRow("beyond", "Member@Example.test", "unsubscribed")),
	}
	got, err := NewStore(engine).MailboxMembers(context.Background(), "aud-1", "member@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"stamped", "beyond"}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf("members %v, want %v", ids(got), want)
	}
	if engine.called("query audienceRosterForSend") != 1 {
		t.Errorf("the roster was read %d times, want once", engine.called("query audienceRosterForSend"))
	}
}

// fullPageOfStrangers is what makes MailboxMembers read the roster: a full page of
// rows without a digest, none of them the mailbox asked about.
func fullPageOfStrangers() []map[string]any {
	var page []map[string]any
	for i := 0; i < digestLessWindow; i++ {
		page = append(page, mailboxRow(fmt.Sprintf("stranger-%02d", i), fmt.Sprintf("stranger%02d@example.test", i), "subscribed"))
	}
	return page
}

// rosterOf builds a roster of n other people with the given rows put in at the
// given positions.
func rosterOf(n int, at map[int]map[string]any) []map[string]any {
	var roster []map[string]any
	for i := 0; i < n; i++ {
		if row, ok := at[i]; ok {
			roster = append(roster, row)
			continue
		}
		roster = append(roster, mailboxRow(fmt.Sprintf("other-%02d", i), fmt.Sprintf("other%02d@example.test", i), "subscribed"))
	}
	return roster
}

// TestMailboxMembersAnswersAnAudienceBeyondTheWalkWithoutWalkingIt: the walk reads
// at most mailboxRosterScanPages pages, so an audience bigger than that cannot end
// any other way than "cannot tell" -- and the audience's size is one count.
func TestMailboxMembersAnswersAnAudienceBeyondTheWalkWithoutWalkingIt(t *testing.T) {
	limit := mailboxRosterScanPages
	mailboxRosterScanPages = 2
	defer func() { mailboxRosterScanPages = limit }()

	for _, tc := range []struct {
		name        string
		size        int
		undecidable bool
	}{
		{"one past what the walk reads", 2*rosterPageRows + 1, true},
		{"far past it", 1000000, true},
		{"exactly what it reads", 2 * rosterPageRows, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &mailboxEngine{withoutDigest: fullPageOfStrangers(), roster: rosterOf(6, nil), rosterSize: tc.size}
			_, err := NewStore(engine).MailboxMembers(context.Background(), "aud-1", "member@example.test")
			if tc.undecidable != errors.Is(err, ErrMailboxUndecidable) {
				t.Fatalf("size %d: error %v, want undecidable = %v", tc.size, err, tc.undecidable)
			}
			if sizes := engine.called("query audienceRosterSize"); sizes != 1 {
				t.Errorf("the audience was counted %d times, want once", sizes)
			}
			if pages := engine.called("query audienceRosterForSend"); tc.undecidable && pages != 0 {
				t.Errorf("the roster was walked for %d pages although the count already said it cannot be searched to the end", pages)
			}
		})
	}
}

// TestMailboxMembersDoesNotReadTheRosterWhenAnOptOutIsAlreadyInHand: a row that
// opted out, found by the digest or among the digest-less page, settles the answer.
// The roster walk costs seconds a page and could not change it.
func TestMailboxMembersDoesNotReadTheRosterWhenAnOptOutIsAlreadyInHand(t *testing.T) {
	const mailbox = "member@example.test"
	for _, tc := range []struct {
		name    string
		engine  *mailboxEngine
		wantIDs []string
	}{
		{"found by its digest", &mailboxEngine{
			byDigest:      []map[string]any{mailboxRow("out", mailbox, "unsubscribed")},
			withoutDigest: fullPageOfStrangers(),
		}, []string{"out"}},
		{"found among the rows without a digest", &mailboxEngine{
			byDigest:      []map[string]any{mailboxRow("here", mailbox, "subscribed")},
			withoutDigest: append(fullPageOfStrangers()[1:], mailboxRow("bare", "MEMBER@example.test", "bounced")),
		}, []string{"here", "bare"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewStore(tc.engine).MailboxMembers(context.Background(), "aud-1", mailbox)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(ids(got), tc.wantIDs) {
				t.Errorf("members %v, want %v", ids(got), tc.wantIDs)
			}
			if pages := tc.engine.called("query audienceRosterForSend"); pages != 0 {
				t.Errorf("the roster was read in %d pages although an opt-out was already found", pages)
			}
		})
	}
}

// TestMailboxMembersWalksTheRosterAPageAtATimeAndStopsAtAnOptOut: the fallback
// reads the audience as the send does, page by page and holding only the rows that
// are the mailbox, and stops the moment one of them has opted out -- nothing after
// it can change the answer.
func TestMailboxMembersWalksTheRosterAPageAtATimeAndStopsAtAnOptOut(t *testing.T) {
	const mailbox = "member@example.test"
	for _, tc := range []struct {
		name      string
		at        map[int]map[string]any
		wantIDs   []string
		wantPages int
	}{
		{"an opt-out on the last page is found", map[int]map[string]any{8: mailboxRow("late", "Member@Example.test", "unsubscribed")}, []string{"late"}, 3},
		{"an opt-out on the first page ends the walk there", map[int]map[string]any{1: mailboxRow("early", "member@example.test", "bounced")}, []string{"early"}, 1},
		{"a subscribed member on the second page needs the whole audience", map[int]map[string]any{4: mailboxRow("here", "member@example.test", "subscribed")}, []string{"here"}, 3},
		{"nobody is found only after the last page", nil, nil, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &mailboxEngine{withoutDigest: fullPageOfStrangers(), roster: rosterOf(9, tc.at), rosterPageSize: 3}
			got, err := NewStore(engine).MailboxMembers(context.Background(), "aud-1", mailbox)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(ids(got), tc.wantIDs) {
				t.Errorf("members %v, want %v", ids(got), tc.wantIDs)
			}
			if pages := engine.called("query audienceRosterForSend"); pages != tc.wantPages {
				t.Errorf("the roster was read in %d pages, want %d", pages, tc.wantPages)
			}
		})
	}
}

// TestMailboxMembersWontGuessPastTheRosterScanLimit: an audience bigger than the
// scan will read, while some of its rows still have no digest, cannot be proved
// free of an opt-out. The answer is "cannot tell" -- never "not there", which would
// welcome a mailbox that opted out -- unless an opt-out was already found. The
// limit is two pages here, of two rows.
func TestMailboxMembersWontGuessPastTheRosterScanLimit(t *testing.T) {
	limit := mailboxRosterScanPages
	mailboxRosterScanPages = 2
	defer func() { mailboxRosterScanPages = limit }()

	const mailbox = "member@example.test"
	for _, tc := range []struct {
		name        string
		rows        int
		at          map[int]map[string]any
		undecidable bool
		wantIDs     []string
	}{
		{"nobody within the limit", 10, nil, true, nil},
		{"a subscribed member within the limit does not rule out an opt-out beyond it", 10, map[int]map[string]any{1: mailboxRow("here", mailbox, "subscribed")}, true, nil},
		{"an opt-out within the limit is decisive", 10, map[int]map[string]any{3: mailboxRow("out", mailbox, "unsubscribed")}, false, []string{"out"}},
		{"an audience the limit covers is answered", 4, nil, false, nil},
		{"an opt-out beyond the limit is exactly why it must not guess", 10, map[int]map[string]any{9: mailboxRow("far", mailbox, "unsubscribed")}, true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := &mailboxEngine{withoutDigest: fullPageOfStrangers(), roster: rosterOf(tc.rows, tc.at), rosterPageSize: 2}
			got, err := NewStore(engine).MailboxMembers(context.Background(), "aud-1", mailbox)
			if tc.undecidable {
				if !errors.Is(err, ErrMailboxUndecidable) {
					t.Fatalf("members %v, error %v; want ErrMailboxUndecidable", ids(got), err)
				}
				if pages := engine.called("query audienceRosterForSend"); pages != 2 {
					t.Errorf("the roster was read in %d pages, want it to stop at the limit after 2", pages)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(ids(got), tc.wantIDs) {
				t.Errorf("members %v, want %v", ids(got), tc.wantIDs)
			}
		})
	}
}

// TestMailboxVerdict: ANY row that opted out refuses the mailbox, whichever the
// list puts first; otherwise the first row is the member; none is a new mailbox.
func TestMailboxVerdict(t *testing.T) {
	subscribed := Recipient{ID: "s1", SubscriptionStatus: "subscribed"}
	legacy := Recipient{ID: "legacy"} // predates the state field: subscribed
	for _, tc := range []struct {
		name          string
		members       []Recipient
		wantID        string
		found, refuse bool
	}{
		{"nobody", nil, "", false, false},
		{"one subscribed", []Recipient{subscribed}, "s1", true, false},
		{"the first is the member", []Recipient{subscribed, {ID: "s2", SubscriptionStatus: "subscribed"}}, "s1", true, false},
		{"a row with no state is subscribed", []Recipient{legacy}, "legacy", true, false},
		{"the opted-out row is second", []Recipient{subscribed, {ID: "u", SubscriptionStatus: "unsubscribed"}}, "u", true, true},
		{"the opted-out row is first", []Recipient{{ID: "b", SubscriptionStatus: "bounced"}, subscribed}, "b", true, true},
		{"complained", []Recipient{subscribed, {ID: "c", SubscriptionStatus: "complained"}}, "c", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member, found, refuse := mailboxVerdict(tc.members)
			if member.ID != tc.wantID || found != tc.found || refuse != tc.refuse {
				t.Errorf("got (%q, found=%v, refuse=%v), want (%q, %v, %v)", member.ID, found, refuse, tc.wantID, tc.found, tc.refuse)
			}
		})
	}
}

// TestTheReadWindowsMirrorTheirQueries: the Go constants and the `paginate` of the
// queries they mirror are one number. digestLessWindow decides when a page is full.
func TestTheReadWindowsMirrorTheirQueries(t *testing.T) {
	queries := campaignsDSL(t, "queries.memql")
	for name, want := range map[string]int{"recipientInAudienceByDigest": digestWindow, "recipientsWithoutDigestInAudience": digestLessWindow, "audienceRosterForSend": rosterPageRows} {
		i := strings.Index(queries, "query recipient "+name+" {")
		if i < 0 {
			t.Fatalf("query %s is not in queries.memql", name)
		}
		rest := queries[i:]
		if end := strings.Index(rest, "\n}\n"); end > 0 {
			rest = rest[:end]
		}
		m := regexp.MustCompile(`paginate\s+(\d+)`).FindStringSubmatch(rest)
		if m == nil {
			t.Fatalf("query %s declares no paginate", name)
		}
		if got, _ := strconv.Atoi(m[1]); got != want {
			t.Errorf("%s paginates %d, the Go constant says %d", name, got, want)
		}
	}
}
