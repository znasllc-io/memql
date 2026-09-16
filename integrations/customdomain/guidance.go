package customdomain

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// DNS guidance is read under the caller's actor and resolved afresh from the
// configured edge host. It must also work after reopening an existing binding;
// the transient add result is not a durable source of routing instructions.
func (i *Integration) handleDNSGuidance(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	domainID := strings.TrimSpace(rowString(args, "domainId"))
	if domainID == "" {
		return nil, fmt.Errorf("customDomainDNSGuidance: domainId is required")
	}
	rows, err := i.store.callerRows(ctx, fmt.Sprintf("query customDomainById(domainId: %s)", langparser.QuoteString(domainID)))
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("customDomainDNSGuidance: binding is not available to this caller")
	}
	edge := NormalizeHostname(i.cfg.EdgeHost)
	if edge == "" {
		return nil, fmt.Errorf("customDomainDNSGuidance: cluster routing hostname is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addresses, err := i.resolver.LookupHost(ctx, edge)
	if err != nil {
		return nil, fmt.Errorf("customDomainDNSGuidance: could not resolve the cluster routing address: %w", err)
	}
	ipv4, ipv6 := []string{}, []string{}
	seen := map[netip.Addr]bool{}
	for _, value := range addresses {
		address, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil || address.Zone() != "" {
			continue
		}
		address = address.Unmap()
		if seen[address] {
			continue
		}
		seen[address] = true
		if address.Is4() {
			ipv4 = append(ipv4, address.String())
		} else {
			ipv6 = append(ipv6, address.String())
		}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("customDomainDNSGuidance: cluster routing hostname has no usable IP addresses")
	}
	sort.Strings(ipv4)
	sort.Strings(ipv6)
	return i.node("dnsGuidance:"+domainID, map[string]any{
		"hostname": rowString(rows[0], "hostname"), "edgeHost": edge,
		"ipv4": ipv4, "ipv6": ipv6,
	})
}
