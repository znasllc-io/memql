package memql

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/openai/openai-go/v3"

	"github.com/znasllc-io/memql/component/metrics"
)

// The engine half of `memql provider-auth check` (memql#4335).
//
// WHY A SUBCOMMAND AND NOT A HEALTH ENDPOINT. The question this answers --
// "is this pod authenticating to a vendor the way I think it is, right now"
// -- is asked exactly twice in the life of a cluster: at the moment of the
// cutover, and the moment something looks wrong afterwards. Both times the
// operator is already at a shell (`kubectl exec`), and both times the answer
// must come from INSIDE a pod, because the whole premise of federation is
// that the credential is the pod's own projected token and exists nowhere
// else.
//
// IT REPORTS THE ENGINE'S SIGN-IN AND MAKES NONE OF ITS OWN (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// A5). It used to force a real exchange and a real API call from this second
// process. A vendor that accepts an identity token once refuses whichever of
// two signers goes second: the check either took the engine's sign-in for that
// rotation, or was itself refused and reported a working federation as broken.
// The engine in the pod signs in at start and writes what happened to a small
// status file (ai_vendor_signin.go); this command reads it. What it reads is
// still a REAL sign-in and a REAL model list, made by the process that serves
// the traffic, which is a better answer to "is this pod authenticating" than a
// second process's would be.
//
// The ids it prints are the ENGINE'S, from that record. Ids applied through
// Settings live in concept storage, which this command cannot read, and win
// over the environment in the engine; the record is how they are reported
// anyway.

// ProviderAuthReport is what `provider-auth check` prints. Every field is
// safe to show an operator: ids name objects, never credentials. The bearer
// the exchange returns appears in no field.
type ProviderAuthReport struct {
	// Provider is the DSL provider entry the check ran against.
	Provider string
	Type     string
	Model    string

	// CredentialPath is "federation" or "api-key" -- the question the
	// cutover asks.
	// Vendor is "anthropic" or "openai". With two federating vendors, a report
	// that does not say which one it describes is a report an operator can
	// read as an answer about the other.
	Vendor string

	CredentialPath string

	// The federation ids, empty on the api-key path.
	// IdentityProviderID is OpenAI's half of the id pair; FederationRuleID,
	// OrganizationID and WorkspaceID are Anthropic's. Each vendor fills only
	// its own, so an empty field means "not this vendor" rather than "unset".
	IdentityProviderID string
	FederationRuleID   string
	OrganizationID     string
	ServiceAccountID   string
	WorkspaceID        string
	IdentityTokenFile  string

	// TokenSubject / TokenAudience come from the projected token on disk,
	// not from Anthropic: they are what the federation rule matches on, and
	// a mismatch here is the single most common denial.
	TokenSubject  string
	TokenAudience []string

	// ExchangeOutcome is the outcome on the engine's record: ok, denied, error
	// or waiting for a sign-in, and misconfigured, notConfigured or notUsed for
	// a vendor the engine holds no sign-in for. Empty with nothing recorded.
	ExchangeOutcome string
	TokenExpiresAt  time.Time
	TokenExpiresIn  time.Duration

	// ModelsListed is the count the vendor returned when the ENGINE listed
	// models with the bearer of its sign-in -- the proof that the credential
	// is not merely well-formed but accepted.
	ModelsListed int

	// Recorded says the engine in this pod has recorded something about the
	// vendor. False means everything in the report describes this process's own
	// environment only.
	Recorded bool
	// SignInAt is when the recorded sign-in was attempted.
	SignInAt time.Time
	// Detail is the vendor's own reason on a refusal, or the fault when the
	// vendor never answered.
	Detail string
	// MayBeReplay marks a refusal that may only mean the identity token was
	// already presented: by the process this engine replaced (a container
	// restarted inside its pod reads the same token again), or by an attempt
	// of this engine the vendor never answered.
	MayBeReplay bool
	// NextTokenDue is when the identity token is expected to be replaced.
	NextTokenDue time.Time

	// Proof is whether the vendor's API has taken a bearer of the recorded
	// sign-in: "ok", "refused", "error", or empty while nothing has used one.
	// An accepted sign-in is a bearer; Proof is what makes it a vendor this
	// node can use. ProofAt is when it was last shown, ProofStatus and
	// ProofDetail the API's own answer when it did not take the bearer.
	Proof       string
	ProofAt     time.Time
	ProofStatus int
	ProofDetail string
	// BearerDroppedAt is when the vendor's API answered 401 for the bearer of
	// the recorded sign-in and the engine dropped it. Zero while it is in use.
	BearerDroppedAt time.Time
}

// The values ProviderAuthReport.ExchangeOutcome and Proof carry, exported for
// the command that prints the report, so a renamed value cannot half-land.
const (
	SignInOutcomeOK            = signInOutcomeOK
	SignInOutcomeMisconfigured = signInOutcomeMisconfigured
	SignInOutcomeNotConfigured = signInOutcomeNotConfigured
	SignInOutcomeNotUsed       = signInOutcomeNotUsed

	SignInProofOK      = signInProofOK
	SignInProofRefused = signInProofRefused
	SignInProofError   = signInProofError
)

// ErrSignInNotVerified is returned when the check cannot say whether the
// vendor accepts this pod's credential: the engine has recorded nothing yet, is
// waiting for the next identity token, was refused in a way a restart alone
// explains, got no answer, or holds an accepted sign-in nothing has used. None
// of those is the vendor refusing the configuration, and the installer must not
// read them as one.
var ErrSignInNotVerified = errors.New("the engine's sign-in is not verified yet")

// ErrVendorNotSetUp is returned when the engine in this pod holds no account at
// the vendor and says why: nobody set the vendor up, or this node type signs in
// to none. It is an answer, not a fault, and it is NOT "verified".
var ErrVendorNotSetUp = errors.New("the vendor is not set up on this node")

// providerAuthCheckPoll is how often the check looks for the engine's record
// while it waits.
const providerAuthCheckPoll = 200 * time.Millisecond

// anthropicProviderTypes are the DSL provider types this check understands.
var anthropicProviderTypes = map[string]bool{
	"anthropic":       true,
	"anthropicchat":   true,
	"anthropicstream": true,
}

// isOpenAIProviderType matches by PREFIX rather than by an exhaustive set,
// unlike the Anthropic map above.
//
// That asymmetry is deliberate. There are three Anthropic provider types and
// ten OpenAI ones (OpenAI, OpenAIStream, OpenAITTS, OpenAISTT, OpenAIWhisper,
// OpenAIRealtime, OpenAIAudio, OpenAIImage, OpenAIDeepResearch, ...), several
// of which are placeholders that will grow. An exhaustive list would go stale
// the way every list in this repo that is not generated goes stale, and its
// failure mode is `provider-auth check --provider openai` refusing to check a
// provider that federates perfectly well.
func isOpenAIProviderType(providerType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(providerType)), "openai")
}

// CheckProviderAuth reports how this pod authenticates to a vendor: the
// configuration it can see, and what the engine in this pod recorded. It
// contacts no vendor.
//
// providerName selects one DSL provider entry by name; empty picks the first
// Anthropic entry in name order (deterministic, so two runs on two pods
// compare).
//
// THE ENGINE'S RECORD IS THE ANSWER, AND ONLY THE RECORD. This process sees the
// ids in its environment; the engine also resolves ids applied through
// Settings, which live in the cluster's storage. So this command cannot tell by
// itself whether a vendor is set up, and it used to try: with no ids in its
// environment and nothing recorded it answered "unavailable" and exited 0,
// which the installer read as "federation verified" for a vendor with no ids at
// all. The engine now records every vendor a moment after it starts, usable or
// not (ai_vendor_signin_boot.go), and each answer here is one of its records:
//
//	nil                    the vendor accepted the sign-in AND its API has taken
//	                       a bearer of it (a model list, or a served call)
//	a plain error          refused: by the vendor, by its API, or by the engine
//	                       itself (ids it will not use)
//	ErrVendorNotSetUp      nobody set the vendor up, or this node type signs in
//	                       to none
//	ErrSignInNotVerified   cannot say yet: nothing recorded, waiting for the
//	                       next identity token, a refusal a restart alone
//	                       explains, no answer, or an accepted sign-in nothing
//	                       has used
//
// IT WAITS, until ctx ends, while the engine has said nothing yet and while an
// accepted sign-in has no proof yet: the installer runs this straight after a
// rollout, the engine signs in a moment after it starts, and its sign-in and
// its model list are one call a moment apart.
//
// ONE THING IT STATES BY ITSELF: a partial set of ids in its own environment,
// when nothing is recorded. That is a refusal it can see without the engine,
// and waiting out the timeout to say it would help nobody.
func CheckProviderAuth(ctx context.Context, logger *slog.Logger, providerName string) (ProviderAuthReport, error) {
	registry := newProviderRegistry()
	if _, err := LoadUnifiedProviders(logger, registry); err != nil {
		return ProviderAuthReport{}, fmt.Errorf("load providers: %w", err)
	}

	entry, err := selectVendorEntry(registry, providerName)
	if err != nil {
		return ProviderAuthReport{}, err
	}

	vendor := metrics.FederationVendorAnthropic
	if isOpenAIProviderType(entry.Config.Type) {
		vendor = metrics.FederationVendorOpenAI
	}

	report := ProviderAuthReport{
		Provider: entry.Config.Name,
		Type:     entry.Config.Type,
		Model:    entry.Config.Model,
		Vendor:   vendor,
	}

	// What THIS process can see: the ids in the environment, and the identity
	// token on disk. They fill the report until the engine's record replaces
	// them. envErr is the constructor's own refusal of what the environment
	// holds (a partial set, a token it cannot use).
	var (
		path   credentialPath
		envErr error
	)
	if vendor == metrics.FederationVendorOpenAI {
		fed := openaiFederationFrom(entry.Config.Auth)
		_, _, path, envErr = openaiCredential(entry.Config, guardedHTTPClient(nil))
		if envErr == nil && path == credentialPathFederation {
			report.IdentityProviderID = fed.IdentityProviderID
			report.ServiceAccountID = fed.ServiceAccountID
			report.IdentityTokenFile = fed.TokenFile
			if claims, terr := readIdentityTokenClaims(fed.TokenFile); terr == nil {
				report.TokenSubject, _ = claims["sub"].(string)
				report.TokenAudience = jwtAudienceValues(claims)
			}
		}
	} else {
		fed := anthropicFederationFrom(entry.Config.Auth)
		_, path, envErr = anthropicCredential(entry.Config, guardedHTTPClient(nil))
		if envErr == nil && path == credentialPathFederation {
			report.FederationRuleID = fed.RuleID
			report.OrganizationID = fed.OrganizationID
			report.ServiceAccountID = fed.ServiceAccountID
			report.WorkspaceID = fed.WorkspaceID
			report.IdentityTokenFile = fed.TokenFile
			if claims, terr := readIdentityTokenClaims(fed.TokenFile); terr == nil {
				report.TokenSubject, _ = claims["sub"].(string)
				report.TokenAudience = jwtAudienceValues(claims)
			}
		}
	}
	report.CredentialPath = string(path)

	if envErr != nil {
		// Stated at once only when the engine has said nothing. With a record,
		// the record decides: the engine may hold the missing value in storage.
		if _, recorded, rerr := readVendorSignInStatus(vendor); rerr == nil && !recorded {
			return report, envErr
		}
	}

	rec, recorded, err := waitForSettledRecord(ctx, vendor)
	if err != nil {
		return report, fmt.Errorf("read the engine's sign-in record: %w", err)
	}
	if !recorded {
		return report, fmt.Errorf("%w: the engine in this pod has recorded nothing about %s. It records every "+
			"vendor a moment after it starts; if the node has only just started, run the check again",
			ErrSignInNotVerified, vendor)
	}

	applySignInRecord(&report, rec)
	return report, signInVerdict(rec)
}

// signInVerdict turns the engine's record into the check's answer (the table on
// CheckProviderAuth).
func signInVerdict(rec vendorSignInRecord) error {
	switch rec.Outcome {
	case signInOutcomeOK:
		switch {
		case rec.Proof == signInProofRefused:
			return fmt.Errorf("the vendor accepted the sign-in, and its API then refused the bearer (status %d): %s",
				rec.ProofStatus, rec.ProofDetail)
		case !rec.BearerDroppedAt.IsZero():
			return fmt.Errorf("%w: the vendor accepted the sign-in, and its API later answered 401 for the bearer "+
				"(at %s), so the engine dropped it. The engine signs in again with the next identity token",
				ErrSignInNotVerified, rec.BearerDroppedAt.UTC().Format(time.RFC3339))
		case rec.Proof == signInProofOK:
			return nil
		case rec.Proof == signInProofError:
			return fmt.Errorf("%w: the vendor accepted the sign-in, and its API gave no answer when the engine "+
				"listed its models with the bearer: %s", ErrSignInNotVerified, rec.ProofDetail)
		default:
			return fmt.Errorf("%w: the vendor accepted the sign-in and nothing has used the bearer yet. The next "+
				"model call on this node, or Verify in Settings > Vendors, shows whether the vendor's API takes it",
				ErrSignInNotVerified)
		}
	case signInOutcomeWaiting:
		return fmt.Errorf("%w: the engine is waiting for the next identity token%s. The one on disk has "+
			"already been presented, and the vendor accepts a token once",
			ErrSignInNotVerified, dueClause(rec.NextTokenDue))
	case signInOutcomeDenied:
		if rec.MayBeReplay {
			return fmt.Errorf("%w: the vendor refused the sign-in (status %d: %s), but this identity token may "+
				"only have been presented already: by the process this one replaced, or by an attempt of this "+
				"one the vendor never answered. The engine presents the next token when it arrives%s",
				ErrSignInNotVerified, rec.Status, rec.Detail, dueClause(rec.NextTokenDue))
		}
		return fmt.Errorf("the vendor refused the engine's sign-in (status %d): %s", rec.Status, rec.Detail)
	case signInOutcomeMisconfigured:
		return fmt.Errorf("the engine refuses this vendor's configuration before any vendor is asked: %s", rec.Detail)
	case signInOutcomeNotConfigured, signInOutcomeNotUsed:
		return fmt.Errorf("%w: %s", ErrVendorNotSetUp, rec.Detail)
	case signInOutcomeError:
		return fmt.Errorf("%w: the engine's last sign-in got no answer from the vendor: %s",
			ErrSignInNotVerified, rec.Detail)
	default:
		return fmt.Errorf("%w: the engine recorded an outcome this command does not know (%q)",
			ErrSignInNotVerified, rec.Outcome)
	}
}

// waitForSettledRecord reads the engine's record for a vendor, polling until it
// says something the check can report or ctx ends. Two states are waited out:
// nothing recorded yet, and an accepted sign-in the vendor's API has not been
// asked about yet. When ctx ends it returns whatever is there.
func waitForSettledRecord(ctx context.Context, vendor string) (vendorSignInRecord, bool, error) {
	for {
		rec, ok, err := readVendorSignInStatus(vendor)
		if err != nil {
			return rec, ok, err
		}
		if ok && !proofPending(rec) {
			return rec, true, nil
		}
		select {
		case <-ctx.Done():
			return rec, ok, nil
		case <-time.After(providerAuthCheckPoll):
		}
	}
}

// proofPending reports an accepted sign-in whose bearer nothing has used yet.
func proofPending(rec vendorSignInRecord) bool {
	return rec.Outcome == signInOutcomeOK && rec.Proof == "" && rec.BearerDroppedAt.IsZero()
}

// applySignInRecord fills the report from the engine's record. The ids and
// the token claims are the record's, which is what the engine really used.
func applySignInRecord(report *ProviderAuthReport, rec vendorSignInRecord) {
	report.Recorded = true
	report.CredentialPath = string(credentialPathFederation)
	if rec.Outcome == signInOutcomeNotConfigured || rec.Outcome == signInOutcomeNotUsed {
		report.CredentialPath = string(credentialPathUnavailable)
	}
	report.ExchangeOutcome = rec.Outcome
	report.SignInAt = rec.At
	report.Detail = rec.Detail
	report.MayBeReplay = rec.MayBeReplay
	report.NextTokenDue = rec.NextTokenDue
	report.ModelsListed = rec.ModelsListed
	report.Proof = rec.Proof
	report.ProofAt = rec.ProofAt
	report.ProofStatus = rec.ProofStatus
	report.ProofDetail = rec.ProofDetail
	report.BearerDroppedAt = rec.BearerDroppedAt
	if !rec.ExpiresAt.IsZero() {
		report.TokenExpiresAt = rec.ExpiresAt
		report.TokenExpiresIn = time.Until(rec.ExpiresAt)
	}
	if rec.TokenSubject != "" {
		report.TokenSubject = rec.TokenSubject
		report.TokenAudience = rec.TokenAudience
	}
	if len(rec.Ids) > 0 {
		report.IdentityProviderID = rec.Ids[authKeyIdentityProviderID]
		report.FederationRuleID = rec.Ids[authKeyFederationRuleID]
		report.OrganizationID = rec.Ids[authKeyOrganizationID]
		report.WorkspaceID = rec.Ids[authKeyWorkspaceID]
		// Both vendors spell this key the same way.
		if id := rec.Ids[authKeyServiceAccountID]; id != "" {
			report.ServiceAccountID = id
		}
	}
}

// dueClause says when the next identity token is expected, or nothing.
func dueClause(due time.Time) string {
	if due.IsZero() {
		return ""
	}
	return ", due about " + due.UTC().Format(time.RFC3339)
}

// listModelsForCheck makes the one authenticated call, per vendor.
func listModelsForCheck(ctx context.Context, entry *ProviderConfigEntry, vendor string) (int, error) {
	if vendor == metrics.FederationVendorOpenAI {
		client, ok := openAIClientOf(entry.Client)
		if !ok {
			return 0, fmt.Errorf("provider %q carries no OpenAI client to check", entry.Config.Name)
		}
		page, err := client.Models.List(ctx)
		if err != nil {
			return 0, err
		}
		if page == nil {
			return 0, nil
		}
		return len(page.Data), nil
	}

	client, err := anthropicClientOf(entry.Client)
	if err != nil {
		return 0, err
	}
	page, err := client.Models.List(ctx, anthropic.ModelListParams{})
	if err != nil {
		return 0, err
	}
	if page == nil {
		return 0, nil
	}
	return len(page.Data), nil
}

// openAIClientOf reaches the SDK client inside whichever OpenAI provider shape
// the registry built.
func openAIClientOf(provider AIProvider) (*openai.Client, bool) {
	switch p := provider.(type) {
	case *openAIProvider:
		return p.client, p.client != nil
	case *openAIStreamProvider:
		return p.client, p.client != nil
	case *openAITTSProvider:
		return p.client, p.client != nil
	}
	return nil, false
}

// selectVendorEntry picks the provider entry to check.
//
// The argument is a VENDOR name ("anthropic" / "openai" -- how an operator
// says it, and how --provider is documented) or the exact name of one DSL
// provider entry. A vendor name is also the name of that vendor's @base
// provider, which is metadata and carries no client, so it is treated as "any
// entry of this vendor".
func selectVendorEntry(registry *ProviderRegistry, providerName string) (*ProviderConfigEntry, error) {
	name := strings.TrimSpace(providerName)
	lower := strings.ToLower(name)

	vendorNamed := lower == "" || anthropicProviderTypes[lower] || lower == "openai"
	if !vendorNamed {
		entry, ok := registry.Entry(name)
		if !ok {
			return nil, fmt.Errorf("no provider named %q is declared in the DSL tree", name)
		}
		if !anthropicProviderTypes[strings.ToLower(entry.Config.Type)] && !isOpenAIProviderType(entry.Config.Type) {
			return nil, fmt.Errorf(
				"provider %q is type %q; provider-auth check covers the two federating vendors, "+
					"Anthropic and OpenAI", name, entry.Config.Type)
		}
		return entry, nil
	}

	// Which vendor a bare name selects. An empty name means Anthropic, which
	// is what it meant before OpenAI federated -- changing it would silently
	// re-point every existing runbook step and CI invocation at a different
	// vendor.
	wantOpenAI := lower == "openai"
	matches := func(entry *ProviderConfigEntry) bool {
		if wantOpenAI {
			return isOpenAIProviderType(entry.Config.Type)
		}
		return anthropicProviderTypes[strings.ToLower(entry.Config.Type)]
	}

	names := registry.Names()
	sort.Strings(names)
	var fallback *ProviderConfigEntry
	for _, n := range names {
		entry, ok := registry.Entry(n)
		if !ok || entry.Config.Base {
			continue
		}
		if !matches(entry) {
			continue
		}
		if entry.Available {
			return entry, nil
		}
		if fallback == nil {
			fallback = entry
		}
	}
	if fallback != nil {
		// Every entry of this vendor failed to construct. Returning one anyway
		// is deliberate: its construction error is the answer the operator
		// came for, and swallowing it for "none available" would hide it.
		return fallback, nil
	}
	vendor := "Anthropic"
	if wantOpenAI {
		vendor = "OpenAI"
	}
	return nil, fmt.Errorf("no %s provider is declared in the DSL tree", vendor)
}

// anthropicClientOf reaches the SDK client inside whichever of the two
// Anthropic provider shapes was registered.
func anthropicClientOf(p AIProvider) (*anthropic.Client, error) {
	switch v := p.(type) {
	case *anthropicProvider:
		return &v.client, nil
	case *anthropicStreamProvider:
		return &v.client, nil
	default:
		return nil, fmt.Errorf("provider client is %T, not an Anthropic client", p)
	}
}

// readIdentityTokenClaims re-reads the projected token for the report. It is
// separate from preflightIdentityToken because the report wants the claims
// even when it would rather not fail -- by the time this runs, preflight has
// already passed.
func readIdentityTokenClaims(path string) (map[string]any, error) {
	raw, err := readFileTrimmed(path)
	if err != nil {
		return nil, err
	}
	return parseJWTClaims(raw)
}
