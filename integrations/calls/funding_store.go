package calls

import (
	"context"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	callrules "github.com/znasllc-io/memql/component/calls"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// Each immutable entry is both a historical receipt and the complete resulting
// simulated state. A key's strictly increasing version stamp selects its head.
// The server-only calls below stamp their named-construct boundary, as the
// credential/history stores do; readAs/writeAs also bind the site owner's
// internal context before execution. No client-origin graph access is added.
type simulationWalletEntry struct {
	ID, SiteID, KeyID, Kind, Plan, RequestDigest string
	Version                                      time.Time
	BalanceCents, CreditedCents, DebitedCents    int64
	UsageRemainderUnits                          int64
	Pass                                         *callrules.AccountPass
}

func simulationWalletFrom(row map[string]any) (*simulationWalletEntry, error) {
	e := &simulationWalletEntry{ID: memql.BareShortId(rowString(row, "id")), SiteID: memql.BareShortId(rowString(row, "siteId")), KeyID: memql.BareShortId(rowString(row, "keyId")), Kind: rowString(row, "kind"), Plan: rowString(row, "plan"), RequestDigest: rowString(row, "requestDigest"), Version: rowTime(row, "createdAt")}
	for name, target := range map[string]*int64{"balanceCents": &e.BalanceCents, "creditedCents": &e.CreditedCents, "debitedCents": &e.DebitedCents} {
		v, ok := rowIntPresent(row, name)
		if !ok || v < 0 || int64(v) > simulatedMaximumBalanceCents {
			return nil, invalidSimulationWallet(name)
		}
		*target = int64(v)
	}
	// Older receipts carry no remainder and migrate as zero, without rebilling.
	if raw, exists := row["usageRemainderUnits"]; exists && raw != nil {
		n, ok := rowIntPresent(row, "usageRemainderUnits")
		if !ok || n < 0 || int64(n) >= usageUnitsPerCent {
			return nil, invalidSimulationWallet("usage remainder")
		}
		e.UsageRemainderUnits = int64(n)
	}
	if e.ID == "" || e.SiteID == "" || e.KeyID == "" || e.Version.IsZero() || len(e.RequestDigest) != 64 {
		return nil, invalidSimulationWallet("identity")
	}
	if e.Kind != "topup" && e.Kind != "activation" && e.Kind != "settlement" {
		return nil, invalidSimulationWallet("kind")
	}
	if e.Kind == "settlement" && (e.CreditedCents != 0 || e.Plan != "") {
		return nil, invalidSimulationWallet("settlement")
	}
	if (e.Kind == "topup" && (e.CreditedCents < simulatedMinimumTopUpCents || e.DebitedCents != 0 || e.Plan != "")) || (e.Kind == "activation" && (e.CreditedCents != 0 || e.DebitedCents == 0)) {
		return nil, invalidSimulationWallet("receipt")
	}
	if passID := memql.BareShortId(rowString(row, "passId")); passID != "" {
		p := &callrules.AccountPass{ID: passID, Plan: rowString(row, "passPlan"), ActivatedAt: rowTime(row, "passActivatedAt"), ExpiresAt: rowTime(row, "passExpiresAt"), TotalSeconds: int64(rowInt(row, "passTotalSeconds")), DailySeconds: int64(rowInt(row, "passDailySeconds")), MaxPeople: rowInt(row, "passMaxPeople")}
		// Stored simulation grants must still be the exact local catalog grant.
		want, cost := simulatedPlan(p.Plan, p.ID, p.ActivatedAt)
		if want == nil || p.ActivatedAt.IsZero() || !p.ExpiresAt.Equal(want.ExpiresAt) || p.TotalSeconds != want.TotalSeconds || p.DailySeconds != want.DailySeconds || p.MaxPeople != want.MaxPeople {
			return nil, invalidSimulationWallet("pass")
		}
		if e.Kind == "activation" && (e.Plan != p.Plan || e.DebitedCents != cost || e.ID != p.ID) {
			return nil, invalidSimulationWallet("activation")
		}
		e.Pass = p
	} else if e.Kind == "activation" {
		return nil, invalidSimulationWallet("missing pass")
	}
	return e, nil
}

func (s *Service) simulatedWallet(ctx context.Context, owner, siteID, keyID string) (*simulationWalletEntry, error) {
	call, err := langparser.RenderCall("callsSimulatedWalletLatest", map[string]any{"siteId": siteID, "keyId": keyID})
	rows, err := s.store.readAs(auth.ContextWithInternalOrigin(ctx), owner, "the simulated wallet", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	entry, err := simulationWalletFrom(rows[0])
	if err != nil {
		return nil, err
	}
	if entry.SiteID != siteID || entry.KeyID != keyID {
		return nil, invalidSimulationWallet("scope")
	}
	return entry, nil
}

func (s *Store) simulatedWalletEntryByID(ctx context.Context, owner, id string) (*simulationWalletEntry, error) {
	call, err := langparser.RenderCall("callsSimulatedWalletEntryById", map[string]any{"id": id})
	rows, err := s.readAs(auth.ContextWithInternalOrigin(ctx), owner, "the simulated funding receipt", call, err)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return simulationWalletFrom(rows[0])
}

func (s *Store) createSimulatedWalletEntry(ctx context.Context, owner string, e simulationWalletEntry) error {
	args := map[string]any{"entryId": e.ID, "siteId": e.SiteID, "keyId": e.KeyID, "kind": e.Kind, "requestDigest": e.RequestDigest, "balanceCents": e.BalanceCents, "creditedCents": e.CreditedCents, "debitedCents": e.DebitedCents, "usageRemainderUnits": e.UsageRemainderUnits, "versionTime": versionStamp(e.Version)}
	if e.Plan != "" {
		args["plan"] = e.Plan
	}
	if p := e.Pass; p != nil {
		args["passId"], args["passPlan"] = p.ID, p.Plan
		args["passActivatedAt"], args["passExpiresAt"] = versionStamp(p.ActivatedAt), versionStamp(p.ExpiresAt)
		args["passTotalSeconds"], args["passDailySeconds"], args["passMaxPeople"] = p.TotalSeconds, p.DailySeconds, p.MaxPeople
	}
	call, err := langparser.RenderCall("createCallsSimulatedWalletEntry", args)
	return s.writeAs(auth.ContextWithInternalOrigin(ctx), owner, absent(simulationWalletConcept, e.ID), "the simulated funding receipt", call, err)
}
