package calls

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"time"

	callrules "github.com/znasllc-io/memql/component/calls"
)

const usageMeteredMode = "usage_metered_preview"
const usageQuoteLifetime = 5 * time.Minute

type usageQuotePayload struct {
	SiteID        string                      `json:"site"`
	KeyID         string                      `json:"key"`
	Configuration callrules.CallConfiguration `json:"configuration"`
	ExpiresAt     time.Time                   `json:"expires"`
}

func validQuoteRequest(r callrules.AccountRequest) bool {
	return r.AccountID != "" && len(r.AccountID) <= 128 && r.Key == "" && r.ChallengeID == "" && len(r.Credential) == 0 && r.CredentialID == "" && r.Label == "" && r.AmountCents == 0 && r.Plan == "" && r.IdempotencyKey == "" && r.ParticipantLimit >= 1 && r.ParticipantLimit <= 100 && (r.DurationMinutes == 10 || r.DurationMinutes >= 15 && r.DurationMinutes <= 1440 && r.DurationMinutes%15 == 0)
}
func (s *Service) quoteCall(ctx context.Context, site callrules.SiteRef, token string, req callrules.AccountRequest) (callrules.AccountResult, error) {
	if (req.Insights || req.Resources) && s.commerceConfig().admission(s.now()) != nil {
		return callrules.AccountResult{}, callrules.Refuse(callrules.ErrInvalid, "insights_unavailable", "Insights guidance requires a fresh funded service envelope")
	}
	h, err := s.fundingHolder(ctx, site, token, req.AccountID)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	policy, err := s.store.PolicyFor(ctx, site.OwnerUserID, site.SiteID)
	if err != nil {
		return callrules.AccountResult{}, err
	}
	c := callrules.CallConfiguration{ParticipationMode: req.ParticipationMode, ParticipantLimit: req.ParticipantLimit, DurationMinutes: req.DurationMinutes, RateVersion: callrules.PreviewRateVersion}
	if req.Insights || req.Resources {
		c.Insights, c.Resources, c.RateVersion = req.Insights, req.Resources, callrules.GuidanceRateVersion
	}
	if err := s.familyConfigurationAllowed(ctx, site, h.row.KeyID, c); err != nil {
		return callrules.AccountResult{}, err
	}
	if err := configuredPolicyAllows(policy, c); err != nil {
		return callrules.AccountResult{}, err
	}
	out, err := s.accountSnapshot(ctx, site, token)
	if err != nil {
		return out, err
	}
	expires := s.now().Add(usageQuoteLifetime).UTC()
	data, _ := json.Marshal(usageQuotePayload{site.SiteID, h.row.KeyID, c, expires})
	out.Quote = &callrules.CallQuote{CallConfiguration: c, QuoteID: callrules.SealHistoryToken(h.pepper, "call-quote", data), RateMicroDollarsPerMinute: callrules.ConfigurationRate(c, c.ParticipantLimit), MaxCostCents: callrules.PreviewReservation(c), ExpiresAt: expires, ExpiresInMs: usageQuoteLifetime.Milliseconds()}
	return out, nil
}
func configuredPolicyAllows(p callrules.Policy, c callrules.CallConfiguration) error {
	if !p.SimulatedFunding || !p.CallConfiguration {
		return accountRefusal("call_configuration_disabled")
	}
	if !callrules.ValidCallConfiguration(c) {
		return callrules.ErrInvalid
	}
	if c.ParticipantLimit > p.MaxPeople {
		return callrules.Refuse(callrules.ErrConflict, "capacity_unavailable", "the requested participant limit exceeds this site's configured capacity")
	}
	return nil
}
func (s *Service) readCallQuote(h keyHolder, token string, expiredOK bool) (callrules.CallConfiguration, error) {
	var p usageQuotePayload
	data, ok := callrules.OpenHistoryToken(h.pepper, "call-quote", token)
	if !ok || json.Unmarshal(data, &p) != nil || p.SiteID != h.site.SiteID || p.KeyID != h.row.KeyID || !callrules.ValidCallConfiguration(p.Configuration) {
		return callrules.CallConfiguration{}, callrules.Refuse(callrules.ErrInvalid, "invalid_quote", "request a new call quote")
	}
	if !expiredOK && !s.now().Before(p.ExpiresAt) {
		return callrules.CallConfiguration{}, callrules.Refuse(callrules.ErrConflict, "quote_expired", "review a fresh quote before starting or scheduling")
	}
	return p.Configuration, nil
}
func encodeUsageConfig(c callrules.CallConfiguration) string {
	b, _ := json.Marshal(c)
	return string(b)
}
func decodeUsageConfig(v string) (callrules.CallConfiguration, error) {
	var c callrules.CallConfiguration
	if json.Unmarshal([]byte(v), &c) != nil || !callrules.ValidCallConfiguration(c) {
		return c, allowanceUnavailable()
	}
	return c, nil
}
func configuredRequestDigest(pepper []byte, kind string, request any) string {
	b, _ := json.Marshal(request)
	return callrules.HistoryDigest(nil, "configured-request", kind, string(b))
}
func configuredOperation(pepper []byte, site, key, kind, nonce string) (string, error) {
	if !fundingOperationID.MatchString(nonce) {
		return "", callrules.ErrInvalid
	}
	return callrules.HistoryDigest(nil, "configured-operation", site, key, kind, nonce)[:32], nil
}
func configuredLinks(pepper []byte, operation string) callrules.CallLinks {
	token := func(purpose string) string {
		raw, _ := hex.DecodeString(callrules.HistoryDigest(pepper, purpose, operation))
		out, _ := callrules.NewToken(bytes.NewReader(raw), callrules.TokenBytes)
		return out
	}
	return callrules.CallLinks{ID: operation, GuestToken: token("configured-guest"), HostToken: token("configured-host")}
}

func (s *Service) configuredFunding(ctx context.Context, site callrules.SiteRef, keyID string, c callrules.CallConfiguration, now time.Time) (roomFunding, callrules.KeyAllowance, error) {
	if err := s.familyConfigurationAllowed(ctx, site, keyID, c); err != nil {
		return roomFunding{}, callrules.KeyAllowance{}, err
	}
	p, err := s.store.PolicyFor(ctx, site.OwnerUserID, site.SiteID)
	if err != nil {
		return roomFunding{}, callrules.KeyAllowance{}, err
	}
	if err = configuredPolicyAllows(p, c); err != nil {
		return roomFunding{}, callrules.KeyAllowance{}, err
	}
	wallet, err := s.simulatedWallet(ctx, site.OwnerUserID, site.SiteID, keyID)
	if err != nil {
		return roomFunding{}, callrules.KeyAllowance{}, err
	}
	p.UsagePreview = true // Reuse only the claimed-key/credential checks, not the old grant.
	eligible, err := s.usagePreviewEligible(ctx, site, keyID, wallet, p)
	if err != nil {
		return roomFunding{}, callrules.KeyAllowance{}, err
	}
	if !eligible {
		return roomFunding{}, callrules.KeyAllowance{}, fundingConflict("insufficient_balance")
	}
	rooms, err := s.store.openRoomsForKey(ctx, site.OwnerUserID, keyID)
	if err != nil {
		return roomFunding{}, callrules.KeyAllowance{}, err
	}
	if len(rooms) != 0 {
		return roomFunding{}, callrules.KeyAllowance{}, callrules.Refuse(callrules.ErrConflict, callrules.ReasonKeyInCall, "one funded key covers one open room")
	}
	reserve := callrules.PreviewReservation(c)
	if wallet.BalanceCents < reserve {
		return roomFunding{}, callrules.KeyAllowance{}, fundingConflict("insufficient_balance")
	}
	deadline := now.Add(time.Duration(c.DurationMinutes) * time.Minute)
	return roomFunding{FundingMode: usageMeteredMode, FundingDeadline: deadline, FundingMaxPeople: c.ParticipantLimit, UsageConfig: encodeUsageConfig(c), UsageReservedCents: reserve, UsageMeter: "[]"}, callrules.KeyAllowance{AvailableUntil: deadline}, nil
}

// Resource capacity is explicitly quoted before any publisher may consume it.
// Existing rooms and stale operator envelopes cannot acquire this new authority.
func (s *Service) resourceBudgetAllowed(room roomRow) error {
	if err := s.commerceConfig().admission(s.now()); err != nil {
		return err
	}
	c, err := decodeUsageConfig(room.UsageConfig)
	if err != nil {
		return err
	}
	if !c.Resources {
		return accountRefusal("resource_tariff_required")
	}
	return nil
}
