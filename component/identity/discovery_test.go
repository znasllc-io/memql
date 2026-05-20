package identity

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHostFromURL_StripsPort(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"http://localhost:8081", "localhost"},
		{"https://copresent.acme.com", "copresent.acme.com"},
		{"https://copresent.acme.com:8443/path", "copresent.acme.com"},
		{"localhost", "localhost"},
		{"localhost:8081", "localhost"},
		{"", ""},
		{"  http://identity:8081  ", "identity"},
	}
	for _, tc := range cases {
		got := hostFromURL(tc.in)
		if got != tc.want {
			t.Errorf("hostFromURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDeriveGRPCEndpoint_Defaults(t *testing.T) {
	emptyEnv := func(string) string { return "" }

	cases := []struct {
		name string
		url  string
		env  func(string) string
		want string
	}{
		{
			name: "https default to :443",
			url:  "https://copresent.acme.com",
			env:  emptyEnv,
			want: "copresent.acme.com:443",
		},
		{
			name: "http localhost default to :50050",
			url:  "http://localhost:8081",
			env:  emptyEnv,
			want: "localhost:50050",
		},
		{
			name: "explicit override wins",
			url:  "https://copresent.acme.com",
			env: func(k string) string {
				if k == "MEMQL_DISCOVERY_GRPC_ENDPOINT" {
					return "bff.copresent.acme.com:8443"
				}
				return ""
			},
			want: "bff.copresent.acme.com:8443",
		},
		{
			name: "MEMQL_GRPC_ADDRESS is intentionally NOT consulted",
			url:  "http://localhost:8081",
			env: func(k string) string {
				if k == "MEMQL_GRPC_ADDRESS" {
					return ":50051" // listen address; not a dial address
				}
				return ""
			},
			want: "localhost:50050",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveGRPCEndpoint(tc.url, tc.env)
			if got != tc.want {
				t.Errorf("deriveGRPCEndpoint(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

func TestDiscoveryHandler_DevDefaults(t *testing.T) {
	cfg := Config{
		BaseURL: "http://localhost:8081",
		RegisteredClients: []RegisteredClient{
			{ClientId: "copresent", RedirectURIs: []string{"http://localhost:8080/auth/callback"}},
			{ClientId: "cockpit", RedirectURIs: []string{"http://127.0.0.1/cockpit/callback"}},
		},
	}
	envValues := map[string]string{
		"MEMQL_DISCOVERY_CLIENT_ID":     "cockpit",
		"MEMQL_DISCOVERY_GRPC_ENDPOINT": "localhost:50050",
		"MEMQL_DISCOVERY_CLUSTER_NAME":  "local",
	}
	env := func(k string) string { return envValues[k] }

	handler := DiscoveryHandler(cfg, env)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/memql-config.json", nil)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"identityUrl":"http://localhost:8081"`,
		`"grpcEndpoint":"localhost:50050"`,
		`"clientId":"cockpit"`,
		`"clusterName":"local"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q missing %q", body, want)
		}
	}
}
