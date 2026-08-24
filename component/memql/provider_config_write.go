package memql

// Seeding AI provider configuration FROM THE PORTAL (epic memql#4440, D4).
//
// WHY THESE EXIST RATHER THAN THE PORTAL CALLING setGlobalSecret. That
// mutation takes `encryptedValue` and `fingerprint` -- already sealed. The
// sealing is `component/secret.Encrypt`, which reads MEMQL_MASTER_KEY, and the
// master key exists on nodes and must never exist in a browser. So a page that
// called the mutation directly could only ever write a row nothing can
// decrypt, or would need the cluster's decryption key shipped to every
// operator's laptop.
//
// The seam is therefore server-side, exactly as the Shopify connector's
// `seedSecret` is (integrations/shopify/config.go): take the plaintext, seal
// it here, write the row. What crosses the wire from the browser is the key
// itself, once, over the same TLS-terminated gRPC stream every other call
// uses -- and it is never sent back.
//
// THE NAMES ARE NOT A PARAMETER, and that is the whole safety property. An
// operator cannot mistype the row name into one the resolver never tries and
// then watch a correctly-entered key do nothing. They pick a VENDOR; this file
// knows the name.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/secret"
)

// ProviderConfigResultConcept is the canonical id of the single-row result
// these actions return. Virtual, like the other action results.
const ProviderConfigResultConcept = "v1:platform:providerConfigResult"

// providerKeyNames maps a VENDOR an operator picks to the row name the
// resolver actually tries.
//
// Exactly the names in the DSL providers' `${...}` placeholders. The
// seal-floor aliasing (memql#4338) means `MEMQL_OPENAI_API_KEY` would also
// resolve, but the portal writes the documented `MEMQL_AI_` form: two rows
// under two names that both work is how an operator ends up rotating one and
// wondering why the old key is still in use.
var providerKeyNames = map[string]string{
	"anthropic": envAnthropicAPIKey,
	"openai":    "MEMQL_AI_OPENAI_API_KEY",
}

// federationFieldNames maps the portal's field ids to the variable names the
// resolver tries. The workspace id is included here and deliberately NOT part
// of the all-or-none required set -- Anthropic needs it only when a rule spans
// more than one workspace.
var federationFieldNames = map[string]string{
	"ruleId":            envAnthropicFederationRuleID,
	"organizationId":    envAnthropicOrganizationID,
	"serviceAccountId":  envAnthropicServiceAccountID,
	"workspaceId":       envAnthropicWorkspaceID,
	"identityTokenFile": envAnthropicIdentityTokenFile,
}

// evaluateProviderKeySetExpression seals one vendor API key into a
// v1:platform:globalSecret row under the name the resolver tries.
//
// WRITE-ONLY BY CONSTRUCTION. The reply carries the row name and the
// fingerprint and nothing else; there is no read-back call anywhere in this
// epic, so a page cannot render a key even by mistake. "Is this key good" is
// answered by `providerVerify` making a live call, not by showing the operator
// what they typed.
//
// IT DOES NOT RELOAD. Seeding and applying are separate acts on purpose (D5):
// a half-typed key saved into the box would otherwise take every provider on
// every node down the moment it was saved.
func (e *MemQLEngine) evaluateProviderKeySetExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	if !rowAuthzIsClusterOwner(ctx) {
		return nil, fmt.Errorf("providerKeySet is owner-only")
	}

	vendor := strings.ToLower(strings.TrimSpace(stringArg(args, "vendor")))
	name, ok := providerKeyNames[vendor]
	if !ok {
		return nil, fmt.Errorf(
			"providerKeySet: unknown vendor %q; MemQL seeds keys for %s",
			vendor, strings.Join(sortedConfigKeys(providerKeyNames), " or "))
	}

	// TRIMMED, and refused when empty. A pasted key almost always carries a
	// trailing newline, and a credential that differs from the vendor's by one
	// byte fails with a 401 that reads exactly like a revoked key.
	value := strings.TrimSpace(stringArg(args, "apiKey"))
	if value == "" {
		return nil, fmt.Errorf("providerKeySet: the key is empty")
	}

	ciphertext, fingerprint, err := secret.Encrypt(value)
	if err != nil {
		// Never wrap the plaintext into an error: this string reaches a log.
		return nil, fmt.Errorf("providerKeySet: seal %s: %w", name, err)
	}

	call := renderProviderConfigCall("setGlobalSecret", map[string]string{
		"id":             "sec-" + strings.ToLower(strings.ReplaceAll(name, "_", "-")),
		"name":           name,
		"encryptedValue": ciphertext,
		"fingerprint":    fingerprint,
		"kind":           "vendor_api_key",
		"description":    "Seeded from the portal's AI providers page.",
		"addedBy":        actorIdOrSystem(ctx),
	})
	if _, err := e.Execute(ctx, call); err != nil {
		return nil, fmt.Errorf("providerKeySet: write %s: %w", name, err)
	}

	return singleVirtualRow(ProviderConfigResultConcept, name, map[string]any{
		"name":        name,
		"vendor":      vendor,
		"fingerprint": fingerprint,
		"applied":     false,
		"message": "Saved. It takes effect on every node when you Apply -- " +
			"seeding and applying are separate so a mistyped key cannot take the fleet down as you save it.",
	})
}

// evaluateProviderFederationSetExpression writes the Anthropic workload
// identity federation ids as v1:platform:globalVariable rows.
//
// PLAINTEXT ROWS, and correctly so: none of the five is a credential. They are
// object IDENTIFIERS naming a rule, an organization, a service account, a
// workspace and a path on disk. The credential in federation is the pod's own
// projected token, which exists only inside a pod and is never written here --
// which is the entire point of preferring this path over a key.
//
// ALL-OR-NONE IS ENFORCED BEFORE THE WRITE, not after. A partial federation
// config REFUSES BOOT (memql#4333, deliberately: zero config is legitimate,
// half config is a mistake), so accepting a partial write here would let an
// operator save from the portal and take the fleet down at its next restart --
// a failure separated from its cause by hours.
func (e *MemQLEngine) evaluateProviderFederationSetExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	if !rowAuthzIsClusterOwner(ctx) {
		return nil, fmt.Errorf("providerFederationSet is owner-only")
	}

	values := map[string]string{}
	for field, name := range federationFieldNames {
		values[name] = strings.TrimSpace(stringArg(args, field))
	}

	// The required set is the four `requiredFederationFields` names -- the
	// workspace id is outside it, matching the constructor that will read
	// these back.
	required := anthropicFederation{
		RuleID:           values[envAnthropicFederationRuleID],
		OrganizationID:   values[envAnthropicOrganizationID],
		ServiceAccountID: values[envAnthropicServiceAccountID],
		TokenFile:        values[envAnthropicIdentityTokenFile],
	}
	present, missing := required.present(), required.missing()
	if len(present) > 0 && len(missing) > 0 {
		return nil, fmt.Errorf(
			"providerFederationSet: federation is all-or-none -- %s given, %s missing. "+
				"A partial set REFUSES BOOT rather than falling back to an API key, so this is "+
				"refused here instead of at the fleet's next restart",
			strings.Join(present, ", "), strings.Join(missing, ", "))
	}

	written := make([]string, 0, len(values))
	for _, name := range sortedConfigKeys(federationFieldNames) {
		envName := federationFieldNames[name]
		value := values[envName]
		if value == "" {
			// An empty optional (the workspace id) writes nothing rather than
			// an empty row: the resolver treats "" as absent, so an empty row
			// is a row that means nothing and shadows nothing.
			continue
		}
		call := renderProviderConfigCall("setGlobalVariable", map[string]string{
			"id":          "var-" + strings.ToLower(strings.ReplaceAll(envName, "_", "-")),
			"name":        envName,
			"value":       value,
			"description": "Seeded from the portal's AI providers page.",
		})
		if _, err := e.Execute(ctx, call); err != nil {
			return nil, fmt.Errorf("providerFederationSet: write %s: %w", envName, err)
		}
		written = append(written, envName)
	}

	return singleVirtualRow(ProviderConfigResultConcept, "anthropic-federation", map[string]any{
		"name":        "anthropic-federation",
		"vendor":      "anthropic",
		"fingerprint": "",
		"applied":     false,
		"written":     written,
		"message": "Saved. It takes effect on every node when you Apply. Federation needs no key at " +
			"rest: each pod exchanges its own projected token for a short-lived bearer.",
	})
}

// renderProviderConfigCall builds `name(k: "v", ...)` with every value a
// quoted string literal.
//
// Sorted, so a failed call logs identically twice. strconv.Quote rather than
// interpolation: these values include base64 ciphertext and operator-typed
// paths, and an unescaped one is a PARSE failure at execute time -- a class
// this repo has been bitten by before (memql#4256), in a path whose unit tests
// can pass without ever parsing anything.
func renderProviderConfigCall(name string, args map[string]string) string {
	keys := sortedConfigKeys(args)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if args[k] == "" {
			continue
		}
		parts = append(parts, k+": "+strconv.Quote(args[k]))
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
}

// sortedConfigKeys is the string-valued sibling of skill_resolver.go's
// sortedKeys (which takes a set). Named apart rather than made generic: the
// two have no caller in common and a shared generic here would be a
// dependency between two unrelated files for the sake of six lines.
func sortedConfigKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// actorIdOrSystem names who seeded a row, for the audit trail on it.
func actorIdOrSystem(ctx context.Context) string {
	if id := strings.TrimSpace(rowAuthzActorUserId(ctx)); id != "" {
		return id
	}
	return "system:portal"
}
