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
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// ProviderConfigResultConcept is the canonical id of the single-row result
// these actions return. Virtual, like the other action results.
const ProviderConfigResultConcept = "v1:platform:providerConfigResult"

// providersConfigAuthorized gates the provider-configuration builtins at
// owner-or-developer (epic memql#5088, design D7).
//
// A SET, NEVER A RANK FLOOR, and the difference is not cosmetic here. This
// repo's role ladder puts developer (300) ABOVE admin (200), so a
// `@requiresRank("admin")` floor would admit developers AND admins, and an
// admin is not who this is for. What the owner asked for is narrower and is
// stated as itself: a developer helps an owner through setup, so a developer
// may put a federation id in front of the engine and apply it. Every other
// owner-only gate in the cluster is untouched.
//
// It is the Integrations gate (C9 of the integration-config record), read
// through the same shape as integrations/email's configureAuthorized so the
// two cannot drift into two different meanings of "owner or developer".
func providersConfigAuthorized(ctx context.Context) bool {
	if rowAuthzIsClusterOwner(ctx) {
		return true
	}
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		return false
	}
	return ac.Role == auth.RoleOwner || ac.Role == auth.RoleDeveloper
}

// federationFieldNames maps a vendor's form field ids to the variable names
// the resolver tries.
//
// THE TOKEN-FILE PATH IS DELIBERATELY ABSENT from both vendors (epic
// memql#5088). It is base env on every engine Deployment, set beside the
// projected volume that produces the file, so a value written here as a global
// variable could only ever DISAGREE with the mount -- and would win, pointing
// the exchanger at a path nothing writes. It is part of the credential and it
// is not part of this form.
var federationFieldNames = map[string]map[string]string{
	"anthropic": {
		"ruleId":           envAnthropicFederationRuleID,
		"organizationId":   envAnthropicOrganizationID,
		"serviceAccountId": envAnthropicServiceAccountID,
		// The workspace id is here and deliberately NOT in the required set --
		// Anthropic needs it only when a rule spans more than one workspace.
		"workspaceId": envAnthropicWorkspaceID,
	},
	"openai": {
		"identityProviderId": envOpenAIIdentityProviderID,
		"serviceAccountId":   envOpenAIServiceAccountID,
	},
}

// federationRequiredFields is the all-or-none set per vendor: the ids an
// operator supplies. It mirrors each vendor's requiredFederationFields minus
// the token file, for the reason above.
var federationRequiredFields = map[string][]string{
	"anthropic": {envAnthropicFederationRuleID, envAnthropicOrganizationID, envAnthropicServiceAccountID},
	"openai":    {envOpenAIIdentityProviderID, envOpenAIServiceAccountID},
}

// federationVendors is the closed set, sorted, for error messages.
func federationVendors() []string {
	out := make([]string, 0, len(federationFieldNames))
	for vendor := range federationFieldNames {
		out = append(out, vendor)
	}
	sort.Strings(out)
	return out
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
	if !providersConfigAuthorized(ctx) {
		return nil, fmt.Errorf("providerFederationSet is owner-or-developer")
	}

	// The vendor is REQUIRED and closed. It used to be implicit -- there was
	// one federating vendor, so the builtin wrote Anthropic's names and
	// answered "anthropic" -- and an implicit vendor is exactly the shape that
	// silently writes the wrong rows the moment a second one exists.
	vendor := strings.ToLower(strings.TrimSpace(stringArg(args, "vendor")))
	fields, ok := federationFieldNames[vendor]
	if !ok {
		return nil, fmt.Errorf(
			"providerFederationSet: unknown vendor %q; MemQL federates with %s",
			vendor, strings.Join(federationVendors(), " and "))
	}

	values := map[string]string{}
	for field, name := range fields {
		values[name] = strings.TrimSpace(stringArg(args, field))
	}

	// All-or-none over the vendor's required ids. The token-file path is not
	// among them: it is base env beside the projected volume on every engine
	// Deployment, so it is part of the credential and not part of this form.
	var present, missing []string
	for _, name := range federationRequiredFields[vendor] {
		if values[name] != "" {
			present = append(present, name)
		} else {
			missing = append(missing, name)
		}
	}
	if len(present) > 0 && len(missing) > 0 {
		return nil, fmt.Errorf(
			"providerFederationSet: %s federation is all-or-none -- %s given, %s missing. "+
				"A partial set REFUSES BOOT, so this is refused here instead of at the fleet's next restart",
			vendor, strings.Join(present, ", "), strings.Join(missing, ", "))
	}

	written := make([]string, 0, len(values))
	for _, field := range sortedConfigKeys(fields) {
		envName := fields[field]
		value := values[envName]
		if value == "" {
			// An empty optional (Anthropic's workspace id) writes nothing
			// rather than an empty row: the resolver treats "" as absent, so an
			// empty row is a row that means nothing and shadows nothing.
			continue
		}
		call := renderProviderConfigCall("setGlobalVariable", map[string]string{
			"id":          "var-" + strings.ToLower(strings.ReplaceAll(envName, "_", "-")),
			"name":        envName,
			"value":       value,
			"description": "Seeded from the OS Settings AI providers section.",
		})
		// INTERNAL ORIGIN, FOR THIS ONE WRITE. A v1:platform:globalVariable row
		// is written by a cluster owner or by server code that has authorized
		// its caller and stamps internal origin for the write
		// (platform_variable_write_guard.go). This builtin admits a DEVELOPER as
		// well as an owner (providersConfigAuthorized, already passed), so a
		// developer's ids are written as the second kind; without the stamp the
		// engine refuses them and the Settings section offers a developer a
		// save that cannot land. Inline, on the one Execute that needs it: the
		// caller's own context is untouched, their actor stays on the write so
		// `createdBy` names who saved it, and what the stamp can reach is
		// bounded above -- the row's name and id come from federationFieldNames,
		// never from the caller.
		// Regression: TestProviderFederationSetWritesItsRowsForEveryRoleItAdmits.
		if _, err := e.Execute(auth.ContextWithInternalOrigin(ctx), call); err != nil {
			return nil, fmt.Errorf("providerFederationSet: write %s: %w", envName, err)
		}
		written = append(written, envName)
	}

	name := vendor + "-federation"
	return singleVirtualRow(ProviderConfigResultConcept, name, map[string]any{
		"name":        name,
		"vendor":      vendor,
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
// Sorted, so a failed call logs identically twice.
//
// `langparser.QuoteString`, NOT `strconv.Quote`, and the difference is not
// cosmetic. Go's quoting emits `\x00`, `\a`, `\v` and `\x7f`, and the MemQL
// lexer REJECTS all four -- so a value carrying any of them renders into a
// statement that fails to PARSE at execute time. QuoteString is JSON escaping
// (`\u0000`), which the lexer decodes, and the persisted value is
// byte-identical either way.
//
// It matters because one of these values is operator-typed: the federation
// form's `value` is a rule id or a filesystem path pasted from a terminal,
// and a stray control byte in a paste is an ordinary event rather than an
// exotic one. The rest -- base64 ciphertext, a hex fingerprint, a derived env
// name -- are safe by construction, which is exactly why using the safe-looking
// function would have gone unnoticed.
//
// Verified empirically rather than assumed: both forms were driven through the
// front end's own parser, and the four escapes above are the ones that differ.
// Thanks to the memql-2e session for the pointer.
func renderProviderConfigCall(name string, args map[string]string) string {
	keys := sortedConfigKeys(args)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if args[k] == "" {
			continue
		}
		parts = append(parts, k+": "+langparser.QuoteString(args[k]))
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
