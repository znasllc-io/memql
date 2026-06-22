package telnyx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/integrations/telephony"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(Options{APIKey: "test-key", ConnectionID: "conn-123", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewRequiresAPIKey(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("expected error without API key")
	}
}

func TestSearchNumbers(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q", got)
		}
		q := r.URL.Query()
		if got := q.Get("filter[national_destination_code]"); got != "415" {
			t.Errorf("area-code filter = %q, want 415", got)
		}
		if got := q.Get("filter[phone_number_type]"); got != "local" {
			t.Errorf("type filter = %q, want local", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{
				"phone_number":     "+14155550100",
				"cost_information": map[string]any{"monthly_cost": "1.00"},
				"features":         []map[string]any{{"name": "voice"}, {"name": "sms"}},
				"region_information": []map[string]any{{"region_type": "location", "region_name": "SAN FRANCISCO"}},
			}},
		})
	})
	got, err := c.SearchNumbers(context.Background(), telephony.NumberQuery{AreaCode: "415", Limit: 5})
	if err != nil {
		t.Fatalf("SearchNumbers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d numbers, want 1", len(got))
	}
	n := got[0]
	if n.E164 != "+14155550100" || n.ProviderID != "+14155550100" {
		t.Errorf("e164/providerID = %q/%q", n.E164, n.ProviderID)
	}
	if n.Carrier != "telnyx" || n.MonthlyCost != 1.00 || n.Region != "SAN FRANCISCO" {
		t.Errorf("number = %+v", n)
	}
	if len(n.Capabilities) != 2 {
		t.Errorf("caps = %v", n.Capabilities)
	}
}

func TestSearchTollFreeTypeMapping(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("filter[phone_number_type]"); got != "toll_free" {
			t.Errorf("want toll_free filter, got %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
	})
	if _, err := c.SearchNumbers(context.Background(), telephony.NumberQuery{Type: telephony.NumberTypeTollFree}); err != nil {
		t.Fatalf("SearchNumbers: %v", err)
	}
}

func TestBuyNumber(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/number_orders" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		var body numberOrderRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.PhoneNumbers) != 1 || body.PhoneNumbers[0].PhoneNumber != "+14155550100" {
			t.Errorf("body = %+v", body)
		}
		if body.ConnectionID != "conn-123" {
			t.Errorf("connection id = %q, want conn-123", body.ConnectionID)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"id":            "order-1",
				"status":        "success",
				"phone_numbers": []map[string]any{{"phone_number": "+14155550100"}},
			},
		})
	})
	n, err := c.BuyNumber(context.Background(), "+14155550100")
	if err != nil {
		t.Fatalf("BuyNumber: %v", err)
	}
	if n.E164 != "+14155550100" {
		t.Errorf("e164 = %q", n.E164)
	}
}

func TestReleaseNumber(t *testing.T) {
	var deleted string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/phone_numbers":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "num-9", "phone_number": "+14155550100"}},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/phone_numbers/num-9":
			deleted = "num-9"
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	if err := c.ReleaseNumber(context.Background(), "+14155550100"); err != nil {
		t.Fatalf("ReleaseNumber: %v", err)
	}
	if deleted != "num-9" {
		t.Fatalf("delete not issued for resolved id")
	}
}

func TestConfigureInboundAssignsConnection(t *testing.T) {
	var patched updateNumberRequest
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/phone_numbers":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "num-9", "phone_number": "+14155550100"}},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/phone_numbers/num-9":
			_ = json.NewDecoder(r.Body).Decode(&patched)
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	if err := c.ConfigureInbound(context.Background(), "+14155550100", "sip:edge.example.com:5061;transport=tls"); err != nil {
		t.Fatalf("ConfigureInbound: %v", err)
	}
	if patched.ConnectionID != "conn-123" {
		t.Fatalf("patched connection = %q, want conn-123", patched.ConnectionID)
	}
}

func TestConfigureInboundWithoutConnectionErrors(t *testing.T) {
	c, _ := New(Options{APIKey: "k"}) // no connection id
	err := c.ConfigureInbound(context.Background(), "+14155550100", "sip:edge")
	if err == nil || !strings.Contains(err.Error(), "connection id") {
		t.Fatalf("want connection-id error, got %v", err)
	}
}

func TestErrorEnvelopeSurfacesDetail(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]any{{"code": "10015", "title": "Invalid number", "detail": "not available"}},
		})
	})
	_, err := c.BuyNumber(context.Background(), "+1bad")
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("want surfaced detail, got %v", err)
	}
}

func TestRegisteredAsDefaultCarrier(t *testing.T) {
	// register.go init() must have registered telnyx; SelectCarrier("") with
	// TELNYX_API_KEY present builds it.
	t.Setenv("TELNYX_API_KEY", "k")
	c, err := telephony.SelectCarrier("")
	if err != nil {
		t.Fatalf("SelectCarrier: %v", err)
	}
	if c.Name() != "telnyx" {
		t.Fatalf("default carrier = %q", c.Name())
	}
}
