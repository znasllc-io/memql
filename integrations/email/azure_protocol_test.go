package email

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAzureDeviceFlowKeepsTokensOffPublicResponses(t *testing.T) {
	a := newAzureProtocol("12345678-1234-1234-1234-123456789012")
	a.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "login.microsoftonline.com" {
			t.Fatal("unexpected authentication host")
		}
		data, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(data))
		if form.Get("client_id") != a.clientID {
			t.Fatal("wrong OAuth client")
		}
		body := `{"device_code":"private-device-code","user_code":"VISIBLE","verification_uri":"https://microsoft.com/devicelogin","expires_in":900,"interval":5}`
		code := 200
		if strings.HasSuffix(r.URL.Path, "/token") {
			if form.Get("device_code") != "private-device-code" {
				t.Fatal("lost private challenge")
			}
			body = `{"error":"authorization_pending","error_description":"private-provider-diagnostic"}`
			code = 400
		} else if form.Get("scope") != azureManagementScope+" offline_access" {
			t.Fatal("unexpected Azure permissions")
		}
		return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	grant, err := a.begin(context.Background(), "organizations")
	if err != nil || grant.UserCode != "VISIBLE" {
		t.Fatal(err)
	}
	reply, err := a.token(context.Background(), "organizations", url.Values{"device_code": {grant.DeviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}})
	if err != nil || reply.Error != "authorization_pending" || reply.AccessToken != "" {
		t.Fatal("pending sign-in was not preserved", err)
	}
}

func TestAzureManagementPaginationRefusesCredentialExfiltration(t *testing.T) {
	a := newAzureProtocol("12345678-1234-1234-1234-123456789012")
	requests := 0
	a.client.Transport = acsRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.Host != "management.azure.com" {
			t.Fatal("bearer token left ARM")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"value":[{"subscriptionId":"first"}],"nextLink":"https://outside.test/collect"}`))}, nil
	})
	if _, err := a.list(context.Background(), "/subscriptions?api-version=2022-12-01", "secret"); err == nil {
		t.Fatal("untrusted continuation accepted")
	}
	if requests != 1 {
		t.Fatal("credential exfiltration request attempted")
	}
}

func TestAzureProtocolErrorsNeverExposeResponseBodies(t *testing.T) {
	a := newAzureProtocol("12345678-1234-1234-1234-123456789012")
	a.client.Transport = acsRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"private-credential-and-address"}}`))}, nil
	})
	err := a.arm(context.Background(), http.MethodGet, "/subscriptions?api-version=2022-12-01", "secret", nil, nil)
	if err == nil || strings.Contains(err.Error(), "private-") {
		t.Fatal("provider diagnostic leaked", err)
	}
}
