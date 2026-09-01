package campaigns

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// rendered_calls_parse_test.go -- the gate on a defect class this package is
// structurally blind to.
//
// # Why the rest of the suite cannot catch this
//
// Every other test here drives a FAKE ENGINE that records call STRINGS and
// matches them with strings.HasPrefix. A fake engine never parses, so a
// rendered call can be syntactically broken and every test stays green: the
// prefix matches, the fake answers, the assertion passes. Production then
// fails at parse on every single call, which is a feature that ships complete
// and has never once worked.
//
// It is a documented recurring shape in this tree rather than a hypothetical.
// memql#3035 was `%q` emitting escapes the MemQL lexer refuses, reached
// through an error string a provider chose; memql#4265 was a nested object
// block that lexed into ONE identifier because its pairs carried no commas --
// lint passed, boot passed, every call failed at render. Both were invisible
// to a fake-engine suite.
//
// # What this does
//
// Drives every rendered call this package produces, through the REAL parser,
// with deliberately awkward values in every string position: quotes,
// backslashes, newlines, a NUL, non-ASCII, and -- for the fields object --
// header names with spaces, hyphens and leading digits, which is what a
// spreadsheet actually produces.
//
// It checks SYNTAX only. Whether the construct exists and its argument names
// resolve is the DSL's own conformance lane; what cannot be checked anywhere
// else is that the text this package emits is a statement at all.

// parsingEngine records every query and answers empty.
type parsingEngine struct {
	mu      sync.Mutex
	queries []string
}

func (e *parsingEngine) Execute(_ context.Context, q string) (any, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queries = append(e.queries, q)
	return map[string]any{"output": []any{}}, nil
}

func (e *parsingEngine) recorded() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, len(e.queries))
	copy(out, e.queries)
	return out
}

// awkward is the value dropped into every string position.
//
// Each character is here for a reason a real send supplies it: a provider's
// error text carries quotes and newlines, a display name carries an
// apostrophe and non-ASCII, a Windows export carries a carriage return, and
// PostgreSQL's jsonb cannot represent a NUL -- which is why truncateError
// substitutes it rather than letting the write that records a failure be the
// write that fails.
const awkward = "quote\" backslash\\ newline\n carriage\r tab\t nul\x00 unicode-é 'apostrophe'"

func TestEveryRenderedCallParses(t *testing.T) {
	engine := &parsingEngine{}
	store := NewStore(engine)
	ctx := context.Background()
	now := time.Now().UTC()
	str := func(v string) *string { return &v }
	num := func(v int) *int { return &v }
	yes := true
	when := now

	// Every write and every read this package renders. Grouped by the file
	// that owns them so a new call site has an obvious home.
	calls := []struct {
		name string
		run  func() error
	}{
		// store.go -- the send path
		{"enqueueCampaignSend", func() error {
			return store.EnqueueSend(ctx, SendJob{
				CampaignID: awkward, CampaignOwnerUserID: awkward, AudienceID: awkward,
				TemplateID: awkward, Status: "scheduled", ScheduledAt: now,
			})
		}},
		{"updateSendJob", func() error {
			return store.UpdateJob(ctx, awkward, SendJobPatch{
				Status: str(awkward), RecipientCount: num(3), SentCount: num(1),
				SkippedCount: num(1), FailedCount: num(1), LastError: str(awkward),
				StartedAt: &when, CompletedAt: &when, ThrottledUntil: &when,
				RosterCursor: str(awkward), RosterOutstanding: &yes,
			})
		}},
		{"recordCampaignDelivery", func() error {
			return store.RecordDelivery(ctx, Delivery{
				CampaignID: awkward, RecipientID: awkward, Email: awkward,
				Status: "failed", SkipReason: awkward, LastError: awkward,
				SentAt: now, Attempts: 2, NextAttemptAt: now,
			})
		}},
		{"updateCampaignProgress", func() error {
			return store.UpdateCampaignProgress(ctx, awkward, CampaignProgress{
				Status: str(awkward), RecipientCount: num(9), SentCount: num(4),
				SkippedCount: num(3), FailedCount: num(2), LastError: str(awkward),
				CompletedAt: &when,
			})
		}},
		{"scheduleCampaign", func() error { return store.ScheduleCampaign(ctx, awkward, now) }},
		{"startCampaign", func() error { return store.SetCampaignStatus(ctx, "startCampaign", awkward) }},
		{"recordSuppression", func() error {
			return store.RecordSuppression(ctx, strings.Repeat("a", 64), "manual", awkward, awkward, awkward)
		}},
		{"setRecipientSubscription", func() error {
			return store.SetRecipientSubscription(ctx, awkward, "unsubscribed", now)
		}},

		// store.go -- reads
		{"campaignById", func() error { _, _, err := store.CampaignByID(ctx, awkward); return err }},
		{"templateById", func() error { _, _, err := store.TemplateByID(ctx, awkward); return err }},
		{"sendJobById", func() error { _, _, err := store.JobByID(ctx, awkward); return err }},
		{"suppressionByDigest", func() error { _, _, err := store.SuppressionByDigest(ctx, awkward); return err }},
		{"audienceRosterForSend", func() error { _, _, err := store.RosterPage(ctx, awkward, ""); return err }},
		{"audienceRosterSize", func() error { _, err := store.RosterSize(ctx, awkward); return err }},
		{"deliveriesForRecipients", func() error {
			_, err := store.LedgerFor(ctx, awkward, []string{awkward, "v1:campaigns:recipient:x"})
			return err
		}},
		{"deliveryLedgerForCampaign", func() error { _, err := store.Ledger(ctx, awkward); return err }},
		{"recipientById", func() error { _, _, err := store.RecipientByID(ctx, awkward); return err }},

		// identity + accounts (memql#4821, #4822)
		{"senderIdentityById", func() error { _, _, err := store.SenderIdentityByID(ctx, awkward); return err }},
		{"clientAccountById", func() error { _, err := store.AccountName(ctx, awkward); return err }},

		// reputation + warmup
		{"reputationWindowsSince", func() error { _, err := store.ReputationSince(ctx, "2026-01-01"); return err }},
		{"recordReputationWindow", func() error {
			return store.RecordReputationWindow(ctx,
				reputationKey{identity: awkward, domain: awkward, day: "2026-01-01"}, awkward,
				reputationCounts{accepted: 1, hardBounce: 2, softBounce: 3, complaint: 4})
		}},
		{"warmupStateForIdentity", func() error { _, _, err := store.WarmupState(ctx, awkward); return err }},
		{"recordWarmupState", func() error {
			return store.RecordWarmupState(ctx, awkward, warmupDecision{
				Step: 1, RatePerMinute: 10, Decision: "held", Reason: awkward,
				StepEnteredAt: now, AcceptedInStep: 5,
			})
		}},

		// feedback ingest
		{"inboundRequestById", func() error { _, _, err := store.InboundRequestByID(ctx, awkward); return err }},
		{"updateInboundRequestStatus", func() error { return store.SetInboundStatus(ctx, awkward, "failed", awkward) }},

		// import + consent (memql#4822, #4820)
		{"libraryArtifactById + libraryFileById", func() error {
			_, _, err := store.LibraryFileForArtifact(ctx, awkward)
			return err
		}},
		{"addRecipient (no fields)", func() error {
			return store.AddRecipient(ctx, awkward, awkward, awkward, awkward, "import", nil)
		}},
		{"addRecipient (awkward field keys)", func() error {
			return store.AddRecipient(ctx, awkward, awkward, awkward, awkward, "import", map[string]string{
				// The header shapes a real spreadsheet produces. A bare key
				// here is a parse error, which is memql#4265's whole shape.
				"Company Name": awkward,
				"2026 spend":   "1200",
				"first-name":   "Ada",
				"plan":         "pro",
				"":             "dropped-by-the-importer-but-rendered-if-passed",
			})
		}},
		{"recordConsentGrant", func() error {
			return store.RecordConsent(ctx, ConsentGrant, ConsentRecord{
				EventID: awkward, EmailDigest: awkward, Source: "import",
				RecipientID: awkward, CampaignID: awkward, OccurredAt: now,
			})
		}},
		{"recordConsentWithdraw", func() error {
			return store.RecordConsent(ctx, ConsentWithdraw, ConsentRecord{
				EventID: awkward, EmailDigest: awkward, Source: "one_click",
				RecipientID: awkward, CampaignID: awkward, OccurredAt: now,
			})
		}},
		{"recordConsentBounce", func() error {
			return store.RecordConsent(ctx, ConsentBounce, ConsentRecord{
				EventID: awkward, EmailDigest: awkward, Source: "provider", CampaignID: awkward, OccurredAt: now,
			})
		}},
		{"recordConsentComplaint", func() error {
			return store.RecordConsent(ctx, ConsentComplaint, ConsentRecord{
				EventID: awkward, EmailDigest: awkward, Source: "provider", CampaignID: awkward, OccurredAt: now,
			})
		}},
		{"recordConsentSuppress", func() error {
			return store.RecordConsent(ctx, ConsentSuppress, ConsentRecord{
				EventID: awkward, EmailDigest: awkward, Source: "operator", Reason: awkward, OccurredAt: now,
			})
		}},

		// stats + engagement (memql#4823)
		{"campaignDeliveryCountByStatus", func() error {
			_, err := store.DeliveryCountByStatus(ctx, awkward, "skipped")
			return err
		}},
		{"campaignSkipCountByReason", func() error {
			_, err := store.SkipCountByReason(ctx, awkward, suppressedSkipReasons)
			return err
		}},
		{"campaignConsentCountByKind", func() error {
			_, err := store.ConsentCountByKind(ctx, awkward, ConsentBounce)
			return err
		}},
		{"campaignEngagementCountByKind", func() error {
			_, err := store.EngagementCountByKind(ctx, awkward, EngagementOpen)
			return err
		}},
		{"campaignEngagementRefs", func() error {
			_, _, err := store.EngagementDeliveryRefs(ctx, awkward, EngagementClick)
			return err
		}},
		{"recordEngagementEvent", func() error {
			return store.RecordEngagementEvent(ctx, EngagementEvent{
				CampaignID: awkward, DeliveryID: awkward, Kind: EngagementClick,
				URL: "https://acme.test/a?b=1&c=" + awkward, OccurredAt: now,
			})
		}},

		// event-email rules (memql#4829): the ledger row carries the rule
		// that produced it, and it must render only when there IS one.
		{"recordCampaignDelivery (rule-stamped)", func() error {
			return store.RecordDelivery(ctx, Delivery{
				CampaignID: awkward, RecipientID: awkward, Email: awkward,
				Status: "sent", SentAt: now, Attempts: 1, EmailRuleID: awkward,
			})
		}},
	}

	before := 0
	for _, c := range calls {
		if err := c.run(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		rendered := engine.recorded()
		if len(rendered) == before {
			t.Fatalf("%s rendered no call at all -- the case is checking nothing", c.name)
		}
		for _, q := range rendered[before:] {
			if _, err := langparser.ParseExpression(q); err != nil {
				t.Errorf("%s: THE REAL PARSER REFUSES THE RENDERED CALL.\n  %s\n  --> %v\n\n"+
					"Every other test in this package drives a fake engine that records call strings and "+
					"never parses one, so this failure is invisible to all of them: the prefix matches, "+
					"the fake answers, the assertions pass, and production fails at parse on every call. "+
					"That is memql#3035 (%%q escapes the lexer refuses) and memql#4265 (an object block "+
					"with no commas lexing into one identifier), both of which shipped green.",
					c.name, q, err)
			}
		}
		before = len(rendered)
	}

	if before < len(calls) {
		t.Fatalf("only %d calls were rendered for %d cases", before, len(calls))
	}
}

// TestQuoteStringIsUsedForEveryInterpolatedValue is the mechanism behind the
// test above, asserted directly.
//
// langparser.QuoteString and Go's %q DISAGREE on four control bytes that no
// natural test value contains, and the disagreement is fail-closed in the
// worst direction: %q emits escapes the MemQL lexer refuses outright, so the
// statement does not parse and the write is dropped. When the dropped write
// is the one recording a provider's failure, the failure disappears
// (memql#3035).
func TestQuoteStringIsUsedForEveryInterpolatedValue(t *testing.T) {
	engine := &parsingEngine{}
	store := NewStore(engine)
	if err := store.RecordDelivery(context.Background(), Delivery{
		CampaignID: "c1", RecipientID: "r1", Email: "a@example.test",
		Status: "failed", LastError: awkward, Attempts: 1,
	}); err != nil {
		t.Fatalf("RecordDelivery: %v", err)
	}
	rendered := engine.recorded()[0]

	// The NUL is substituted before quoting -- PostgreSQL's jsonb cannot
	// represent U+0000, so a single such byte from a remote server makes the
	// insert fail, and the write that fails is the one recording the failure.
	if strings.ContainsRune(rendered, 0) {
		t.Error("a NUL byte reached the rendered call; truncateError must substitute it")
	}
	if _, err := langparser.ParseExpression(rendered); err != nil {
		t.Fatalf("the rendered call does not parse: %v", err)
	}
	// The %q spelling of the same value, for contrast: if this ever starts
	// parsing, the two encoders have converged and the note above is stale
	// rather than wrong.
	if strings.Contains(rendered, `\x00`) {
		t.Error("the rendered call carries a \\x00 escape, which is the Go-quoting spelling the lexer refuses")
	}
}
