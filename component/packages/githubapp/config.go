// Package githubapp is the engine's half of the cluster's GitHub App (epic
// memql#4912, C1/C6).
//
// It exists because a GitHub App speaks with TWO voices and the engine needs
// both. The person's USER token is what "list the repositories you can reach"
// is asked under -- it is their authority, it expires in eight hours, and it
// is refreshed here rather than by sending them back through a browser. The
// INSTALLATION token is what a fetch, a poll or an auto-deploy runs under: it
// is minted from the app's own private key for one installation, so background
// work never depends on somebody being awake and signed in (C6).
//
// Three rules hold everywhere in this package:
//
//   - NO TOKEN, REFRESH TOKEN OR CLIENT SECRET EVER LEAVES IT AS TEXT. Not in
//     an error, not in a returned struct, not in a log line -- this package
//     has no logger, which is the cheapest way to keep that true. A StatusError
//     carries the status and the ENDPOINT, and the endpoint is a path with no
//     query, because GitHub's own OAuth endpoints take secrets in the body and
//     a URL in an error is how one reaches a log.
//   - AN ABSENT CONFIGURATION IS AN ANSWER, NOT A BOOT FAILURE. The engine
//     nodes read the app's configuration and refuse the CALL with
//     ErrNotConfigured when it is missing; the identity node is where a
//     partial configuration refuses boot, because it is the node that would
//     otherwise present a Connect button that fails per person (design B).
//   - THE BASE URLS ARE FIELDS. A test stands a fake GitHub behind them, and a
//     fake reached through a path-routed transport is the same exercise the
//     rest of component/packages already gets.
package githubapp

import (
	"os"
	"strings"
)

// The five values an ENGINE node needs. The sixth of the design's six,
// MEMQL_GITHUB_APP_WEBHOOK_SECRET, is deliberately absent: nothing in this
// package verifies a delivery -- the inbound seam already did, under its own
// per-source HMAC key -- and a second reader of a signing secret is a second
// place it can be logged.
//
// One const and one envValue call per variable, so a grep for the name finds
// exactly one read site. That is the same rule component/packages' own
// envValue records, applied inside this package rather than by importing the
// parent, which would be an import cycle.
const (
	EnvAppId         = "MEMQL_GITHUB_APP_ID"
	EnvSlug          = "MEMQL_GITHUB_APP_SLUG"
	EnvClientId      = "MEMQL_GITHUB_APP_CLIENT_ID"
	EnvClientSecret  = "MEMQL_GITHUB_APP_CLIENT_SECRET"
	EnvPrivateKeyB64 = "MEMQL_GITHUB_APP_PRIVATE_KEY_B64"
)

// envValue reads an environment variable. Named so the read sites above have
// one function a grep finds, exactly as component/packages.envValue does.
func envValue(key string) string { return os.Getenv(key) }

// Config is the app's identity on this cluster.
//
// PrivateKeyB64 is the only secret here and it is never rendered: Missing()
// reports NAMES, and nothing in this package formats a Config.
type Config struct {
	// AppId is the app's numeric id, the `iss` of every app JWT.
	AppId string
	// Slug is the app's URL slug, and the only reason it is here is the
	// installation LINK: "install on another organization" is a URL a person
	// follows, and a cluster that could mint tokens but not name where to
	// install would answer repository_not_installed with no repair attached.
	Slug string
	// ClientId and ClientSecret are the OAuth pair, used for exactly two
	// calls: refreshing a user token, and revoking a grant at disconnect.
	ClientId     string
	ClientSecret string
	// PrivateKeyB64 is the app's RSA private key, PEM, base64-encoded. Base64
	// rather than raw PEM because it travels through a Kubernetes secret and
	// an env var, and a multi-line value in either is a formatting accident
	// waiting to happen.
	PrivateKeyB64 string
}

// ConfigFromEnv reads the five values. Trimmed, because a secret pasted into a
// manifest arrives with a newline more often than not, and a key that differs
// from the real one by trailing whitespace fails at signature verification
// with a message about the signature.
func ConfigFromEnv() Config {
	return Config{
		AppId:         strings.TrimSpace(envValue(EnvAppId)),
		Slug:          strings.TrimSpace(envValue(EnvSlug)),
		ClientId:      strings.TrimSpace(envValue(EnvClientId)),
		ClientSecret:  strings.TrimSpace(envValue(EnvClientSecret)),
		PrivateKeyB64: strings.TrimSpace(envValue(EnvPrivateKeyB64)),
	}
}

// Configured reports whether every value an engine node needs is present.
//
// ALL FIVE OR NONE, matching the identity node's boot rule (design B). A
// partial configuration is treated as absent rather than as "most of an app":
// three of the five would mint installation tokens and fail every refresh,
// which presents to a person as a connection that works until tomorrow.
func (c Config) Configured() bool { return len(c.Missing()) == 0 }

// Missing names the values that are absent, in declaration order, so an
// operator reading a refusal is told what to set rather than that something is
// wrong. It returns names and never values.
func (c Config) Missing() []string {
	var out []string
	for _, pair := range []struct {
		name  string
		value string
	}{
		{EnvAppId, c.AppId},
		{EnvSlug, c.Slug},
		{EnvClientId, c.ClientId},
		{EnvClientSecret, c.ClientSecret},
		{EnvPrivateKeyB64, c.PrivateKeyB64},
	} {
		if strings.TrimSpace(pair.value) == "" {
			out = append(out, pair.name)
		}
	}
	return out
}
