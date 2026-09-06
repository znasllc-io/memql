package envregistry

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/frontdoor"
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
//
// The suffix comes from component/frontdoor rather than being concatenated
// here, because the SAME rule decides the Ingress hosts cmd/frontdoorhosts
// writes. Two copies of that rule would be two copies that can disagree, and a
// disagreement here is an issuer nothing is served at -- which fails as
// "sign-in is broken" with both manifests looking correct.
//
// There is no environment label in the derivation (epic memql#3943). It used to
// take one, so that a second environment in the same cluster could hyphenate
// its role hosts and nest its site hosts; MemQL ships one installation shape,
// and a second environment is a second install with its own domain.
func DomainDerivations(domain string) map[string]string {
	d := strings.TrimSpace(domain)
	if !domainPattern.MatchString(d) || numericTLD.MatchString(d) {
		return map[string]string{}
	}

	suffix := frontdoor.DomainDerivationSuffix(d)

	identity := "https://identity" + suffix
	api := "https://api" + suffix
	app := "https://app" + suffix
	// The OS shell is the platform's own site (memql#4705, and the only one
	// since epic memql#4984 retired the portal): its bundle is served at its
	// OWN hostname, not a sub-path of another node's origin, so its redirect
	// URI is this origin's own /auth/callback. The host is frontdoor.OsHost --
	// the same call the engine's SeedMaterializer makes for the site row and
	// cmd/frontdoorhosts makes for the OS's Ingress rule and certificate SAN
	// (memql#4224). Missing the CORS origin is a silent sign-in death
	// (memql#3315).
	osOrigin := "https://" + frontdoor.OsHost(d)

	// The cockpit CLIENT is loopback BY DESIGN (RFC 8252 native-client
	// redirect), so it carries no domain and is spelled out unchanged. Note
	// that the client is still called "cockpit" -- what was renamed
	// (memql#3704 / the api.<domain> rename) is the HOST identity itself is
	// reached at, not the OAuth client id.
	//
	// THE OS SHELL'S REDIRECT URI IS A FUNCTION OF WHERE ITS BUNDLE IS SERVED,
	// not of the front door's name. The page composes it from its own
	// `location.origin`, and identity matches redirect_uri by EXACT string --
	// so a URI registered for an origin the bundle is not served from is a 400
	// at /authorize with nothing in the shell's own logs. One URI, not
	// several: registering more than the one the bundle is actually served
	// from is the accept-either pattern the pre-release no-shims rule forbids.
	//
	// THE CLIENT ID IS `os`, AND IT WAS `portal` UNTIL EPIC memql#4984. The
	// portal owned this client and the OS shell borrowed it -- one client with
	// two redirect URIs -- so retiring the portal left a client named after a
	// thing that no longer exists. Renaming it is safe precisely because no
	// client hardcodes it: a bundle reads its own client id out of the edge's
	// runtime-config document, which resolves the request hostname against
	// these registered redirect URIs (component/edge/runtimeconfig.go).
	clients := fmt.Sprintf(
		`[{"clientId":"app","redirectURIs":["%s/auth/callback"]},`+
			`{"clientId":"cockpit","redirectURIs":["http://127.0.0.1/cockpit/callback","http://localhost/cockpit/callback"]},`+
			`{"clientId":"os","redirectURIs":["%s/auth/callback"]}]`,
		app, osOrigin)

	return map[string]string{
		"MEMQL_IDENTITY_BASE_URL":                 identity,
		"MEMQL_IDENTITY_VERIFIER_EXPECTED_ISSUER": identity,
		"MEMQL_IDENTITY_BOOTSTRAP_DOMAIN":         d,
		"MEMQL_DISCOVERY_GRPC_ENDPOINT":           "api" + suffix + ":443",
		"MEMQL_IDENTITY_CORS_ALLOWED_ORIGINS":     api + "," + app + "," + osOrigin,
		"MEMQL_IDENTITY_REGISTERED_CLIENTS":       clients,
		// The MCP protocol head's own front-door host (memql#3704) -- advertised
		// in OAuth discovery metadata and the 401 WWW-Authenticate hint
		// (app/transport_mcp.go). Not an identity value, but the same
		// set-if-absent derivation applies: a deployment that pins its own
		// MEMQL_MCP_PUBLIC_URL keeps it untouched.
		"MEMQL_MCP_PUBLIC_URL": "https://mcp" + suffix,
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
			logger.Warn("MEMQL_DOMAIN does not compose a usable host set; deriving nothing",
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
