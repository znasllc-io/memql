package visit

import (
	"net/http"
	"net/netip"
	"testing"
)

func req(remote string, xff ...string) *http.Request {
	r := &http.Request{Header: http.Header{}, RemoteAddr: remote}
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestClientAddressIgnoresAForgedLeftMostEntry(t *testing.T) {
	cases := []struct {
		name string
		r    *http.Request
		want string
	}{
		{"forged left", req("10.0.0.1:1", "203.0.113.9, 198.51.100.7"), "198.51.100.7"},
		{"invalid right-most falls back to peer", req("192.0.2.5:99", "203.0.113.9, nonsense"), "192.0.2.5"},
		{"last line, last entry", req("10.0.0.1:1", "203.0.113.9, 198.51.100.1", "198.51.100.2, 198.51.100.7"), "198.51.100.7"},
		{"mapped is unmapped", req("10.0.0.1:1", "::ffff:198.51.100.7"), "198.51.100.7"},
	}
	for _, tc := range cases {
		got, ok := ClientAddress(tc.r)
		if !ok || AddressKey(got) != tc.want {
			t.Fatalf("%s: got %v ok=%v, want %s", tc.name, got, ok, tc.want)
		}
	}
	if _, ok := ClientAddress(req("garbage")); ok {
		t.Fatal("an unparseable peer must report no address")
	}
}

func TestAddressKeyUsesTheIPv6Slash64(t *testing.T) {
	if got := AddressKey(netip.MustParseAddr("2001:db8:1:2:3:4:5:6")); got != "2001:db8:1:2::/64" {
		t.Fatalf("got %q", got)
	}
}
