package shopify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
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
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return grant, errConnectExchange
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxConnectExchangeBody+1))
	if err != nil || len(body) > maxConnectExchangeBody || response.StatusCode != http.StatusOK {
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

func (c *Connector) adminToken(ctx context.Context, store Store) (string, error) {
	if !strings.HasSuffix(store.AdminTokenRef, "_OFFLINE_GRANT") {
		return c.stores.AdminToken(ctx, store)
	}
	// No cached token pair: another replica may have refreshed since this
	// replica's last request. The shared DB lock also serializes reauthorization.
	unlock, err := c.acquireConnectApp(ctx, "offline:"+store.ID)
	if err != nil {
		return "", fmt.Errorf("shopify: connection storage is unavailable")
	}
	defer unlock()
	sealed, err := c.stores.Secret(operatorContext(ctx), store.AdminTokenRef)
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
