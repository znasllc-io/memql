// Package visit classifies one HTTP request into the fields a website
// statistic is counted by. It is pure: no database, no network, so the edge's
// request handler can call it on the hot path and tests need no fixtures.
package visit

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ipv6KeyBits is the prefix an IPv6 address is keyed on: one subscriber is
// handed a /64, so keying on the whole address would let a single household
// look like many visitors.
const ipv6KeyBits = 64

// ClientAddress is the address a request is attributed to: the right-most
// valid X-Forwarded-For entry, else the peer. The right-most entry is the one
// the ingress itself appended; every entry to its left is the caller's own
// claim and trivially forged. The same rule bounds access-code guessing, so a
// statistic and a rate limit never disagree about who a visitor is.
//
// THE COST: the right-most entry is the visitor only when the ingress is the
// first proxy to see them. Behind another proxy, visitors share that proxy's
// address. The second result is false when neither source yields an address.
func ClientAddress(r *http.Request) (netip.Addr, bool) {
	if entry := rightMostForwarded(r); entry != "" {
		if addr, err := netip.ParseAddr(entry); err == nil && addr.Zone() == "" {
			return addr, true
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	// The peer's own zone is nobody's to choose and says nothing about who this is.
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.WithZone(""), true
	}
	return netip.Addr{}, false
}

// AddressKey is what one visitor is counted under: an IPv4 address, an
// IPv4-mapped one as the address it maps, and an IPv6 address as its /64.
func AddressKey(a netip.Addr) string {
	a = a.Unmap()
	if a.Is6() {
		return netip.PrefixFrom(a, ipv6KeyBits).Masked().String()
	}
	return a.String()
}

// rightMostForwarded is the right-most non-empty X-Forwarded-For entry across
// every header line, last line first (an ingress may add its own line rather
// than extend the caller's). It scans from the right without splitting, since
// the header's length is the caller's to choose.
func rightMostForwarded(r *http.Request) string {
	lines := r.Header.Values("X-Forwarded-For")
	for i := len(lines) - 1; i >= 0; i-- {
		rest := lines[i]
		for {
			comma := strings.LastIndexByte(rest, ',')
			if entry := strings.TrimSpace(rest[comma+1:]); entry != "" {
				return entry
			}
			if comma < 0 {
				break
			}
			rest = rest[:comma]
		}
	}
	return ""
}
