package calls

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

const historyLedgerLimit = 1000

func historyUsageStatus(status string) *callrules.HistoryConfiguredUsage {
	return &callrules.HistoryConfiguredUsage{Basis: "retained_hosted_call_configuration", Status: status}
}

func countHistoryFlag(value *bool, count *int64, known *bool) {
	if value == nil {
		*known = false
	} else if *value {
		*count++
	}
}

func historyPeriod(now time.Time, days int) (time.Time, time.Time) {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return today.AddDate(0, 0, 1-days), today.AddDate(0, 0, 1)
}

func (s *Store) historyLedgerEntries(ctx context.Context, owner, site, key string, first, until time.Time) ([]*simulationWalletEntry, bool, error) {
	call, err := langparser.RenderCall("callsWalletEntriesForPeriod", map[string]any{"siteId": site, "keyId": key, "since": versionStamp(first), "until": versionStamp(until)})
	if err != nil {
		return nil, false, err
	}
	owned, ok := rowsAs(auth.ContextWithInternalOrigin(ctx), owner)
	if !ok {
		return nil, false, errNoOwner
	}
	out := make([]*simulationWalletEntry, 0)
	seen := make(map[string]bool)
	cursor := ""
	cursors := make(map[string]bool)
	// Read the engine continuation, not an assumed page length: operator
	// MaxResults can cap even an explicit paginate below its DSL window.
	for range 5 {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		result, err := s.engine.Execute(memql.ContextWithCursor(memql.ContextWithRootsOnlyRead(owned), cursor), "query "+call)
		if err != nil {
			return nil, false, fmt.Errorf("calls: private posted wallet history: %w", scrubbed(err))
		}
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		executed, rows, err := accountingInventoryPage(result)
		if err != nil {
			return nil, false, err
		}

		if len(out)+len(rows) > historyLedgerLimit {
			return nil, true, nil
		}
		for _, raw := range rows {
			entry, err := simulationWalletFrom(raw)
			if err != nil {
				return nil, false, err
			}
			if entry.SiteID != site || entry.KeyID != key || entry.Version.Before(first) || !entry.Version.Before(until) || seen[entry.ID] {
				return nil, false, invalidSimulationWallet("history scope")
			}
			seen[entry.ID] = true
			out = append(out, entry)
		}
		next := ""
		if executed.GetMeta() != nil {
			next = executed.GetMeta().Cursor
		}
		if next == "" {
			if executed.GetMeta() != nil && executed.GetMeta().HasMore {
				return nil, true, nil
			}
			return out, false, nil
		}
		if cursors[next] || len(rows) == 0 {
			return nil, true, nil
		}
		cursors[next] = true
		cursor = next
	}
	return nil, true, nil
}

func sameHistoryWallet(a, b *simulationWalletEntry) bool {
	return a == nil && b == nil || a != nil && b != nil && a.ID == b.ID && a.Version.Equal(b.Version)
}

func sameHistoryCommerce(a, b *commerceState) bool {
	return a == nil && b == nil || a != nil && b != nil && a.Version.Equal(b.Version)
}

// This ledger intentionally survives deletion of private call metadata. It has
// no call identifiers or titles and does not read operational sources to recover
// deleted detail. Costs are dated when immutable wallet entries were posted,
// including a committed operation whose response was lost. Reservations and
// pending provider checkouts never enter these totals. Refund/dispute callbacks
// currently set a review hold, not a wallet reversal; do not call this net cash.
func (s *Service) historyLedger(ctx context.Context, h keyHolder, out *callrules.HistoryOverview) error {
	first, until := historyPeriod(s.now(), out.PeriodDays)
	// Keep the same reporting days if the history read crossed UTC midnight.
	if len(out.Series) == out.PeriodDays && out.PeriodDays > 0 {
		if day, err := time.Parse("2006-01-02", out.Series[0].Date); err == nil {
			first, until = day, day.AddDate(0, 0, out.PeriodDays)
		}
	}
	fresh := memql.ContextWithFreshRead(ctx)
	out.Ledger = &callrules.HistoryLedger{Basis: "posted_wallet_entries", Status: "incomplete"}
	for range 3 {
		wallet, available, reserved, err := s.usageWalletSnapshot(fresh, h.site.OwnerUserID, h.site.SiteID, h.row.KeyID)
		if err != nil {
			if callrules.ReasonOf(err) == "wallet_snapshot_changed" {
				continue
			}
			return err
		}
		commerce, err := s.store.commerceState(fresh, h.site.OwnerUserID, h.site.SiteID, h.row.KeyID)
		if err != nil {
			return err
		}
		entries, limited, err := s.store.historyLedgerEntries(fresh, h.site.OwnerUserID, h.site.SiteID, h.row.KeyID, first, until)
		if err != nil {
			return err
		}
		commerceAfter, err := s.store.commerceState(fresh, h.site.OwnerUserID, h.site.SiteID, h.row.KeyID)
		if err != nil {
			return err
		}
		after, err := s.simulatedWallet(fresh, h.site.OwnerUserID, h.site.SiteID, h.row.KeyID)
		if err != nil {
			return err
		}
		if !sameHistoryWallet(wallet, after) || !sameHistoryCommerce(commerce, commerceAfter) {
			continue
		}
		// Stable absence of a wallet is the native account's known zero balance.
		out.AvailableCents, out.ReservedCents = &available, &reserved
		review := commerce != nil && commerce.FinancialHold
		out.Ledger.FinancialReview = &review
		if limited {
			out.Ledger.Status = "limited"
			return nil
		}
		var credits, usage, passes int64
		daily := make([]int64, out.PeriodDays)
		for _, entry := range entries {
			if wallet == nil || entry.Version.After(wallet.Version) {
				return invalidSimulationWallet("history head")
			}
			credits += entry.CreditedCents
			if entry.Kind == "settlement" {
				usage += entry.DebitedCents
			}
			if entry.Kind == "activation" {
				passes += entry.DebitedCents
			}
			day := int(entry.Version.Sub(first) / (24 * time.Hour))
			daily[day] += entry.DebitedCents
		}
		spend := usage + passes
		out.SpendCents = &spend
		out.Ledger.Status = "complete"
		out.Ledger.CreditedCents, out.Ledger.UsageDebitedCents, out.Ledger.PassDebitedCents = &credits, &usage, &passes
		if len(out.Series) == 0 {
			for day := range out.PeriodDays {
				out.Series = append(out.Series, callrules.HistorySeries{Date: first.AddDate(0, 0, day).Format("2006-01-02")})
			}
		}
		for day := range daily {
			out.Series[day].CostCents = &daily[day]
		}
		return nil
	}
	return nil
}
