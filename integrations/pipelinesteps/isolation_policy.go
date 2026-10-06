package pipelinesteps

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
)

// CIDR grants and pod identities are separate policy mechanisms on Cilium.
// A successful pod probe cannot establish these exclusions. Read every additive
// policy in the step namespace and refuse an IP grant that reaches a protected
// range. The live probe remains responsible for actual pod-policy enforcement.
// These are the minimum exclusions in the shipped substrate, not a complete
// inventory of a provider's endpoints or an operator's custom network ranges.
var isolationProtectedCIDRs = []string{
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "168.63.129.16/32",
}

type isolationPolicy struct {
	Metadata ObjectMeta `json:"metadata"`
	Spec     struct {
		PodSelector *struct {
			MatchLabels      map[string]string `json:"matchLabels"`
			MatchExpressions []json.RawMessage `json:"matchExpressions"`
		} `json:"podSelector"`
		PolicyTypes []string `json:"policyTypes"`
		Egress      []struct {
			To []struct {
				PodSelector       json.RawMessage `json:"podSelector"`
				NamespaceSelector json.RawMessage `json:"namespaceSelector"`
				IPBlock           *struct {
					CIDR   string   `json:"cidr"`
					Except []string `json:"except"`
				} `json:"ipBlock"`
			} `json:"to"`
		} `json:"egress"`
	} `json:"spec"`
}

func (k *Kube) CheckIsolationCIDRs(ctx context.Context) error {
	out, err := k.api.Do(ctx, http.MethodGet, "/apis/networking.k8s.io/v1/namespaces/"+url.PathEscape(k.ns)+"/networkpolicies", "", nil)
	if err != nil {
		return fmt.Errorf("reading pipeline network policies: %w", err)
	}
	var policies struct {
		Items    []isolationPolicy `json:"items"`
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(out, &policies); err != nil {
		return fmt.Errorf("reading pipeline network policies: %w", err)
	}
	if policies.Metadata.Continue != "" {
		return fmt.Errorf("pipeline network policy inventory is incomplete")
	}
	if len(policies.Items) == 0 {
		return fmt.Errorf("pipeline namespace has no network policies")
	}
	isolatesAllPods := false
	for _, policy := range policies.Items {
		selector := policy.Spec.PodSelector
		if selector != nil && len(selector.MatchLabels) == 0 && len(selector.MatchExpressions) == 0 {
			for _, kind := range policy.Spec.PolicyTypes {
				if kind == "Egress" {
					isolatesAllPods = true
				}
			}
		}
		for _, egress := range policy.Spec.Egress {
			if len(egress.To) == 0 {
				return fmt.Errorf("network policy %s permits egress to every address", policy.Metadata.Name)
			}
			for _, peer := range egress.To {
				if peer.IPBlock == nil {
					if len(peer.PodSelector) == 0 && len(peer.NamespaceSelector) == 0 {
						return fmt.Errorf("network policy %s permits an unrestricted peer", policy.Metadata.Name)
					}
					continue
				}
				grant, err := netip.ParsePrefix(peer.IPBlock.CIDR)
				if err != nil || !grant.Addr().Is4() {
					return fmt.Errorf("network policy %s has an unsupported IP grant %q", policy.Metadata.Name, peer.IPBlock.CIDR)
				}
				exclusions := make([]netip.Prefix, 0, len(peer.IPBlock.Except))
				for _, raw := range peer.IPBlock.Except {
					prefix, err := netip.ParsePrefix(raw)
					if err != nil {
						return fmt.Errorf("network policy %s has an invalid exclusion %q", policy.Metadata.Name, raw)
					}
					exclusions = append(exclusions, prefix.Masked())
				}
				for _, raw := range isolationProtectedCIDRs {
					protected := netip.MustParsePrefix(raw)
					if !grant.Overlaps(protected) {
						continue
					}
					intersection := protected
					if grant.Bits() > protected.Bits() {
						intersection = grant.Masked()
					}
					covered := false
					for _, excluded := range exclusions {
						if excluded.Bits() <= intersection.Bits() && excluded.Contains(intersection.Addr()) {
							covered = true
							break
						}
					}
					if !covered {
						return fmt.Errorf("network policy %s permits protected IP range %s", policy.Metadata.Name, intersection)
					}
				}
			}
		}
	}
	if !isolatesAllPods {
		return fmt.Errorf("no network policy isolates egress for every pipeline pod")
	}
	return nil
}
