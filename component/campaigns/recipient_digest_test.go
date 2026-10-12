package campaigns

import (
	"context"
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

// TestRecipientDigestMigrationMatchesEmailDigest: the backfill's digest is the
// one this package looks members up by.
func TestRecipientDigestMigrationMatchesEmailDigest(t *testing.T) {
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
	// Addresses either side can use. Text that cannot be a mailbox is stamped by
	// the mutation with a digest of whatever it was given and has none in
	// EmailDigest; nothing ever looks that stamp up, so it is not compared.
	for _, addr := range []string{" Foo@Example.TEST ", "ÄBC@x.test", "person@example.test", "Person+News@Example.Test", "\u00a0nbsp@example.test\u00a0"} {
		v, err := memql.EvalExpr(context.Background(), expr, memql.MapScope{"args": map[string]any{"email": addr}}, memql.EvalOptions{})
		if err != nil {
			t.Fatalf("evaluating the stamp for %q: %v", addr, err)
		}
		if got, want := v, EmailDigest(addr); got != want || want == "" {
			t.Errorf("addRecipient stamps %v for %q, EmailDigest says %q", got, addr, want)
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
}

func (e *mailboxEngine) Execute(_ context.Context, q string) (any, error) {
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
		rows = e.roster
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
	for name, want := range map[string]int{"recipientInAudienceByDigest": digestWindow, "recipientsWithoutDigestInAudience": digestLessWindow} {
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
