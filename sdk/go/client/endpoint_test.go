package client

import "testing"

func TestParseClusterEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantDial string
		wantTLS  bool
		wantErr  bool
	}{
		{"bare host:port plaintext", "localhost:50050", "localhost:50050", false, false},
		{"bare host gets default gRPC port", "localhost", "localhost:50051", false, false},
		{"grpc scheme plaintext", "grpc://localhost:50050", "localhost:50050", false, false},
		{"http scheme plaintext", "http://localhost:50050", "localhost:50050", false, false},
		{"https scheme TLS default 443", "https://bff.local.znas.io", "bff.local.znas.io:443", true, false},
		{"https scheme TLS explicit port", "https://bff.local.znas.io:50443", "bff.local.znas.io:50443", true, false},
		{"grpcs scheme TLS default 443", "grpcs://bff.local.znas.io", "bff.local.znas.io:443", true, false},
		{"empty", "", "", false, true},
		{"unsupported scheme", "ssh://host:22", "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dial, useTLS, err := ParseClusterEndpoint(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got dial=%q useTLS=%v", dial, useTLS)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if dial != tc.wantDial {
				t.Errorf("dial: want %q, got %q", tc.wantDial, dial)
			}
			if useTLS != tc.wantTLS {
				t.Errorf("useTLS: want %v, got %v", tc.wantTLS, useTLS)
			}
		})
	}
}
