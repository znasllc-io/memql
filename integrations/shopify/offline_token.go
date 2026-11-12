package shopify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// The entire rotating pair and its deadlines are sealed as ONE secret row.
// A reader can never observe a new access token with the old refresh token.
type offlineGrant struct {
	AccessToken      string    `json:"access_token"`
	RefreshToken     string    `json:"refresh_token"`
	ClientID         string    `json:"client_id"`
	ClientSecret     string    `json:"client_secret"`
	Scope            string    `json:"scope"`
	ExpiresAt        time.Time `json:"expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}

func (g offlineGrant) String() string  { return "Shopify offline grant (sealed)" }
func (g offlineGrant) encoded() string { data, _ := json.Marshal(g); return string(data) }

func (c *Connector) exchangeOffline(ctx context.Context, shop, clientID, clientSecret, code, refresh string) (offlineGrant, error) {
	var grant offlineGrant
	shop, err := NormalizeShopDomain(shop)
	if err != nil {
		return grant, errConnectExchange
	}
	endpoint := "https://" + shop + "/admin/oauth/access_token"
	if c.connectTokenURL != nil {
		endpoint = c.connectTokenURL(shop)
	}
	form := url.Values{"client_id": {clientID}, "client_secret": {clientSecret}}
	if refresh != "" {
		form.Set("grant_type", "refresh_token")
		form.Set("refresh_token", refresh)
	} else {
		form.Set("code", code)
		form.Set("expiring", "1")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return grant, errConnectExchange
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: tokenExchangeTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return grant, errConnectExchange
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxConnectExchangeBody+1))
	if err != nil || len(body) > maxConnectExchangeBody {
		return grant, errConnectExchange
	}
	// THE ORIGIN'S OWN REFUSAL OF A RENEWAL. Shopify's token endpoint answers
	// a refresh token it no longer honours -- of an app that was uninstalled
	// or revoked, expired, unknown, or already replaced -- with one response
	// for all of them: 401 Unauthorized and {"error": "invalid_request"}
	// ("This request requires an active refresh_token"), which its
	// documentation calls final. THAT PAIR, AND ONLY THAT PAIR, says the grant
	// no longer exists. A 401 that says anything else, a 400 or a 403 about
	// the request or the app's own credentials, a 429, a 5xx and a renewal
	// that does not arrive say nothing about the grant: a shop's purge is
	// decided on this (compliance.go, storeAnswers), and a purge cannot be
	// taken back.
	if refresh != "" && response.StatusCode == http.StatusUnauthorized {
		var refusal struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &refusal) == nil && refusal.Error == renewalRefusedError {
			return grant, grantRefused{}
		}
		return grant, errConnectExchange
	}
	if response.StatusCode != http.StatusOK {
		return grant, errConnectExchange
	}
	var wire struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		Scope            string `json:"scope"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
	}
	if json.Unmarshal(body, &wire) != nil || wire.AccessToken == "" || wire.RefreshToken == "" || wire.ExpiresIn <= 0 || wire.RefreshExpiresIn <= 0 || wire.ExpiresIn > 31536000 || wire.RefreshExpiresIn > 31536000 {
		return grant, errConnectExchange
	}
	now := c.now().UTC()
	return offlineGrant{AccessToken: wire.AccessToken, RefreshToken: wire.RefreshToken, ClientID: clientID, ClientSecret: clientSecret, Scope: wire.Scope, ExpiresAt: now.Add(time.Duration(wire.ExpiresIn) * time.Second), RefreshExpiresAt: now.Add(time.Duration(wire.RefreshExpiresIn) * time.Second)}, nil
}

// tokenExchangeTimeout is how long the token endpoint is given to answer an
// exchange or a renewal. A variable, so a test can have one time out without
// sitting through it.
var tokenExchangeTimeout = 15 * time.Second

// renewalRefusedError is the error the token endpoint names, with a 401, when
// the refresh token it was shown is not one it honours any more.
const renewalRefusedError = "invalid_request"

// grantRefused is the origin's own refusal to renew a store's grant: the token
// endpoint's 401 invalid_request to its refresh token (exchangeOffline). It
// reads as any other failed renewal does, and can be told from one: a shop's
// purge takes it for the origin saying the app is gone (compliance.go,
// storeAnswers).
type grantRefused struct{}

func (grantRefused) Error() string { return "shopify: this store's connection could not be renewed" }

func (c *Connector) adminToken(ctx context.Context, store Store) (string, error) {
	if !store.Managed() {
		return c.stores.AdminToken(ctx, store)
	}
	// No cached token pair: another replica may have refreshed since this
	// replica's last request. The shared DB lock also serializes reauthorization.
	unlock, err := c.acquireConnectApp(ctx, "offline:"+store.ID)
	if err != nil {
		return "", fmt.Errorf("shopify: connection storage is unavailable")
	}
	defer unlock()
	return c.offlineAdminTokenLocked(ctx, store)
}

// offlineAdminTokenLocked is adminToken for a sealed offline grant, for a
// caller that already holds the store's offline lock: the uninstall handler,
// which must judge the grant and then disconnect the store with no reinstall
// or refresh landing in between (uninstall.go). The lock is not reentrant --
// it is a transaction-scoped advisory lock on its own connection -- so asking
// adminToken from under it would wait on itself.
//
// The sealed pair is read FRESH, never from this node's result cache: it is
// the read half of a read-modify-write another replica may have completed
// under this lock a moment ago -- a rotation, or a reinstall's new grant --
// and a cached pair would refresh with a token Shopify has already retired
// (memql#5431's rule).
func (c *Connector) offlineAdminTokenLocked(ctx context.Context, store Store) (string, error) {
	sealed, err := c.stores.Secret(memql.ContextWithFreshRead(operatorContext(ctx)), store.AdminTokenRef)
	var grant offlineGrant
	if err != nil || json.Unmarshal([]byte(sealed), &grant) != nil || grant.AccessToken == "" {
		return "", fmt.Errorf("shopify: reconnect this store")
	}
	if c.now().Add(time.Minute).Before(grant.ExpiresAt) {
		return grant.AccessToken, nil
	}
	if !c.now().Before(grant.RefreshExpiresAt) {
		return "", fmt.Errorf("shopify: reconnect this store")
	}
	next, err := c.exchangeOffline(ctx, store.Domain, grant.ClientID, grant.ClientSecret, "", grant.RefreshToken)
	if errors.As(err, &grantRefused{}) {
		return "", grantRefused{}
	}
	if err != nil {
		return "", fmt.Errorf("shopify: this store's connection could not be renewed")
	}
	if next.Scope == "" {
		next.Scope = grant.Scope
	}
	if _, err = seedSecret(operatorContext(ctx), c.engine, store.AdminTokenRef, next.encoded(), connectSealDescription, store.OwnerUserID); err != nil {
		return "", fmt.Errorf("shopify: the renewed connection could not be saved")
	}
	return next.AccessToken, nil
}
