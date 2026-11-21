package edge

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFamilyPollingAndRevocationUseSeparateBudgets(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"action":"status","invitationId":"opaque"}`, callsOpAccountRead},
		{`{"identity":"seat","roomId":"room","action":"list"}`, callsOpAccountRead},
		{`{"action":"revoke","roomId":"room","identity":"seat","invitationId":"opaque","idempotencyKey":"nonce"}`, callsOpLeave},
		{`{"action":"claim","invitationToken":"opaque"}`, callsOpJoin},
		{`{"action":"status","action":"mock_approve"}`, callsOpJoin},
		{`{"action":"status","Action":"mock_approve"}`, callsOpJoin},
		{`{"action":"status","parentalAuthority":true}`, callsOpJoin},
		{`{"action":"status","idempotencyKey":"nonce"}`, callsOpJoin},
		{`{"action":"status"} {"action":"mock_approve"}`, callsOpJoin},
		{`{"action":"status","invitationId":42}`, callsOpJoin},
	} {
		r := httptest.NewRequest("POST", "/_memql/calls/family", strings.NewReader(tc.body))
		if got := callsFamilyBudget(r); got != tc.want {
			t.Errorf("body %q got %q want %q", tc.body, got, tc.want)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != tc.body {
			t.Fatal("request body was changed")
		}
	}
}
