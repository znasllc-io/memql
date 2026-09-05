// Package githubconnect is the GitHub App protocol half of GitHub Connect
// (epic memql#4912, issue memql#4913): the six configuration values, the
// authorize URL, and the three GitHub calls the callback makes.
//
// IT HOLDS NO STATE AND TOUCHES NO ROW. The connect-state row's
// compare-and-swap lives in component/identity/store_githubconnect.go, where
// the internal-origin stamp is already allowlisted; the HTTP handler lives in
// component/identity/http. Keeping this package pure is what lets the whole
// protocol be tested against an httptest server with no engine and no
// database.
package githubconnect

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// The six environment variables. Named as constants rather than written as
// literals at each read site, because every refusal below names them back to
// the operator and a typo in a message is a typo in the repair instruction.
const (
	EnvAppID         = "MEMQL_GITHUB_APP_ID"
	EnvAppSlug       = "MEMQL_GITHUB_APP_SLUG"
	EnvClientID      = "MEMQL_GITHUB_APP_CLIENT_ID"
	EnvClientSecret  = "MEMQL_GITHUB_APP_CLIENT_SECRET"
	EnvPrivateKeyB64 = "MEMQL_GITHUB_APP_PRIVATE_KEY_B64"
	EnvWebhookSecret = "MEMQL_GITHUB_APP_WEBHOOK_SECRET"
)

// CallbackPath is where GitHub sends the browser back, and it is also the
// app's setup URL. One route serves both shapes -- see
// component/identity/http/github_callback.go for why they cannot be split.
const CallbackPath = "/auth/github/callback"

// Config is the cluster's GitHub App. The zero value means "no app", which is
// a supported install: Connect is simply absent and the Source stop offers the
// pasted-token path alone.
type Config struct {
	// AppID is the app's numeric id, as a string. Carried as text because
	// nothing here does arithmetic on it and a numeric type would invite a
	// parse failure at boot over a value that is only ever concatenated.
	// Env: MEMQL_GITHUB_APP_ID
	AppID string

	// AppSlug is the app's URL slug -- the last segment of
	// https://github.com/apps/<slug>. It exists for the INSTALLATION link,
	// which is a different page from the authorize endpoint: authorizing
	// grants this cluster the person's own authority, installing is what puts
	// the app on a repository. A person who authorized and installed nothing
	// has a grant that can reach no repository at all, so the link has to be
	// offerable.
	// Env: MEMQL_GITHUB_APP_SLUG
	AppSlug string

	// ClientID / ClientSecret are the app's OAuth credentials, used for the
	// user-token exchange at the callback.
	// Env: MEMQL_GITHUB_APP_CLIENT_ID / MEMQL_GITHUB_APP_CLIENT_SECRET
	ClientID     string
	ClientSecret string

	// PrivateKeyB64 is the app's private key, base64 of the PEM. It is not
	// read by anything in this package: it mints INSTALLATION tokens, which
	// is the engine's half of the epic. It is required here all the same,
	// because a cluster that can connect a person and cannot fetch under the
	// resulting grant is a cluster whose Connect button works exactly once
	// and then fails in the background, where nobody is watching.
	// Env: MEMQL_GITHUB_APP_PRIVATE_KEY_B64
	PrivateKeyB64 string

	// WebhookSecret is the app's webhook secret, which is also the HMAC key
	// the inbound seam verifies deliveries with
	// (MEMQL_INBOUND_SOURCE_GITHUB_SECRET). Required for the same reason the
	// private key is: without it the installation ids on a grant go stale the
	// first time somebody adds or removes a repository, and a stale list
	// turns a clear repository_not_installed back into an unexplained 404.
	// Env: MEMQL_GITHUB_APP_WEBHOOK_SECRET
	WebhookSecret string
}

// LoadFromEnv reads the six values. Every one is optional; none of them set is
// the ordinary state of a cluster that does not offer Connect.
func LoadFromEnv() Config {
	return Config{
		AppID:         strings.TrimSpace(os.Getenv(EnvAppID)),
		AppSlug:       strings.TrimSpace(os.Getenv(EnvAppSlug)),
		ClientID:      strings.TrimSpace(os.Getenv(EnvClientID)),
		ClientSecret:  strings.TrimSpace(os.Getenv(EnvClientSecret)),
		PrivateKeyB64: strings.TrimSpace(os.Getenv(EnvPrivateKeyB64)),
		WebhookSecret: strings.TrimSpace(os.Getenv(EnvWebhookSecret)),
	}
}

// fields is the set that decides the branch: all six present means Connect is
// on, none means it is absent, anything between refuses boot.
//
// The order is the order the missing-key error names them in, which is the
// order the runbook creates them in -- the app id and slug are visible on the
// app's settings page the moment it exists, the OAuth pair is generated next,
// and the private key and webhook secret are the two an operator has to
// deliberately go and mint.
func (c Config) fields() []struct {
	envName string
	value   string
} {
	return []struct {
		envName string
		value   string
	}{
		{EnvAppID, c.AppID},
		{EnvAppSlug, c.AppSlug},
		{EnvClientID, c.ClientID},
		{EnvClientSecret, c.ClientSecret},
		{EnvPrivateKeyB64, c.PrivateKeyB64},
		{EnvWebhookSecret, c.WebhookSecret},
	}
}

// Present returns the env names that carry a value; Missing returns the ones
// that do not. Both are exported because the refusal names both halves and a
// test asserts on the names rather than on prose.
func (c Config) Present() []string {
	var out []string
	for _, f := range c.fields() {
		if f.value != "" {
			out = append(out, f.envName)
		}
	}
	return out
}

func (c Config) Missing() []string {
	var out []string
	for _, f := range c.fields() {
		if f.value == "" {
			out = append(out, f.envName)
		}
	}
	return out
}

// Configured reports whether this cluster has a GitHub App. All six or none;
// a partial configuration never reaches here because Validate refuses boot.
func (c Config) Configured() bool { return len(c.Missing()) == 0 }

// Validate refuses a HALF-configured app at boot rather than at the first
// person to press Connect.
//
// The Anthropic workload-identity precedent (component/memql/ai_anthropic_federation.go),
// and the same argument the OIDC provider makes: a button that appears and
// then fails per-user is worse than no button, because the failure arrives to
// everyone EXCEPT the operator who could fix it. Naming both halves is
// deliberate -- an operator part-way through setup needs to know which of the
// six they already have, not only which they lack.
func (c Config) Validate() error {
	present, missing := c.Present(), c.Missing()
	switch {
	case len(missing) == 0:
		return nil
	case len(present) == 0:
		// The ordinary state of a cluster with no GitHub App. Connect is
		// absent, githubConnectBegin answers github_app_not_configured, and
		// the Source stop offers the pasted token alone.
		return nil
	default:
		return fmt.Errorf(
			"the GitHub App is HALF-CONFIGURED: %s set, %s missing. Set all six (or none, to leave "+
				"GitHub Connect off and offer the pasted-token path alone). A partial configuration is "+
				"refused rather than started, because the half that is missing fails where nobody is "+
				"watching -- a Connect button that mints a grant the engine cannot fetch under, or a "+
				"grant whose installation list silently goes stale. Register the redirect URI as "+
				"https://identity.<domain>%s. Runbook: docs/public/operate/env-vars.md",
			strings.Join(present, ", "), strings.Join(missing, ", "), CallbackPath)
	}
}

// RedirectURI is where GitHub sends the browser back.
//
// Derived from the identity service's own base URL rather than from the
// request Host, for the reason oidcRedirectURI gives: a value taken from the
// request is a value an attacker chooses. It is also never typed into a
// manifest -- GitHub matches the registered callback by prefix, and a
// hand-written second copy is how a cluster ends up redirecting to an origin
// it does not serve.
func RedirectURI(identityBaseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(identityBaseURL), "/")
	if base == "" {
		return ""
	}
	return base + CallbackPath
}

// AuthorizeURL is where the browser goes to start the flow.
//
// `state` is the PLAINTEXT state value; only its digest is stored, so this is
// the one place the plaintext appears outside the reply to the caller who
// asked for it.
func (c Config) AuthorizeURL(redirectURI, state string) string {
	if !c.Configured() || strings.TrimSpace(redirectURI) == "" || strings.TrimSpace(state) == "" {
		return ""
	}
	q := url.Values{}
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	return "https://github.com/login/oauth/authorize?" + q.Encode()
}

// InstallURL is the app's installation page -- "Install on another
// organization". Distinct from AuthorizeURL: that one asks the person for
// their own authority, this one puts the app on an account's repositories.
func (c Config) InstallURL() string {
	if !c.Configured() {
		return ""
	}
	return "https://github.com/apps/" + url.PathEscape(c.AppSlug) + "/installations/new"
}
