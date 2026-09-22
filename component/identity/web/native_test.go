package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/identity"
)

func nativeTestServer(t *testing.T) (*Server, *http.ServeMux) {
	t.Helper()
	s, err := NewServer(identity.Config{BaseURL: "https://identity.example.test"}, slog.Default(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.CountUsers = func(context.Context) (int, error) { return 0, nil }
	s.ClusterClaimed = func(context.Context) (bool, error) { return false, nil }
	mux := http.NewServeMux()
	s.Mount(mux)
	return s, mux
}

func nativeGET(mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("Accept", NativeMediaType)
	r.Header.Set("Origin", "https://os.example.test")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestNativeOwnershipNeedsBothReadableSignals(t *testing.T) {
	s, mux := nativeTestServer(t)
	if w := nativeGET(mux, "/auth/setup/state"); w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"unclaimed"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	s.ClusterClaimed = func(context.Context) (bool, error) { return true, nil }
	if w := nativeGET(mux, "/auth/setup/state"); !strings.Contains(w.Body.String(), `"state":"claimed"`) {
		t.Fatal(w.Body.String())
	}
	s.CountUsers = func(context.Context) (int, error) { return 0, errors.New("database disconnected") }
	if w := nativeGET(mux, "/auth/setup/state"); w.Code != 503 || strings.Contains(w.Body.String(), "unclaimed") {
		t.Fatal(w.Code, w.Body.String())
	}
	s.CountUsers = nil
	if w := nativeGET(mux, "/auth/setup/state"); w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func TestNativeSetupCrossReplicaCSRFAndOrigin(t *testing.T) {
	_, first := nativeTestServer(t)
	s, second := nativeTestServer(t)
	issued := 0
	s.IssueMagicLink = func(context.Context, IssueMagicLinkInput) (IssueMagicLinkResult, error) {
		issued++
		return IssueMagicLinkResult{}, nil
	}
	page := nativeGET(first, "/setup")
	var result struct {
		CSRF string `json:"csrf"`
		Page string `json:"page"`
	}
	if err := json.Unmarshal(page.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Page != "setup_wizard" || result.CSRF == "" {
		t.Fatal(page.Body.String())
	}
	for _, tc := range []struct {
		origin, csrf string
		want         int
	}{
		{"https://evil.test", result.CSRF, 403}, {"https://os.example.test", "", 403}, {"https://os.example.test", result.CSRF, 200},
	} {
		r := httptest.NewRequest("POST", "/setup", strings.NewReader("domain=example.test&brand_name=Example&owner_email=owner%40example.test&owner_first_name=First&owner_last_name=Last"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Accept", NativeMediaType)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-CSRF-Token", tc.csrf)
		for _, cookie := range page.Result().Cookies() {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		second.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("origin %s csrf %q: %d %s", tc.origin, tc.csrf, w.Code, w.Body.String())
		}
	}
	if issued != 1 {
		t.Fatalf("issued %d links", issued)
	}
}

func TestOldIdentityPageHandsOffWithoutExposingQueryToOSLogs(t *testing.T) {
	_, mux := nativeTestServer(t)
	r := httptest.NewRequest("GET", "/setup?state=a%2Bb&code_challenge=a%25b", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatal(w.Code, w.Body.String())
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "os.example.test" || u.Path != "/identity/setup" || u.RawQuery != "" || u.Fragment != r.URL.RawQuery {
		t.Fatal(u)
	}
	if strings.Contains(w.Body.String(), "<form") {
		t.Fatal("standalone form remains reachable")
	}
}

func TestNativeSetupDeliveryFailureIsNotSuccess(t *testing.T) {
	s, mux := nativeTestServer(t)
	s.IssueMagicLink = func(context.Context, IssueMagicLinkInput) (IssueMagicLinkResult, error) {
		return IssueMagicLinkResult{}, errors.New("mail unavailable")
	}
	// Handler-level test isolates the delivery outcome from the CSRF test above.
	r := httptest.NewRequest("POST", "/setup", strings.NewReader("domain=example.test&brand_name=Example&owner_email=a%40example.test&owner_first_name=A&owner_last_name=B"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept", NativeMediaType)
	w := httptest.NewRecorder()
	s.handleSetupPost(w, r)
	if w.Code != 503 || strings.Contains(w.Body.String(), "check-email") {
		t.Fatal(w.Code, w.Body.String())
	}
	_ = mux
}

func TestSetupRequiresOrganizationBeforeAnySettingsOrEnrollmentWrite(t *testing.T) {
	for _, name := range []string{"", "   ", strings.Repeat("x", 201)} {
		t.Run(fmt.Sprint(len(name)), func(t *testing.T) {
			s, _ := nativeTestServer(t)
			s.PersistClusterSettings = func(context.Context, ClusterSettingsInput) error {
				t.Fatal("invalid organization reached persistence")
				return nil
			}
			s.IssueMagicLink = func(context.Context, IssueMagicLinkInput) (IssueMagicLinkResult, error) {
				t.Fatal("invalid organization sent verification")
				return IssueMagicLinkResult{}, nil
			}
			form := url.Values{"domain": {"example.test"}, "brand_name": {name}, "owner_email": {"ada@example.test"}, "owner_first_name": {"Ada"}, "owner_last_name": {"Owner"}}
			r := httptest.NewRequest("POST", "/setup", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Accept", NativeMediaType)
			w := httptest.NewRecorder()
			s.handleSetupPost(w, r)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Organization name") {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}
