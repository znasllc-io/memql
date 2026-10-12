package campaigns

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/database"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// recipient_digest_test.go -- the digest every recipient row carries, so a
// signup finds an existing membership in one read instead of walking the
// roster (memql#6022, F9).
//
// One normalization is written four times: here (EmailDigest), in the database
// package (RecipientEmailDigest, which the backfill migration hashes with and
// which cannot import this package), in the email-rules package (addressDigest,
// which does not import this one either), and in the DSL (addRecipient's
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

// digestLookupEngine answers every recipientInAudienceByDigest with canned rows
// and records what it was asked.
type digestLookupEngine struct {
	mu    sync.Mutex
	calls []string
	rows  []map[string]any
}

func (e *digestLookupEngine) Execute(_ context.Context, q string) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, q)
	out := make([]any, 0, len(e.rows))
	for _, r := range e.rows {
		out = append(out, r)
	}
	return map[string]any{"output": out}, nil
}

func (e *digestLookupEngine) OrganizationCapable(context.Context, string, string, string) bool {
	return false
}

// TestRecipientInAudienceByDigestAsksOneQuestionAndTakesTheOldestAnswer: one
// keyed read, the engine's oldest-first order honoured, and no read at all for
// a digest that is not one.
func TestRecipientInAudienceByDigestAsksOneQuestionAndTakesTheOldestAnswer(t *testing.T) {
	digest := EmailDigest("member@example.test")
	engine := &digestLookupEngine{rows: []map[string]any{
		{"id": "v1:campaigns:recipient:older", "accountId": "v1:accounts:account:org-1", "email": " Member@Example.test ", "subscriptionStatus": "subscribed"},
		{"id": "v1:campaigns:recipient:newer", "email": "member@example.test", "subscriptionStatus": "unsubscribed"},
	}}
	store := NewStore(engine)

	got, found, err := store.RecipientInAudienceByDigest(context.Background(), "aud-1", digest)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got.ID != "older" || got.CanonicalID != "v1:campaigns:recipient:older" || got.AccountID != "org-1" || got.SubscriptionStatus != "subscribed" {
		t.Errorf("took %+v; the engine answers oldest first and the oldest match wins", got)
	}
	if len(engine.calls) != 1 || engine.calls[0] != `query recipientInAudienceByDigest(audienceId: "aud-1", emailDigest: "`+digest+`")` {
		t.Errorf("asked %q, want one keyed read of the audience by digest", engine.calls)
	}

	engine.calls = nil
	for _, none := range []string{"", EmailDigest("no-at-sign")} {
		if _, found, err := store.RecipientInAudienceByDigest(context.Background(), "aud-1", none); found || err != nil {
			t.Errorf("digest %q: found=%v err=%v", none, found, err)
		}
	}
	if len(engine.calls) != 0 {
		t.Errorf("a digest of nothing was sent to the engine as %q: an empty digest could match every row that has none", engine.calls)
	}

	engine.rows = nil
	if _, found, err := store.RecipientInAudienceByDigest(context.Background(), "aud-1", digest); found || err != nil {
		t.Errorf("an audience with no such member answered found=%v err=%v", found, err)
	}
}
