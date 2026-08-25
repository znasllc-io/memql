package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/devicecode"
)

// builtin_client_device_test.go -- memql#4515.
//
// The device flow was the FALLBACK the extension reached for when the loopback
// redirect could not serve (a remote host, a locked-down network), and it
// dead-ended in the same place the browser flow did: /device/code resolves the
// client through identity.ResolveClient, which knew only static config and the
// DCR store, so on a cluster with DCR off there was no path to a client_id at
// all. This pins that the built-in editor client is admitted here with NOTHING
// configured -- which is the whole point of it being compiled in.

// newBareDeviceServer is newDeviceTestServer with the static client list
// EMPTY. That is the shape of a hardened cluster, and the shape the old
// resolver had no answer for.
func newBareDeviceServer(t *testing.T) *Server {
	t.Helper()
	s, _ := newDeviceTestServer(t)
	s.Cfg.RegisteredClients = nil
	return s
}

func postDeviceCode(t *testing.T, s *Server, clientId string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/device/code",
		strings.NewReader("client_id="+clientId))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	s.handleDeviceCode(rec, req)
	return rec
}

func TestDeviceCode_AcceptsTheBuiltinEditorClientWithNothingConfigured(t *testing.T) {
	s := newBareDeviceServer(t)

	rec := postDeviceCode(t, s, identity.BuiltinClientVSCode)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /device/code status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out DeviceAuthorizationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		t.Fatalf("device authorization returned no credentials: %+v", out)
	}
	if out.Interval != devicecode.DefaultIntervalSeconds {
		t.Errorf("interval = %d, want %d", out.Interval, devicecode.DefaultIntervalSeconds)
	}
}

func TestDeviceCode_StillRefusesAnUnregisteredClient(t *testing.T) {
	// The built-in tier is additive: it must not turn /device/code into a
	// surface that accepts any client_id somebody types.
	s := newBareDeviceServer(t)

	rec := postDeviceCode(t, s, "not-a-registered-client")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unregistered client; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_client") {
		t.Errorf("body = %s, want invalid_client", rec.Body.String())
	}
}
