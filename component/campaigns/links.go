package campaigns

import (
	"net/url"
	"strings"
)

// links.go -- the URLs placed in an email (memql#6033).
//
// Every link in a message is built here from the secret and the origin the
// unsubscribe link has always used (Config.UnsubscribeSecret and
// Config.UnsubscribeBaseURL), so there is one place that knows the shape of a
// link and the pages cannot drift from the one-click unsubscribe. Each builder
// mints the token for ITS page and nothing else, so a URL built by ConfirmURL
// opens the confirm page and no other.
//
// The builders do not re-check the origin. A missing or unusable origin or
// secret is refused ahead of a send by Config.RequireUnsubscribe, as it is for
// the unsubscribe link.

const (
	// ConfirmPath, PreferencesPath and ViewPath are the routes the three
	// email-link pages are served from, beside UnsubscribePath. They are exact
	// paths: the token rides in the query, not in the path.
	ConfirmPath     = "/email/confirm"
	PreferencesPath = "/email/preferences"
	ViewPath        = "/email/view"
)

// UnsubscribeURL is the one-click unsubscribe link for a recipient: the u3
// token at UnsubscribePath.
func UnsubscribeURL(cfg Config, p UnsubscribePayload) (string, error) {
	token, err := MintUnsubscribeToken(cfg.UnsubscribeSecret, p)
	if err != nil {
		return "", err
	}
	return linkURL(cfg.UnsubscribeBaseURL, UnsubscribePath, token), nil
}

// ConfirmURL is the link that opens the confirm-subscription page. p must carry
// ExpiresAt; confirmLinkLifetime is how far ahead a caller sets it.
func ConfirmURL(cfg Config, p LinkPayload) (string, error) {
	return pageURL(cfg, LinkConfirm, ConfirmPath, p)
}

// PreferencesURL is the link that opens the preferences page.
func PreferencesURL(cfg Config, p LinkPayload) (string, error) {
	return pageURL(cfg, LinkPreferences, PreferencesPath, p)
}

// ViewURL is the link that opens the view-in-browser page. p must carry the
// template and the template revision that was sent.
func ViewURL(cfg Config, p LinkPayload) (string, error) {
	return pageURL(cfg, LinkView, ViewPath, p)
}

func pageURL(cfg Config, kind LinkKind, path string, p LinkPayload) (string, error) {
	token, err := MintLinkToken(cfg.UnsubscribeSecret, kind, p)
	if err != nil {
		return "", err
	}
	return linkURL(cfg.UnsubscribeBaseURL, path, token), nil
}

// linkURL joins origin, path and token: <origin><path>?token=<escaped token>.
// Whitespace around the configured origin and any trailing slashes are dropped,
// as validateUnsubscribeBaseURL and the unsubscribe link have always treated
// them; a path inside the origin is kept.
func linkURL(baseURL, path, token string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return base + path + "?token=" + url.QueryEscape(token)
}
