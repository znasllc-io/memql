package genesis

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
)

// MEMQL_DOMAIN is the ONE input from which every domain-shaped env var is
// derived. A deployment states its domain once; identity's base URL, the issuer
// every node verifies against, the discovery endpoint, the CORS origins, the
// OAuth redirect URIs, and the MCP protocol head's public URL all follow from
// it.
//
// WHY DERIVE RATHER THAN CONFIGURE EACH. Those six values are one fact spelled
// six ways, and the local overlay used to spell it six times plus twice more in
// its Ingresses. Missing one is not a visible failure: memql#3315 was a single
// forgotten CORS origin, and it presented as sign-in dying with an empty
// identity log. One input with a tested derivation cannot drift against itself.
//
// SET-IF-ABSENT, ALWAYS. An explicitly configured value is a statement of
// intent and wins. That is what lets staging and prod -- which set every one of
// these explicitly -- carry MEMQL_DOMAIN or not, and be entirely unaffected
// either way.
//
// Call ApplyDomainDerivations AFTER ApplyLegacyEnvAliases (so a legacy spelling
// is already bridged onto its new name) and BEFORE any component reads its
// config.
//
// Refs: memql#3593 memql#3590 memql#3315 memql#3704

// domainPattern is what a domain may look like: two or more lowercase labels,
// no scheme, no port, no wildcard, no trailing dot. Deliberately strict --
// deriving an issuer from garbage produces a cluster that boots and then cannot
// be signed into, which is a worse failure than refusing to derive.
var domainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// numericTLD matches a final label made entirely of digits. `127.0.0.1` is four
// perfectly good labels and passes domainPattern, but an address is not a
// domain: deriving `https://identity.127.0.0.1` from it produces an issuer that
// resolves nowhere. No real TLD is all-numeric, so this is the whole test.
var numericTLD = regexp.MustCompile(`\.[0-9]+$`)

// DomainDerivations returns the env values a domain implies. An empty map means
// the domain was empty or malformed; callers derive nothing rather than
// deriving something wrong.
func DomainDerivations(domain string) map[string]string {
	d := strings.TrimSpace(domain)
	if !domainPattern.MatchString(d) || numericTLD.MatchString(d) {
		return map[string]string{}
	}

	identity := "https://identity." + d
	api := "https://api." + d
	app := "https://app." + d

	// The cockpit CLIENT is loopback BY DESIGN (RFC 8252 native-client
	// redirect), so it carries no domain and is spelled out unchanged. Note
	// that the client is still called "cockpit" -- what was renamed is the
	// HOST it dials, not the OAuth client id.
	//
	// THE PORTAL'S REDIRECT URI IS A FUNCTION OF WHERE ITS BUNDLE IS SERVED,
	// not of the front door's name. The page composes it as
	// `location.origin + import.meta.env.BASE_URL + "auth/callback"`
	// (clients/portal/src/auth/AuthProvider.tsx defaultRedirectUri, via
	// portalRedirectPath), and identity matches redirect_uri by EXACT string --
	// so a URI registered for an origin the bundle is not served from is a 400
	// at /authorize with nothing in the portal's own logs. The bundle is served
	// at /portal/ on the bff, so `api.<d>/portal/auth/callback` is the value
	// until memql#3711 gives the portal its own origin, and it moves then.
	// The `portal.<d>` CORS origin below is deliberate and separate: memql#3714
	// needs it, and an unused allowed origin is inert.
	clients := fmt.Sprintf(
		`[{"clientId":"app","redirectURIs":["%s/auth/callback"]},`+
			`{"clientId":"cockpit","redirectURIs":["http://127.0.0.1/cockpit/callback","http://localhost/cockpit/callback"]},`+
			`{"clientId":"portal","redirectURIs":["%s/portal/auth/callback"]}]`,
		app, api)

	return map[string]string{
		"MEMQL_IDENTITY_BASE_URL":                 identity,
		"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER": identity,
		"MEMQL_IDENTITY_BOOTSTRAP_DOMAIN":         d,
		"MEMQL_DISCOVERY_GRPC_ENDPOINT":           "api." + d + ":443",
		"MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS":     api + "," + app + ",https://portal." + d,
		"MEMQL_IDENTITY_REGISTERED_CLIENTS":       clients,
		// The MCP protocol head's own front-door host (memql#3704) -- advertised
		// in OAuth discovery metadata and the 401 WWW-Authenticate hint
		// (app/transport_mcp.go). Not an identity value, but the same
		// set-if-absent derivation applies: a deployment that pins its own
		// MEMQL_MCP_PUBLIC_URL keeps it untouched.
		"MEMQL_MCP_PUBLIC_URL": "https://mcp." + d,
	}
}

// ApplyDomainDerivations paints the derived values onto the process
// environment, set-if-absent. Idempotent: a second call finds every name
// populated and does nothing.
func ApplyDomainDerivations(logger *slog.Logger) {
	domain := strings.TrimSpace(os.Getenv("MEMQL_DOMAIN"))
	if domain == "" {
		return
	}

	derived := DomainDerivations(domain)
	if len(derived) == 0 {
		if logger != nil {
			logger.Warn("MEMQL_DOMAIN is not a usable domain; deriving nothing",
				"domain", domain)
		}
		return
	}

	// Deterministic order so the log line is stable run-to-run.
	names := make([]string, 0, len(derived))
	for name := range derived {
		names = append(names, name)
	}
	sort.Strings(names)

	filled := make([]string, 0, len(names))
	for _, name := range names {
		if os.Getenv(name) != "" {
			continue // explicit wins
		}
		if err := os.Setenv(name, derived[name]); err != nil {
			if logger != nil {
				logger.Warn("failed to apply domain derivation", "name", name, "err", err)
			}
			continue
		}
		filled = append(filled, name)
	}

	if logger != nil && len(filled) > 0 {
		logger.Info("domain derivations applied", "domain", domain, "vars", filled)
	}
}
