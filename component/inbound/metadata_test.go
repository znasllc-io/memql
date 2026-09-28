package inbound

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerifiedDeliveryStagesOnlyAllowedMetadata(t *testing.T) {
	src := hexSource()
	src.ForwardHeaders = []string{"X-Topic", "Authorization", "Cookie", "X-Sig"}
	r := signedRequest(t, `{"id":1}`)
	r.Header.Set("X-Topic", "products/update")
	r.Header.Set("Authorization", "Bearer private-token")
	r.Header.Set("Cookie", "private-cookie")
	r.Header.Set("X-Unknown", "unrequested-value")
	eng := &fakeEngine{}
	w := httptest.NewRecorder()
	testHandler(t, eng, src).ServeHTTP(w, r)
	if w.Code != http.StatusAccepted || len(eng.calls) != 1 {
		t.Fatalf("delivery: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(eng.calls[0], `headersJson: "{\"x-topic\":\"products/update\"}"`) {
		t.Fatal("staged delivery lost its routing metadata")
	}
	for _, secret := range []string{"private-token", "private-cookie", "unrequested-value", r.Header.Get("X-Sig")} {
		if strings.Contains(eng.calls[0], secret) {
			t.Fatal("staged delivery contains credential or unrequested header")
		}
	}
}

func TestDeliveryMetadataRejectsAmbiguousOrUnstorableValues(t *testing.T) {
	for _, values := range [][]string{{"one", "two"}, {"bad\x00value"}, {strings.Repeat("x", 1025)}, {"bad\r\nvalue"}, {string([]byte{255})}} {
		src := hexSource()
		src.ForwardHeaders = []string{"X-Topic"}
		r := signedRequest(t, `{}`)
		r.Header["X-Topic"] = values
		eng := &fakeEngine{}
		w := httptest.NewRecorder()
		testHandler(t, eng, src).ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || len(eng.calls) != 0 {
			t.Fatalf("invalid metadata staged: %d", w.Code)
		}
	}
}

func TestMetadataRoundTripsQuotesWithoutChangingBytes(t *testing.T) {
	value := `quoted "topic" \\`
	raw, err := deliveryHeaders(SourceConfig{ForwardHeaders: []string{"X-Topic"}}, http.Header{"X-Topic": []string{value}})
	var got map[string]string
	if err != nil || json.Unmarshal([]byte(raw), &got) != nil || got["x-topic"] != value {
		t.Fatalf("metadata did not round trip: %q, %v", raw, err)
	}
}
