package githubconnect

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// conversion.go -- trading GitHub's one-time code for the app it just created
// (design record 2026-09-20-github-app-setup, D3).
//
// ONE CALL, AND IT RETURNS EVERYTHING. POST /app-manifests/{code}/conversions
// answers the app's id, slug, client id, client secret, webhook secret and
// private key in a single body, exactly once: the code is spent by the call,
// and GitHub offers no second way to read the key it generated. So this is the
// most credential-dense reply this package handles, and three rules follow.
//
//   - THE ENDPOINT IS NEVER IN AN ERROR. Every other call here names its URL in
//     its errors, which is right for them and wrong for this one: the CODE is a
//     path segment, and until it is spent it is as good as the credentials it
//     unlocks. Errors below say conversionEndpoint -- the route's SHAPE.
//   - THE BODY IS NEVER IN AN ERROR, success or failure.
//   - THE REPLY NEVER LEAVES AS TEXT. Registration has no String, is never
//     logged, and carries the key already base64-encoded -- the one form the
//     rest of the tree accepts it in (EnvPrivateKeyB64), so no second spelling
//     of the key exists to be mishandled.

// conversionEndpoint names the route in errors, without the code that is in it.
const conversionEndpoint = "POST /app-manifests/{code}/conversions"

// ErrConversionRefused is GitHub saying the code is not good: spent, expired
// (it lives for an hour), or never issued. The person starts again; there is
// nothing to fix.
var ErrConversionRefused = errors.New("githubconnect: GitHub refused the manifest code")

// Registration is the app GitHub created, as this cluster keeps it.
type Registration struct {
	// The six values, named as Config names them.
	Config Config
	// Name is what the owner called the app on GitHub's confirmation page.
	Name string
	// HTMLURL is the app's own page, https://github.com/apps/<slug>.
	HTMLURL string
	// OwnerLogin is the account or organization the app was registered under.
	OwnerLogin string
	// Permissions is what GitHub says the app may do, for the caller to check
	// against what was asked.
	Permissions map[string]string
	// WebhookSecretGenerated reports that GitHub returned no webhook secret and
	// one was minted here instead (see ConvertManifest).
	WebhookSecretGenerated bool
}

// conversionReply is GitHub's body. Decoded into a struct of its own rather
// than straight into Registration so the wire names stay in one place.
type conversionReply struct {
	ID            int64             `json:"id"`
	Slug          string            `json:"slug"`
	Name          string            `json:"name"`
	HTMLURL       string            `json:"html_url"`
	ClientID      string            `json:"client_id"`
	ClientSecret  string            `json:"client_secret"`
	WebhookSecret *string           `json:"webhook_secret"`
	PEM           string            `json:"pem"`
	Permissions   map[string]string `json:"permissions"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// ConvertManifest spends the code and answers the app.
//
// `mintSecret` supplies a webhook secret when GitHub returns none, which it
// does for an app whose webhook is off (the field is documented as nullable).
// The six values are all-or-none everywhere else in this tree, and an app with
// its webhook off still has to satisfy that rule; a locally minted secret does,
// and is never used, because nothing is ever delivered to verify.
func (c *Client) ConvertManifest(ctx context.Context, code string, mintSecret func() (string, error)) (Registration, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return Registration{}, ErrConversionRefused
	}
	endpoint := c.apiBase() + "/app-manifests/" + url.PathEscape(code) + "/conversions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return Registration{}, fmt.Errorf("githubconnect: build %s", conversionEndpoint)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		// NOT wrapped. A transport error from net/http quotes the request URL,
		// and the URL carries the code.
		return Registration{}, fmt.Errorf("githubconnect: %s could not reach GitHub", conversionEndpoint)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Registration{}, fmt.Errorf("githubconnect: read the reply to %s", conversionEndpoint)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity:
		// 404: no such code. 422: validation failed, or the endpoint was
		// spammed. Both are "this code is not good", and the repair is the same.
		return Registration{}, ErrConversionRefused
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return Registration{}, fmt.Errorf("githubconnect: %s answered %d", conversionEndpoint, resp.StatusCode)
	}

	var reply conversionReply
	if err := json.Unmarshal(body, &reply); err != nil {
		// The decode error is dropped too: encoding/json quotes the offending
		// bytes, and these bytes are credentials.
		return Registration{}, fmt.Errorf("githubconnect: %s answered a body this cluster could not read", conversionEndpoint)
	}

	reg := Registration{
		Name:        strings.TrimSpace(reply.Name),
		HTMLURL:     strings.TrimSpace(reply.HTMLURL),
		OwnerLogin:  strings.TrimSpace(reply.Owner.Login),
		Permissions: reply.Permissions,
		Config: Config{
			AppSlug:      strings.TrimSpace(reply.Slug),
			ClientID:     strings.TrimSpace(reply.ClientID),
			ClientSecret: strings.TrimSpace(reply.ClientSecret),
		},
	}
	if reply.ID > 0 {
		reg.Config.AppID = strconv.FormatInt(reply.ID, 10)
	}
	if pem := strings.TrimSpace(reply.PEM); pem != "" {
		// Base64 of the PEM, WHOLE, newlines included -- the form
		// MEMQL_GITHUB_APP_PRIVATE_KEY_B64 has always taken, so the key an
		// owner's registration stores and the key an operator exports are read
		// by the same parser.
		reg.Config.PrivateKeyB64 = base64.StdEncoding.EncodeToString([]byte(pem + "\n"))
	}
	if reply.WebhookSecret != nil {
		reg.Config.WebhookSecret = strings.TrimSpace(*reply.WebhookSecret)
	}
	if reg.Config.WebhookSecret == "" && mintSecret != nil {
		secret, merr := mintSecret()
		if merr != nil {
			return Registration{}, fmt.Errorf("githubconnect: mint a webhook secret: %w", merr)
		}
		reg.Config.WebhookSecret = strings.TrimSpace(secret)
		reg.WebhookSecretGenerated = true
	}

	if missing := reg.Config.Missing(); len(missing) > 0 {
		// NAMES, never values: what GitHub left out, in the six's own words.
		return Registration{}, fmt.Errorf("githubconnect: %s answered an app with no %s", conversionEndpoint, strings.Join(missing, ", "))
	}
	return reg, nil
}

// PermissionsAreWithin reports whether everything `got` grants is something
// `asked` asked for, at no higher level.
//
// The owner can edit only the app's NAME on GitHub's confirmation page, so an
// app coming back with more than contents and metadata read is not the app
// this cluster's manifest described -- whatever produced it, its credentials
// are not ones to keep. "Within" rather than "equal": GitHub may report a
// permission it implies (metadata read comes with everything), and refusing a
// correct app over a field it added would break setup for no safety.
func PermissionsAreWithin(got, asked map[string]string) bool {
	rank := map[string]int{"": 0, "none": 0, "read": 1, "write": 2, "admin": 3}
	for name, level := range got {
		have, known := rank[strings.ToLower(strings.TrimSpace(level))]
		if !known {
			return false
		}
		if have > rank[strings.ToLower(strings.TrimSpace(asked[name]))] {
			return false
		}
	}
	return true
}
