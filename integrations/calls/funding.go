package calls

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
	"github.com/znasllc-io/memql/component/memql"
)

const simulationWalletConcept = "v1:calls:simulatedWalletEntry"
const simulatedMinimumTopUpCents int64 = 1000
const simulatedMaximumBalanceCents int64 = 50000

var fundingOperationID = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

// The local simulation catalog is deliberately separate from any real payment
// provider or ledger. These are the approved preview products, not live prices.
func simulatedPlan(plan string, id string, now time.Time) (*callrules.AccountPass, int64) {
	p := &callrules.AccountPass{ID: id, Plan: plan, ActivatedAt: now, DailySeconds: 10800, MaxPeople: 8}
	switch plan {
	case "day":
		p.ExpiresAt, p.TotalSeconds = now.Add(24*time.Hour), 10800
		return p, 150
	case "month":
		p.ExpiresAt, p.TotalSeconds = now.Add(30*24*time.Hour), 72000
		return p, 800
	default:
		return nil, 0
	}
}

func validFundingRequest(r callrules.AccountRequest) bool {
	if r.Key != "" || r.ChallengeID != "" || len(r.Credential) != 0 || r.CredentialID != "" || r.Label != "" || r.AccountID == "" || len(r.AccountID) > 128 || !fundingOperationID.MatchString(r.IdempotencyKey) {
		return false
	}
	if r.Action == "simulate_topup" {
		return r.Plan == "" && r.AmountCents >= simulatedMinimumTopUpCents && r.AmountCents <= simulatedMaximumBalanceCents
	}
	return r.Action == "activate_pass" && r.AmountCents == 0 && (r.Plan == "day" || r.Plan == "month")
}

func fundingConflict(reason string) error {
	return callrules.Refuse(callrules.ErrConflict, reason, "the simulated funding operation was refused")
}

// accountFunding commits state and its receipt in ONE immutable row. The next
// entry carries the complete new balance/pass, so there is no debit/receipt gap
// to reconcile after a process crash. The account+key gates serialize all entry
// writers with admission, and the version-fenced deterministic id prevents replay.
// These rows can never be consumed as real balance or a paid/test key grant.
func (s *Service) accountFunding(ctx context.Context, site callrules.SiteRef, token string, req callrules.AccountRequest) (callrules.AccountResult, error) {
	policy, err := s.store.PolicyFor(ctx, site.OwnerUserID, site.SiteID)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	if !policy.SimulatedFunding {
		return callrules.AccountResult{}, accountRefusal("simulation_disabled")
	}
	h, err := s.fundingHolder(ctx, site, token, req.AccountID)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	release, err := s.gates(ctx, accountGate(h.row.KeyID), keyGate(h.row.KeyID))
	if err != nil {
		return callrules.AccountResult{}, err
	}
	defer release()
	fresh := memql.ContextWithFreshRead(ctx)
	policy, err = s.store.PolicyFor(fresh, site.OwnerUserID, site.SiteID)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	if !policy.SimulatedFunding {
		return callrules.AccountResult{}, accountRefusal("simulation_disabled")
	}
	h, err = s.fundingHolder(fresh, site, token, req.AccountID)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	// Scope the operation id to the authenticated key. The caller's expected id
	// is a stale-tab check, never authority to select a key or ledger.
	// These identifiers contain no secrets and must survive a later token-pepper
	// rotation. Authorization still comes only from the current scoped session.
	id, digest := callrules.SimulationOperation(site.SiteID, h.row.KeyID, req)
	existing, err := s.store.simulatedWalletEntryByID(fresh, site.OwnerUserID, id)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	if existing != nil {
		if existing.KeyID != h.row.KeyID || existing.SiteID != site.SiteID || existing.RequestDigest != digest {
			return callrules.AccountResult{}, fundingConflict("idempotency_conflict")
		}
		return s.fundingAnswer(fresh, site, token, req.IdempotencyKey, *existing, true)
	}
	previous, err := s.simulatedWallet(fresh, site.OwnerUserID, site.SiteID, h.row.KeyID)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	// Match the graph's microsecond precision so an original receipt and its
	// persisted replay have identical timestamps even on a nanosecond clock.
	now := s.now().UTC().Truncate(time.Microsecond)
	entry := simulationWalletEntry{ID: id, SiteID: site.SiteID, KeyID: h.row.KeyID, RequestDigest: digest, Version: now}
	if previous != nil {
		entry.BalanceCents, entry.Pass = previous.BalanceCents, previous.Pass
		entry.Version = memql.VersionTimeAfter(previous.Version, now)
	}
	if req.Action == "simulate_topup" {
		if entry.BalanceCents > simulatedMaximumBalanceCents-req.AmountCents {
			return callrules.AccountResult{}, fundingConflict("balance_limit")
		}
		entry.Kind, entry.CreditedCents = "topup", req.AmountCents
		entry.BalanceCents += req.AmountCents
	} else {
		if entry.Pass != nil && now.Before(entry.Pass.ExpiresAt) {
			return callrules.AccountResult{}, fundingConflict("active_pass")
		}
		open, err := s.store.openRoomsForKey(fresh, site.OwnerUserID, h.row.KeyID)
		if err != nil {
			return callrules.AccountResult{}, err
		}
		if len(open) != 0 {
			return callrules.AccountResult{}, fundingConflict("hosted_room_open")
		}
		pass, cost := simulatedPlan(req.Plan, id, now)
		if pass == nil {
			return callrules.AccountResult{}, callrules.ErrInvalid
		}
		if entry.BalanceCents < cost {
			return callrules.AccountResult{}, fundingConflict("insufficient_balance")
		}
		entry.Kind, entry.DebitedCents, entry.Plan, entry.Pass = "activation", cost, req.Plan, pass
		entry.BalanceCents -= cost
	}
	if err := s.store.createSimulatedWalletEntry(fresh, site.OwnerUserID, entry); err != nil {
		return callrules.AccountResult{}, err
	}
	return s.fundingAnswer(fresh, site, token, req.IdempotencyKey, entry, false)
}

func (s *Service) fundingHolder(ctx context.Context, site callrules.SiteRef, token, expected string) (keyHolder, error) {
	h, err := s.authenticatedKey(ctx, callrules.Session{Site: site, Token: token})
	if err != nil {
		return h, err
	}
	if h.row.KeyID != expected {
		return keyHolder{}, fundingConflict("account_changed")
	}
	key, err := s.store.keyByID(ctx, site.OwnerUserID, h.row.KeyID)
	if err != nil {
		return keyHolder{}, err
	}
	if key == nil || key.SiteID != site.SiteID || !keyActive(key.Status) {
		return keyHolder{}, accountRefusal("key_unavailable")
	}
	if key.Kind != "guest" {
		return keyHolder{}, accountRefusal("legacy_key")
	}
	if h.row.CredentialID == "" || key.UserHandle == "" {
		return keyHolder{}, accountRefusal("unclaimed_key")
	}
	return h, nil
}

func (s *Service) fundingAnswer(ctx context.Context, site callrules.SiteRef, token, operation string, entry simulationWalletEntry, replayed bool) (callrules.AccountResult, error) {
	out, err := s.accountSnapshot(ctx, site, token)
	if err != nil {
		// The receipt already committed. A failed answer is retried with the SAME
		// operation id, never inferred by the caller to mean no credit/debit.
		return out, err
	}
	out.Receipt = &callrules.FundingReceipt{ID: entry.ID, IdempotencyKey: operation, Kind: entry.Kind, Status: "completed", Simulated: true, CreditedCents: entry.CreditedCents, DebitedCents: entry.DebitedCents, BalanceCents: entry.BalanceCents, CreatedAt: entry.Version, Plan: entry.Plan}
	out.Replayed = replayed
	return out, nil
}

// fundingSnapshot never hides a still-active pass merely because its allowance
// is exhausted. Disabling simulation prevents hosting without disguising stored
// simulated funds as real money. An incomplete allowance read fails closed.
func (s *Service) fundingSnapshot(ctx context.Context, site callrules.SiteRef, out *callrules.AccountResult, key *Key) error {
	out.Account.FundingMode = "unfunded"
	if out.Account.Legacy {
		out.Account.FundingMode = "legacy"
	}
	wallet, available, reserved, err := s.usageWalletSnapshot(ctx, site.OwnerUserID, site.SiteID, key.ID)
	if err != nil {
		return err
	}
	if wallet == nil {
		if out.Capabilities.CallConfiguration && !out.Account.Legacy && !out.Account.Unclaimed {
			out.Account.HostingMode = usageMeteredMode
			zero := int64(0)
			out.Account.AvailableCents = &zero
			out.Account.ReservedCents = &zero
		}
		return nil
	}
	out.Account.FundingMode = "simulation"
	balance := wallet.BalanceCents
	out.Account.BalanceCents = &balance
	out.Account.AvailableCents, out.Account.ReservedCents = &available, &reserved
	out.Account.CanHost = false
	preview := (out.Capabilities.UsagePreview || out.Capabilities.CallConfiguration) && available > 0 && keyActive(key.Status) && key.UserHandle != "" && out.Account.CredentialCount > 0
	if preview {
		out.Account.HostingMode, out.Account.CanHost = usagePreviewMode, true
	}
	if out.Capabilities.CallConfiguration && !out.Account.Legacy && !out.Account.Unclaimed {
		out.Account.HostingMode = usageMeteredMode
	}
	if wallet.Pass == nil || !s.now().Before(wallet.Pass.ExpiresAt) {
		return nil
	}
	pass := *wallet.Pass
	allowance, err := s.passAllowance(ctx, site.OwnerUserID, site.SiteID, key.ID, pass, s.now())
	if err != nil {
		return err
	}
	pass.TotalRemainingSeconds, pass.TodayRemainingSeconds, pass.DailyResetsAt = allowance.RemainingSeconds, allowance.DailyRemainingSeconds, allowance.DailyResetsAt
	out.Account.ActivePass = &pass
	out.Account.CanHost = preview || out.Capabilities.SimulatedFunding && keyActive(key.Status) && allowance.RemainingSeconds > 0 && allowance.DailyRemainingSeconds > 0 && out.Account.CredentialCount > 0
	return nil
}

var errSimulationWallet = errors.New("calls: simulated wallet state is invalid")

func invalidSimulationWallet(field string) error {
	return fmt.Errorf("%w: %s", errSimulationWallet, field)
}
