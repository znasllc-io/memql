package shopify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOfflineExchangeRequiresCompleteExpiringPair(t *testing.T) {
	for _, body := range []string{
		`{"access_token":"secret","scope":"read_products"}`,
		`{"access_token":"secret","refresh_token":"refresh","expires_in":0,"refresh_token_expires_in":3600}`,
		`{"access_token":"secret","refresh_token":"refresh","expires_in":3600,"refresh_token_expires_in":9223372036854775807}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			c := NewConnector(nil, nil, nil, nil)
			c.connectTokenURL = func(string) string { return server.URL }
			_, err := c.exchangeOffline(context.Background(), "store.myshopify.com", "client", "client-secret", "code", "")
			if err == nil {
				t.Fatal("accepted an unusable offline grant")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "refresh") {
				t.Fatal("token response leaked in error")
			}
		})
	}
}
func TestOfflineExchangeDoesNotRedirectCredentials(t *testing.T) {
	reached := false
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer sink.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	c := NewConnector(nil, nil, nil, nil)
	c.connectTokenURL = func(string) string { return server.URL }
	if _, err := c.exchangeOffline(context.Background(), "store.myshopify.com", "client", "secret", "code", ""); err == nil {
		t.Fatal("redirect accepted")
	}
	if reached {
		t.Fatal("credentials followed redirect")
	}
}
