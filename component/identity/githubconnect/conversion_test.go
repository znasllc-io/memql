package githubconnect

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// conversion_test.go -- spending GitHub's one-time code (design record
// 2026-09-20-github-app-setup, D3). The reply is the most credential-dense
// body this package reads, so half of this file is about what must NOT appear
// in an error.

const (
	testManifestCode = "a180b1a3d263c81bc6441d7b990bae27d4c10679"
	testAppSecret    = "example-conversion-client-secret"
	testHookSecret   = "example-conversion-webhook-secret"
	// testPEMMarker stands where key material would be. It is not any: nothing
	// in this package parses what GitHub sends, only carries it.
	testPEMMarker = "MIIEexamplekeymaterial"
)

// THE PEM IS ASSEMBLED, NEVER SPELLED. A begin-to-end private-key block is
// what the repository's secret scanner refuses on sight, in a fixture exactly
// as in a leak -- and its push lane reads COMMITS, so a block that was ever
// spelled in this file stays a finding after it is edited away
// (.github/workflows/gitleaks.yml). Building the armor from its parts keeps
// the fixture PEM-shaped for the code under test and keeps the shape out of
// the source.
func pemArmor(edge string) string { return "-----" + edge + " RSA PRIVATE KEY-----" }

var (
	testPEMBody = pemArmor("BEGIN") + "\n" + testPEMMarker + "\n" + pemArmor("END")
	// As GitHub's JSON carries it: the newlines escaped.
	testPEMJSON       = strings.ReplaceAll(testPEMBody, "\n", `\n`)
	conversionReplyOK = `{"id":424242,"slug":"memql-on-lab-example-com","name":"MemQL on lab.example.com",` +
		`"html_url":"https://github.com/apps/memql-on-lab-example-com","owner":{"login":"znasllc-io"},` +
		`"client_id":"Iv1.conversionclient","client_secret":"` + testAppSecret + `","webhook_secret":"` + testHookSecret + `",` +
		`"pem":"` + testPEMJSON + `",` +
		`"permissions":{"contents":"read","metadata":"read"},"events":["push"]}`
)

func conversionServer(t *testing.T, status int, body string, seen *[]string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = append(*seen, r.Method+" "+r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{OAuthBaseURL: srv.URL, APIBaseURL: srv.URL}
}

func neverMint() (string, error) { return "", errors.New("the mint was not expected") }

func TestTheCodeIsTradedForAllSixValues(t *testing.T) {
	var seen []string
	c := conversionServer(t, http.StatusCreated, conversionReplyOK, &seen)
	reg, err := c.ConvertManifest(context.Background(), testManifestCode, neverMint)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "POST /app-manifests/"+testManifestCode+"/conversions" {
		t.Fatalf("GitHub saw %v", seen)
	}
	if !reg.Config.Configured() {
		t.Fatalf("the registration is missing %v", reg.Config.Missing())
	}
	if reg.Config.AppID != "424242" || reg.Config.AppSlug != "memql-on-lab-example-com" || reg.Config.ClientID != "Iv1.conversionclient" {
		t.Errorf("identifiers = %q %q %q", reg.Config.AppID, reg.Config.AppSlug, reg.Config.ClientID)
	}
	if reg.Config.ClientSecret != testAppSecret || reg.Config.WebhookSecret != testHookSecret || reg.WebhookSecretGenerated {
		t.Error("the secrets GitHub returned are not the ones kept")
	}
	// THE KEY IS KEPT IN THE ONE FORM THE TREE READS IT IN: base64 of the PEM,
	// whole. Decoding it has to give the PEM back, newlines included.
	raw, derr := base64.StdEncoding.DecodeString(reg.Config.PrivateKeyB64)
	if derr != nil || strings.TrimSpace(string(raw)) != testPEMBody {
		t.Errorf("the private key did not survive the round trip: %v", derr)
	}
	if reg.OwnerLogin != "znasllc-io" || reg.Name != "MemQL on lab.example.com" || reg.HTMLURL != "https://github.com/apps/memql-on-lab-example-com" {
		t.Errorf("what the owner sees: %q %q %q", reg.OwnerLogin, reg.Name, reg.HTMLURL)
	}
	if !PermissionsAreWithin(reg.Permissions, RequestedPermissions()) {
		t.Errorf("permissions = %v", reg.Permissions)
	}
}

// An app whose webhook is off comes back with `webhook_secret: null`. The six
// are all-or-none everywhere else, so one is minted -- and said to be.
func TestAnAppWithNoWebhookSecretGetsOneMintedHere(t *testing.T) {
	body := strings.Replace(conversionReplyOK, `"webhook_secret":"`+testHookSecret+`"`, `"webhook_secret":null`, 1)
	c := conversionServer(t, http.StatusCreated, body, nil)
	reg, err := c.ConvertManifest(context.Background(), testManifestCode, func() (string, error) { return "minted-here", nil })
	if err != nil {
		t.Fatal(err)
	}
	if reg.Config.WebhookSecret != "minted-here" || !reg.WebhookSecretGenerated {
		t.Errorf("webhook secret = %q generated=%v", reg.Config.WebhookSecret, reg.WebhookSecretGenerated)
	}
	if !reg.Config.Configured() {
		t.Errorf("missing %v", reg.Config.Missing())
	}
}

func TestASpentOrUnknownCodeIsRefusedByName(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnprocessableEntity} {
		c := conversionServer(t, status, `{"message":"Not Found"}`, nil)
		if _, err := c.ConvertManifest(context.Background(), testManifestCode, neverMint); !errors.Is(err, ErrConversionRefused) {
			t.Errorf("HTTP %d: err = %v, want ErrConversionRefused", status, err)
		}
	}
	if _, err := (&Client{}).ConvertManifest(context.Background(), "   ", neverMint); !errors.Is(err, ErrConversionRefused) {
		t.Errorf("an empty code: err = %v", err)
	}
}

func TestAnAppMissingAValueIsRefusedNamingIt(t *testing.T) {
	body := strings.Replace(conversionReplyOK, `"pem":"`+testPEMJSON+`",`, `"pem":"",`, 1)
	c := conversionServer(t, http.StatusCreated, body, nil)
	_, err := c.ConvertManifest(context.Background(), testManifestCode, neverMint)
	if err == nil || !strings.Contains(err.Error(), EnvPrivateKeyB64) {
		t.Fatalf("err = %v, want it to name %s", err, EnvPrivateKeyB64)
	}
}

// TestNoErrorCarriesTheCodeTheURLOrACredential. The code is a PATH SEGMENT of
// the endpoint, so the habit every other call here has -- naming its URL in its
// errors -- would put an unspent code in a log. Each failure mode is driven and
// its error scanned, WITH A CONTROL: the secrets really were in the body the
// cluster received, so "does not contain" is not passing on an empty string.
func TestNoErrorCarriesTheCodeTheURLOrACredential(t *testing.T) {
	forbidden := []string{testManifestCode, testAppSecret, testHookSecret, testPEMMarker, "127.0.0.1", "http://"}

	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()

	cases := map[string]*Client{
		"a status GitHub does not document": conversionServer(t, http.StatusInternalServerError, conversionReplyOK, nil),
		"a body that is not JSON":           conversionServer(t, http.StatusCreated, conversionReplyOK[:len(conversionReplyOK)-40], nil),
		"an app with a value missing":       conversionServer(t, http.StatusCreated, strings.Replace(conversionReplyOK, `"slug":"memql-on-lab-example-com",`, "", 1), nil),
		"GitHub unreachable":                {OAuthBaseURL: closedURL, APIBaseURL: closedURL},
	}
	for name, c := range cases {
		_, err := c.ConvertManifest(context.Background(), testManifestCode, neverMint)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		for _, secret := range forbidden {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: the error carries %q:\n  %v", name, secret, err)
			}
		}
		if !strings.Contains(err.Error(), "/app-manifests/{code}/conversions") && !strings.Contains(err.Error(), "MEMQL_GITHUB_APP_") {
			t.Errorf("%s: the error does not say which call failed: %v", name, err)
		}
	}
	// THE CONTROL.
	for _, secret := range []string{testAppSecret, testHookSecret, testPEMMarker} {
		if !strings.Contains(conversionReplyOK, secret) {
			t.Fatalf("the fixture does not carry %q, so the scan above proved nothing", secret)
		}
	}
}

func TestPermissionsAreCheckedAgainstWhatWasAsked(t *testing.T) {
	asked := RequestedPermissions()
	for name, tc := range map[string]struct {
		got  map[string]string
		want bool
	}{
		"exactly what was asked":        {map[string]string{"contents": "read", "metadata": "read"}, true},
		"less":                          {map[string]string{"metadata": "read"}, true},
		"nothing":                       {nil, true},
		"an explicit none":              {map[string]string{"contents": "read", "issues": "none"}, true},
		"write where read was asked":    {map[string]string{"contents": "write", "metadata": "read"}, false},
		"a permission nobody asked for": {map[string]string{"contents": "read", "administration": "read"}, false},
		"a level this build never saw":  {map[string]string{"contents": "superuser"}, false},
	} {
		if got := PermissionsAreWithin(tc.got, asked); got != tc.want {
			t.Errorf("%s: PermissionsAreWithin = %v, want %v", name, got, tc.want)
		}
	}
}
