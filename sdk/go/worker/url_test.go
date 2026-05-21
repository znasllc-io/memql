package worker

import "testing"

func TestParseClusterURL(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantEnd    string
		wantTLS    bool
		wantErr    bool
	}{
		{name: "bare host:port", raw: "memql.local:50051", wantEnd: "memql.local:50051", wantTLS: false},
		{name: "bare host", raw: "memql.local", wantEnd: "memql.local", wantTLS: false},
		{name: "http", raw: "http://memql.local:8080", wantEnd: "memql.local:8080", wantTLS: false},
		{name: "grpc", raw: "grpc://memql.local:50051", wantEnd: "memql.local:50051", wantTLS: false},
		{name: "https with port", raw: "https://memql.local:443", wantEnd: "memql.local:443", wantTLS: true},
		{name: "https no port", raw: "https://memql.local", wantEnd: "memql.local:443", wantTLS: true},
		{name: "grpcs", raw: "grpcs://memql.local", wantEnd: "memql.local:443", wantTLS: true},
		{name: "empty", raw: "", wantErr: true},
		{name: "scheme without host", raw: "http://", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint, useTLS, err := ParseClusterURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got endpoint=%q useTLS=%v", endpoint, useTLS)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if endpoint != tt.wantEnd {
				t.Errorf("endpoint = %q, want %q", endpoint, tt.wantEnd)
			}
			if useTLS != tt.wantTLS {
				t.Errorf("useTLS = %v, want %v", useTLS, tt.wantTLS)
			}
		})
	}
}
