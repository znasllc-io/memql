package wholesalepack

// happened_test.go -- an entitlement's times through the pack's own writer:
// what each attempt writes, and what the export answers of the row the engine
// keeps (a write at an existing id merges into the row's current version).

import (
	"context"
	"regexp"
	"testing"
	"time"
)

// fixedWidthTime is the one form a when-it-happened field is written in:
// UTC, nine fractional digits, so the text sorts as the time.
var fixedWidthTime = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{9}Z$`)

// happenedFields are the fields of when-it-happened.
var happenedFields = []string{fieldSubmittedAt, fieldDecidedAt, fieldAttemptedAt, fieldGrantedAt, fieldRevokedAt}

// spellRow drives one application's entitlement through the pack's writer, a
// minute apart each attempt, and keeps the row as the engine would: each
// write merged into the one before.
type spellRow struct {
	t      *testing.T
	reader *fixtureReader
	now    time.Time
	stored map[string]any
	times  map[string]time.Time // each attempt's clock, by label
}

func newSpellRow(t *testing.T) *spellRow {
	registerShippedAdapters()
	return &spellRow{t: t, reader: approvedFixture(), now: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
		stored: map[string]any{}, times: map[string]time.Time{}}
}

// attempt runs one provision (or, revoking, one removal) under a caller that
// answers reply, labelled for the time it was made, and merges what it wrote.
func (s *spellRow) attempt(label string, revoking bool, reply map[string]any, transitions ...string) {
	s.t.Helper()
	s.now = s.now.Add(time.Minute)
	s.times[label] = s.now
	var log []map[string]any
	for _, tr := range transitions {
		log = append(log, map[string]any{"transition": tr})
	}
	s.reader.decisions["app-1"] = log
	caller := &fakeCaller{}
	if reply != nil {
		caller.replies = map[string]map[string]any{"shopifyTagWholesaleCustomer": reply}
	}
	writer := &fakeWriter{}
	at := s.now
	p := &Provider{reader: s.reader, caller: caller, writer: writer, now: func() time.Time { return at }}
	call := p.provisionEntitlement
	if revoking {
		call = p.revokeEntitlement
	}
	if _, err := call(internalAsMerchant(), map[string]any{"applicationId": "app-1"}, 0); err != nil {
		s.t.Fatalf("%s: %v", label, err)
	}
	args := writer.only(s.t, "writeEntitlementRow")
	for _, f := range happenedFields {
		if v, ok := args[f]; ok && !fixedWidthTime.MatchString(asString(v)) {
			s.t.Fatalf("%s wrote %s %q, not the fixed-width form", label, f, v)
		}
	}
	for k, v := range args {
		s.stored[k] = v
	}
	s.stored["id"] = EntitlementRowID("app-1")
	s.stored["createdAt"] = s.now
	row := map[string]any{}
	for k, v := range s.stored {
		row[k] = v
	}
	s.reader.ents["app-1"] = []map[string]any{row}
}

// exported is the row as the export answers it: each time, "" when absent.
func (s *spellRow) exported() (state, granted, revoked string) {
	out := happenedEntitlementRow(s.stored)
	return asString(out["state"]), asString(out[fieldGrantedAt]), asString(out[fieldRevokedAt])
}

func (s *spellRow) at(label string) string { return timeText(s.times[label]) }

var (
	tagged  = map[string]any{"status": "ok", "reference": "customer=gid://shopify/Customer/1;tag=memql-wholesale"}
	refused = map[string]any{"status": "refused", "reason": "no customer on this store has that email"}
)

// GRANTED, REVOKED, GRANTED AGAIN: the export answers the second grant's time,
// and no removal time -- the removal belongs to a spell that is over.
func TestTheExportShowsTheSecondGrantAndNoRemovalAfterAGrantAgain(t *testing.T) {
	s := newSpellRow(t)
	s.attempt("grant", false, tagged, TransitionApprove)
	s.attempt("revoke", true, nil, TransitionRevoke, TransitionApprove)
	if state, granted, revoked := s.exported(); state != EntitlementRevoked || granted != "" || revoked != s.at("revoke") {
		t.Fatalf("revoked: the export answers state %s, grantedAt %q, revokedAt %q; want revoked, none, %s",
			state, granted, revoked, s.at("revoke"))
	}
	s.attempt("grant again", false, tagged, TransitionApprove, TransitionRevoke, TransitionApprove)
	if state, granted, revoked := s.exported(); state != EntitlementGranted || granted != s.at("grant again") || revoked != "" {
		t.Fatalf("granted again: the export answers state %s, grantedAt %q, revokedAt %q; want granted, %s, none",
			state, granted, revoked, s.at("grant again"))
	}
}

// GRANTED, A FAILED ATTEMPT, GRANTED AGAIN: the failure ends the granted
// period, so the export answers no grant while it stands, and the last grant's
// time after.
func TestTheExportStartsANewGrantedPeriodAfterAFailedAttempt(t *testing.T) {
	s := newSpellRow(t)
	s.attempt("grant", false, tagged, TransitionApprove)
	s.attempt("failed", false, refused, TransitionApprove)
	if state, granted, _ := s.exported(); state != EntitlementFailed || granted != "" {
		t.Fatalf("failed: the export answers state %s, grantedAt %q; want failed and no grant", state, granted)
	}
	s.attempt("grant again", false, tagged, TransitionApprove)
	if state, granted, _ := s.exported(); state != EntitlementGranted || granted != s.at("grant again") {
		t.Fatalf("granted again: the export answers state %s, grantedAt %q; want granted at %s",
			state, granted, s.at("grant again"))
	}
}

// A RE-PROVISION OVER A GRANT keeps the period's start: "since" does not move.
func TestARegrantOverAGrantKeepsTheStartOfThePeriod(t *testing.T) {
	s := newSpellRow(t)
	s.attempt("grant", false, tagged, TransitionApprove)
	s.attempt("regrant", false, tagged, TransitionApprove)
	if _, granted, _ := s.exported(); granted != s.at("grant") {
		t.Fatalf("after a re-provision over a grant the export answers grantedAt %q, want %s", granted, s.at("grant"))
	}
}

// THE PACK'S WRITERS OF A DECISION AND OF A CARRIED TIME write the fixed-width
// form too (the entitlement's writer is held to it above, the rest in the
// database lane).
func TestADecisionAndACarriedTimeAreWrittenFixedWidth(t *testing.T) {
	writer := &fakeWriter{}
	p := &Provider{writer: writer, now: func() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 100_000_000, time.UTC) }}
	if err := p.appendDecision(context.Background(), map[string]any{"id": "app-1", "storeId": "store-live"}, TransitionApprove, ""); err != nil {
		t.Fatalf("appendDecision: %v", err)
	}
	if got := asString(writer.only(t, "appendApplicationDecision")[fieldDecidedAt]); !fixedWidthTime.MatchString(got) {
		t.Fatalf("decidedAt %q is not the fixed-width form", got)
	}
	written := time.Date(2026, 3, 4, 5, 6, 7, 120_000_000, time.UTC)
	for concept, row := range map[string]map[string]any{
		applicationConcept: {"createdAt": written},
		decisionConcept:    {"createdAt": written},
		entitlementConcept: {"createdAt": written, "state": EntitlementGranted},
	} {
		for f, v := range missingTimes(concept, row) {
			if !fixedWidthTime.MatchString(asString(v)) {
				t.Fatalf("the backfill carries %s %q into %s, not the fixed-width form", f, v, concept)
			}
		}
	}
}
