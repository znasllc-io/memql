package memql

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// The engine half of `memql provider-auth check` (memql#4335).
//
// WHY A SUBCOMMAND AND NOT A HEALTH ENDPOINT. The question this answers --
// "is this pod authenticating to Anthropic the way I think it is, right now"
// -- is asked exactly twice in the life of a cluster: at the moment of the
// cutover, and the moment something looks wrong afterwards. Both times the
// operator is already at a shell (`kubectl exec`), and both times the answer
// must come from INSIDE a pod, because the whole premise of federation is
// that the credential is the pod's own projected token and exists nowhere
// else. A health endpoint would have to be reachable, authorized and shaped,
// and would still be answering from the same place.
//
// It forces a REAL exchange and a REAL API call on purpose. Reading config
// back proves the config parses; the failure modes that matter here -- a rule
// whose subject prefix does not match, an audience typo, a service account
// removed in the Console -- are all invisible until Anthropic answers.

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
	CredentialPath string

	// The federation ids, empty on the api-key path.
	FederationRuleID  string
	OrganizationID    string
	ServiceAccountID  string
	WorkspaceID       string
	IdentityTokenFile string

	// TokenSubject / TokenAudience come from the projected token on disk,
	// not from Anthropic: they are what the federation rule matches on, and
	// a mismatch here is the single most common denial.
	TokenSubject  string
	TokenAudience []string

	// The observed exchange. Zero on the api-key path (there is none).
	ExchangeOutcome string
	TokenExpiresAt  time.Time
	TokenExpiresIn  time.Duration

	// ModelsListed is the count returned by the live models.list call -- the
	// proof that the credential is not merely well-formed but accepted.
	ModelsListed int
}

// anthropicProviderTypes are the DSL provider types this check understands.
var anthropicProviderTypes = map[string]bool{
	"anthropic":       true,
	"anthropicchat":   true,
	"anthropicstream": true,
}

// CheckProviderAuth builds the provider the way boot does, forces one
// credential exchange, and calls models.list.
//
// providerName selects one DSL provider entry by name; empty picks the first
// available Anthropic entry in name order (deterministic, so two runs on two
// pods compare).
//
// ONE HONEST LIMITATION, stated here and in the command's output: this runs
// without the engine, so the two concept-storage tiers of auth resolution
// (v1:platform:globalSecret, globalVariable) are not consulted -- only the
// process environment, which is where a pod's credential config actually
// lives. An operator who seeded the key into concept storage instead will see
// this report say the key is absent while the running node has it. It is the
// same trade `memql env` makes, for the same reason: a check that needed a
// database would not run in the situations it exists for.
func CheckProviderAuth(ctx context.Context, logger *slog.Logger, providerName string) (ProviderAuthReport, error) {
	registry := newProviderRegistry("")
	if _, err := LoadUnifiedProviders(logger, registry); err != nil {
		return ProviderAuthReport{}, fmt.Errorf("load providers: %w", err)
	}

	entry, err := selectAnthropicEntry(registry, providerName)
	if err != nil {
		return ProviderAuthReport{}, err
	}

	report := ProviderAuthReport{
		Provider: entry.Config.Name,
		Type:     entry.Config.Type,
		Model:    entry.Config.Model,
	}

	// Rebuild the credential from the resolved auth map rather than reading
	// the registered client's, so the report describes the same decision the
	// constructor made and names it in the constructor's own words when it
	// refuses.
	fed := anthropicFederationFrom(entry.Config.Auth)
	_, path, err := anthropicCredential(entry.Config, guardedHTTPClient(nil))
	if err != nil {
		return report, err
	}
	report.CredentialPath = string(path)
	if path == credentialPathFederation {
		report.FederationRuleID = fed.RuleID
		report.OrganizationID = fed.OrganizationID
		report.ServiceAccountID = fed.ServiceAccountID
		report.WorkspaceID = fed.WorkspaceID
		report.IdentityTokenFile = fed.TokenFile
		if claims, cerr := readIdentityTokenClaims(fed.TokenFile); cerr == nil {
			report.TokenSubject, _ = claims["sub"].(string)
			report.TokenAudience = jwtAudienceValues(claims)
		}
	}

	if !entry.Available || entry.Client == nil {
		if entry.err != nil {
			return report, fmt.Errorf("provider %q did not construct: %w", entry.Config.Name, entry.err)
		}
		return report, fmt.Errorf("provider %q is registered but unavailable", entry.Config.Name)
	}

	client, err := anthropicClientOf(entry.Client)
	if err != nil {
		return report, err
	}

	// The live call. models.list is the cheapest authenticated request
	// Anthropic serves -- it spends no tokens, so a check that runs on every
	// deploy costs nothing but a round trip.
	page, err := client.Models.List(ctx, anthropic.ModelListParams{})
	if err != nil {
		if rec := LastFederationExchange(); rec != nil {
			report.ExchangeOutcome = rec.Outcome
		}
		return report, fmt.Errorf("models.list failed on the %s credential path: %w", report.CredentialPath, err)
	}
	if page != nil {
		report.ModelsListed = len(page.Data)
	}
	if rec := LastFederationExchange(); rec != nil && path == credentialPathFederation {
		report.ExchangeOutcome = rec.Outcome
		report.TokenExpiresAt = rec.ExpiresAt
		report.TokenExpiresIn = rec.ExpiresIn
	}
	return report, nil
}

// selectAnthropicEntry picks the provider entry to check.
func selectAnthropicEntry(registry *ProviderRegistry, providerName string) (*ProviderConfigEntry, error) {
	name := strings.TrimSpace(providerName)
	// "anthropic" names the VENDOR, which is how an operator says it and how
	// the flag is documented; it is also the name of the @base provider,
	// which is metadata and has no client. Treat it as "any Anthropic entry".
	if name != "" && !anthropicProviderTypes[strings.ToLower(name)] {
		entry, ok := registry.Entry(name)
		if !ok {
			return nil, fmt.Errorf("no provider named %q is declared in the DSL tree", name)
		}
		if !anthropicProviderTypes[strings.ToLower(entry.Config.Type)] {
			return nil, fmt.Errorf(
				"provider %q is type %q; provider-auth check covers Anthropic providers only "+
					"(OpenAI has no federation mechanism -- its key is verified by "+
					"scripts/install/verify-provider-key.sh)",
				name, entry.Config.Type)
		}
		return entry, nil
	}

	names := registry.Names()
	sort.Strings(names)
	var fallback *ProviderConfigEntry
	for _, n := range names {
		entry, ok := registry.Entry(n)
		if !ok || entry.Config.Base {
			continue
		}
		if !anthropicProviderTypes[strings.ToLower(entry.Config.Type)] {
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
		// Every Anthropic provider failed to construct. Returning one anyway
		// is deliberate: its construction error is the answer the operator
		// came for, and swallowing it for "none available" would hide it.
		return fallback, nil
	}
	return nil, fmt.Errorf("no Anthropic provider is declared in the DSL tree")
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
