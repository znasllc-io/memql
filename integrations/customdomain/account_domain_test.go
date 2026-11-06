package customdomain

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// account_domain_test.go -- the account domain walk writes a failure it finds
// again only once its stored stamp is accountDomainStampRefresh old.
//
// The fake engine answers the walk's read through the real accountDomainWalk
// shape (accountWalkRows): a stored field the shape does not project would read
// as "" on a real engine, and the skip would never fire there however well it
// worked here.

type fakeWalkEngine struct {
	t     *testing.T
	rows  []map[string]any
	calls []string
}

func (f *fakeWalkEngine) Execute(_ context.Context, query string) (any, error) {
	f.calls = append(f.calls, query)
	if _, perr := langparser.ParseExpression(query); perr != nil {
		f.t.Fatalf("the engine was handed a call it cannot parse:\n  %s\n  %v", query, perr)
	}
	switch {
	case strings.HasPrefix(query, "query accountsForDomainWalk"):
		return accountWalkRows(f.t, f.rows), nil
	case strings.HasPrefix(query, "mutation recordAccountDomainCheck"):
		return []map[string]any{}, nil
	default:
		f.t.Fatalf("the walk asked for a construct this fake does not know:\n  %s", query)
		return nil, nil
	}
}

// writes is every account write the walk made.
func (f *fakeWalkEngine) writes() []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "mutation ") {
			out = append(out, c)
		}
	}
	return out
}

// txtZone answers TXT lookups from a fixture and NXDOMAIN for any other name,
// which is what a registrar's zone without the record answers.
type txtZone map[string][]string

func (z txtZone) LookupTXT(_ context.Context, name string) ([]string, error) {
	if records, ok := z[name]; ok {
		return records, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func (z txtZone) LookupCNAME(_ context.Context, name string) (string, error) { return name, nil }

func (z txtZone) LookupHost(context.Context, string) ([]string, error) { return nil, nil }

const (
	walkDomain = "acme.com"
	walkToken  = "tok-acme"
)

// walkNow is the walk's clock in every test here.
var walkNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// unpublished is a zone that does not carry the ownership record yet: the
// state production's self account sat in, failing identically on every pass.
var unpublished = txtZone{}

// published carries the ownership record.
var published = txtZone{VerifyRecordName(walkDomain): {"v=spf1 -all", walkToken}}

// walkAccount is a stored account the walk reads: its domain, its token, and
// the answer the walk last recorded.
func walkAccount(status, reason, detail, checkedAt string) map[string]any {
	return map[string]any{
		"id":                  "v1:accounts:account:acme",
		"domain":              walkDomain,
		"domainToken":         walkToken,
		"domainStatus":        status,
		"domainFailureReason": reason,
		"domainFailureDetail": detail,
		"domainLastCheckedAt": checkedAt,
		"memqlDomain":         "memql." + walkDomain,
	}
}

// runWalk runs one pass of the walk over one stored account.
func runWalk(t *testing.T, row map[string]any, zone Resolver) (*fakeWalkEngine, AccountDomainPass) {
	t.Helper()
	eng := &fakeWalkEngine{t: t, rows: []map[string]any{row}}
	i := &Integration{store: NewStore(eng), resolver: zone, now: func() time.Time { return walkNow }}
	pass, err := i.ReconcileAccountDomains(context.Background())
	if err != nil {
		t.Fatalf("ReconcileAccountDomains: %v", err)
	}
	if pass.Checked != 1 {
		t.Fatalf("the walk checked %d accounts, want the one stored: %+v", pass.Checked, pass)
	}
	return eng, pass
}

// storedFailure is the answer the walk records for an unpublished record --
// computed by the same check rather than restated, so the fixture cannot drift
// from what the walk compares.
func storedFailure() CheckResult {
	return CheckOwnership(context.Background(), unpublished, walkDomain, walkToken)
}

// TestAnUnchangedFailureIsNotWrittenAgainWithinThirtyMinutes. The production
// case: the record is not published, the walk finds the same failure every two
// minutes, and every pass used to write it again with a fresh stamp.
func TestAnUnchangedFailureIsNotWrittenAgainWithinThirtyMinutes(t *testing.T) {
	missing := storedFailure()
	if missing.OK || missing.Reason == "" || missing.Detail == "" {
		t.Fatalf("the unpublished zone does not fail the check with a reason and a detail: %+v", missing)
	}
	for _, age := range []time.Duration{0, 2 * time.Minute, accountDomainStampRefresh - time.Second} {
		t.Run(age.String(), func(t *testing.T) {
			eng, pass := runWalk(t, walkAccount(AccountDomainVerifying, missing.Reason, missing.Detail, stampTime(walkNow.Add(-age))), unpublished)
			if w := eng.writes(); len(w) != 0 {
				t.Fatalf("a failure the row already records, checked %s ago, was written again:\n  %s", age, strings.Join(w, "\n  "))
			}
			if pass.Unchanged != 1 {
				t.Errorf("unchanged = %d, want the skipped account counted", pass.Unchanged)
			}
		})
	}
}

// TestAnUnchangedFailureIsWrittenAgainOnceItsStampIsStale. The rail shows
// "Last looked <moment>" from the stamp, so it must not fall more than thirty
// minutes behind the checks the walk keeps making -- and a stamp that cannot be
// judged is never a reason to skip.
func TestAnUnchangedFailureIsWrittenAgainOnceItsStampIsStale(t *testing.T) {
	missing := storedFailure()
	for name, checkedAt := range map[string]string{
		"exactly thirty minutes old": stampTime(walkNow.Add(-accountDomainStampRefresh)),
		"three hours old":            stampTime(walkNow.Add(-3 * time.Hour)),
		"never stamped":              "",
		"unreadable":                 "last tuesday",
		"in the future":              stampTime(walkNow.Add(time.Hour)),
	} {
		t.Run(name, func(t *testing.T) {
			eng, pass := runWalk(t, walkAccount(AccountDomainVerifying, missing.Reason, missing.Detail, checkedAt), unpublished)
			w := eng.writes()
			if len(w) != 1 {
				t.Fatalf("want one write refreshing the stamp, got %d:\n  %s", len(w), strings.Join(w, "\n  "))
			}
			for _, want := range []string{
				`domainLastCheckedAt: ` + langparser.QuoteString(stampTime(walkNow)),
				`domainFailureReason: ` + langparser.QuoteString(missing.Reason),
				`domainFailureDetail: ` + langparser.QuoteString(missing.Detail),
				`domainStatus: ` + langparser.QuoteString(AccountDomainVerifying),
			} {
				if !strings.Contains(w[0], want) {
					t.Errorf("the refresh lacks %s:\n  %s", want, w[0])
				}
			}
			if pass.Unchanged != 0 {
				t.Errorf("unchanged = %d for an account that was written", pass.Unchanged)
			}
		})
	}
}

// TestAChangedFailureIsWrittenAtOnce. Somebody who has just pasted a record
// with the wrong value is watching the rail: the new detail -- what the lookup
// found -- must reach the row on the pass that finds it, however fresh the
// stamp.
func TestAChangedFailureIsWrittenAtOnce(t *testing.T) {
	wrongValue := txtZone{VerifyRecordName(walkDomain): {"tok-someone-else"}}
	found := CheckOwnership(context.Background(), wrongValue, walkDomain, walkToken)
	missing := storedFailure()
	fresh := stampTime(walkNow.Add(-2 * time.Minute))
	for name, tc := range map[string]struct {
		row  map[string]any
		zone Resolver
		want CheckResult
	}{
		"the detail changed": {walkAccount(AccountDomainVerifying, missing.Reason, missing.Detail, fresh), wrongValue, found},
		"the reason changed": {walkAccount(AccountDomainVerifying, "dns_lookup_failed", missing.Detail, fresh), unpublished, missing},
		"the status changed": {walkAccount(AccountDomainUnverified, missing.Reason, missing.Detail, fresh), unpublished, missing},
	} {
		t.Run(name, func(t *testing.T) {
			eng, _ := runWalk(t, tc.row, tc.zone)
			w := eng.writes()
			if len(w) != 1 {
				t.Fatalf("want the changed answer written at once, got %d writes:\n  %s", len(w), strings.Join(w, "\n  "))
			}
			for _, want := range []string{
				`domainFailureDetail: ` + langparser.QuoteString(tc.want.Detail),
				`domainFailureReason: ` + langparser.QuoteString(tc.want.Reason),
				`domainLastCheckedAt: ` + langparser.QuoteString(stampTime(walkNow)),
			} {
				if !strings.Contains(w[0], want) {
					t.Errorf("the write lacks %s:\n  %s", want, w[0])
				}
			}
		})
	}
}

// TestAPassIsWrittenAtOnce. Proof of ownership is the answer everything after
// it waits on -- joining and the reserved name -- so a fresh failure on the row
// never holds it back.
func TestAPassIsWrittenAtOnce(t *testing.T) {
	missing := storedFailure()
	eng, pass := runWalk(t, walkAccount(AccountDomainVerifying, missing.Reason, missing.Detail, stampTime(walkNow.Add(-time.Minute))), published)
	w := eng.writes()
	if len(w) != 1 {
		t.Fatalf("want the pass written at once, got %d writes:\n  %s", len(w), strings.Join(w, "\n  "))
	}
	for _, want := range []string{
		`domainStatus: ` + langparser.QuoteString(AccountDomainVerified),
		`domainVerifiedAt: ` + langparser.QuoteString(stampTime(walkNow)),
		`domainFailureReason: ""`,
		`domainFailureDetail: ""`,
		`memqlReservedAt: ` + langparser.QuoteString(stampTime(walkNow)),
	} {
		if !strings.Contains(w[0], want) {
			t.Errorf("the pass lacks %s:\n  %s", want, w[0])
		}
	}
	if pass.Verified != 1 || pass.Reserved != 1 {
		t.Errorf("pass = %+v, want one verified and one reserved", pass)
	}
}
