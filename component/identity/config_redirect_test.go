package identity

import "testing"

func TestAllowsRedirectURI_LoopbackAnyPort(t *testing.T) {
	cfg := Config{
		RegisteredClients: []RegisteredClient{
			{
				ClientId: "cockpit",
				RedirectURIs: []string{
					"http://127.0.0.1/cockpit/callback",
					"http://localhost/cockpit/callback",
				},
			},
			{
				ClientId: "app",
				RedirectURIs: []string{
					"http://localhost:8080/auth/callback",
				},
			},
		},
	}

	cases := []struct {
		name     string
		clientId string
		uri      string
		want     bool
	}{
		{
			name:     "loopback 127.0.0.1 with arbitrary port",
			clientId: "cockpit",
			uri:      "http://127.0.0.1:54321/cockpit/callback",
			want:     true,
		},
		{
			name:     "loopback localhost with arbitrary port",
			clientId: "cockpit",
			uri:      "http://localhost:60000/cockpit/callback",
			want:     true,
		},
		{
			name:     "loopback wrong path -- rejected",
			clientId: "cockpit",
			uri:      "http://127.0.0.1:54321/some/other/path",
			want:     false,
		},
		{
			name:     "loopback https scheme mismatch -- rejected",
			clientId: "cockpit",
			uri:      "https://127.0.0.1:54321/cockpit/callback",
			want:     false,
		},
		{
			name:     "non-loopback host -- exact match required",
			clientId: "app",
			uri:      "http://localhost:8080/auth/callback",
			want:     true,
		},
		{
			name:     "non-loopback wrong port -- exact match required, no flex",
			clientId: "app",
			uri:      "http://localhost:9090/auth/callback",
			want:     false,
		},
		{
			name:     "unknown client -- rejected",
			clientId: "stranger",
			uri:      "http://127.0.0.1:54321/cockpit/callback",
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cfg.AllowsRedirectURI(tc.clientId, tc.uri)
			if got != tc.want {
				t.Errorf("AllowsRedirectURI(%q, %q) = %v, want %v", tc.clientId, tc.uri, got, tc.want)
			}
		})
	}
}
