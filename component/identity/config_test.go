package identity

import "testing"

func TestIsLocalHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		// Loopback literals.
		{"localhost", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"0.0.0.0", true},
		{"localhost:8081", true},
		{"127.0.0.1:8081", true},
		// .localhost TLD (RFC 6761).
		{"foo.localhost", true},
		{"identity.localhost", true},
		// Dev wildcard convention: *.local.<domain>.
		{"identity.local.znas.io", true},
		{"app.local.znas.io", true},
		{"bff.local.acme.test", true},
		// Production-shaped hostnames must NOT be treated as local --
		// they pair with a real cert and require the encryption key.
		{"identity.acme.com", false},
		{"identity.staging.acme.com", false},
		{"local.io", false},                      // local-only-as-TLD-label, not dev shape
		{"localdev.example.com", false},          // "local" must be a full label, not a substring
		{"identity.localdev.example.com", false}, // ditto
	}
	for _, tc := range cases {
		got := isLocalHost(tc.host)
		if got != tc.want {
			t.Errorf("isLocalHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}
