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

	// The observed exchange. Zero on the api-key path (there is none).
	ExchangeOutcome string
	TokenExpiresAt  time.Time
	TokenExpiresIn  time.Duration

	// ModelsListed is the count the vendor returned when the ENGINE listed
	// models with the bearer of its sign-in -- the proof that the credential
	// is not merely well-formed but accepted.
	ModelsListed int

	// Recorded says the engine in this pod has recorded a sign-in for the
	// vendor. False means everything below describes configuration only.
	Recorded bool
	// SignInAt is when the recorded sign-in was attempted.
	SignInAt time.Time
	// Detail is the vendor's own reason on a refusal, or the fault when the
	// vendor never answered.
	Detail string
	// MayBeReplay marks a refusal of an identity token issued before the
	// engine started: a container restarted inside its pod reads the same
	// token again, and a vendor that accepts a token once refuses it.
	MayBeReplay bool
	// NextTokenDue is when the identity token is expected to be replaced.
	NextTokenDue time.Time
}

// ErrSignInNotVerified is returned when the check cannot say whether the
// vendor accepts this pod's credential: the engine has recorded no sign-in
// yet, is waiting for the next identity token, was refused in a way a restart
// alone explains, or got no answer. None of those is the vendor refusing the
// configuration, and the installer must not read them as one.
var ErrSignInNotVerified = errors.New("the engine's sign-in is not verified yet")

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
// configuration it can see, and the sign-in the engine in this pod recorded.
// It contacts no vendor.
//
// providerName selects one DSL provider entry by name; empty picks the first
// Anthropic entry in name order (deterministic, so two runs on two pods
// compare).
//
// It WAITS, until ctx ends, for the engine's record when federation is
// configured in this environment and nothing is recorded yet: the installer
// runs this straight after a rollout, and the engine signs in a moment after
// it starts. With no ids in the environment and no record it does not wait:
// that is a vendor nobody set up, and the report says so.
//
// The error is nil when the recorded sign-in was accepted, wraps
// ErrSignInNotVerified when the check cannot say, and is a plain error when
// the vendor refused.
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
	// token on disk. A half-configured set is refused here in the
	// constructor's own words, exactly as boot refuses it.
	var path credentialPath
	if vendor == metrics.FederationVendorOpenAI {
		fed := openaiFederationFrom(entry.Config.Auth)
		_, _, p, cerr := openaiCredential(entry.Config, guardedHTTPClient(nil))
		if cerr != nil {
			return report, cerr
		}
		path = p
		if path == credentialPathFederation {
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
		_, p, cerr := anthropicCredential(entry.Config, guardedHTTPClient(nil))
		if cerr != nil {
			return report, cerr
		}
		path = p
		if path == credentialPathFederation {
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

	// What the ENGINE recorded. Waited for only when this environment says
	// federation is configured.
	rec, recorded, err := waitForSignInRecord(ctx, vendor, path == credentialPathFederation)
	if err != nil {
		return report, fmt.Errorf("read the engine's sign-in record: %w", err)
	}
	if !recorded {
		if path == credentialPathUnavailable {
			// UNAVAILABLE IS AN ANSWER, NOT A FAILURE. No ids here and no
			// record: nobody set this vendor up on this node, and the command's
			// job is to say so plainly rather than to error out on a provider
			// that is behaving exactly as designed.
			return report, nil
		}
		return report, fmt.Errorf("%w: the engine in this pod has recorded no %s sign-in. It signs in once at "+
			"start; if the node has only just started, run the check again", ErrSignInNotVerified, vendor)
	}

	applySignInRecord(&report, rec)
	switch rec.Outcome {
	case signInOutcomeOK:
		return report, nil
	case signInOutcomeWaiting:
		return report, fmt.Errorf("%w: the engine is waiting for the next identity token%s. The one on disk has "+
			"already been presented, and the vendor accepts a token once",
			ErrSignInNotVerified, dueClause(rec.NextTokenDue))
	case signInOutcomeDenied:
		if rec.MayBeReplay {
			return report, fmt.Errorf("%w: the vendor refused the sign-in (status %d: %s), but the identity token "+
				"was issued before this engine process started, so it may only have been used already by the "+
				"process this one replaced. The engine signs in again with the next token%s",
				ErrSignInNotVerified, rec.Status, rec.Detail, dueClause(rec.NextTokenDue))
		}
		return report, fmt.Errorf("the vendor refused the engine's sign-in (status %d): %s", rec.Status, rec.Detail)
	default:
		return report, fmt.Errorf("%w: the engine's last sign-in got no answer from the vendor: %s",
			ErrSignInNotVerified, rec.Detail)
	}
}

// waitForSignInRecord reads the engine's record for a vendor. With wait set it
// polls until one exists or ctx ends; without, it reads once.
func waitForSignInRecord(ctx context.Context, vendor string, wait bool) (vendorSignInRecord, bool, error) {
	for {
		rec, ok, err := readVendorSignInStatus(vendor)
		if err != nil || ok || !wait {
			return rec, ok, err
		}
		select {
		case <-ctx.Done():
			return vendorSignInRecord{}, false, nil
		case <-time.After(providerAuthCheckPoll):
		}
	}
}

// applySignInRecord fills the report from the engine's record. The ids and
// the token claims are the record's, which is what the engine really used.
func applySignInRecord(report *ProviderAuthReport, rec vendorSignInRecord) {
	report.Recorded = true
	report.CredentialPath = string(credentialPathFederation)
	report.ExchangeOutcome = rec.Outcome
	report.SignInAt = rec.At
	report.Detail = rec.Detail
	report.MayBeReplay = rec.MayBeReplay
	report.NextTokenDue = rec.NextTokenDue
	report.ModelsListed = rec.ModelsListed
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
