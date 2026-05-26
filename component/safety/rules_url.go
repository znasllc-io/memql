package safety

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// URL / host rules cover ActionHTTPFetch + ActionWebhook. The
// classifier looks at Payload.URL (and Payload.Method for the
// audit-shaping); the body itself is rule-out-of-scope (Body is
// sent verbatim through the redactor before any logging).

// ruleHTTPUserinfo: a URL with userinfo (`https://user:pass@host`)
// is a strong credential_access signal -- the agent is about to
// transmit an embedded credential, or fetch through a credential
// already in the URL. Either way, surface it.
func ruleHTTPUserinfo(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionHTTPFetch && desc.Action != ActionWebhook {
		return Classification{}, ErrNoClassifierOpinion
	}
	raw := desc.Payload.URL
	if raw == "" {
		return Classification{}, ErrNoClassifierOpinion
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Don't classify on unparseable URLs -- escalate. A
		// downstream parser will reject the malformed URL anyway.
		return Classification{}, ErrNoClassifierOpinion
	}
	if u.User != nil {
		return cls(TierHigh,
			"URL contains userinfo (credential embedded in URL)",
			CategoryCredentialAccess), nil
	}
	return Classification{}, ErrNoClassifierOpinion
}

// ruleHTTPLocalNetwork: an http_fetch / webhook to a local /
// private / link-local / loopback / cloud-metadata address is a
// near-certain SSRF / exfiltration / privilege-elevation signal.
//
//   - loopback (127.0.0.0/8, ::1)
//   - link-local (169.254.0.0/16 -- includes the cloud metadata
//     service at 169.254.169.254; fe80::/10 for v6)
//   - RFC1918 private (10/8, 172.16/12, 192.168/16) and the IPv6
//     ULA range (fc00::/7)
//   - hostname `localhost` (case-insensitive) and `.local` mDNS
//     suffixes
//   - the unspecified address 0.0.0.0 / :: (which loopback-binds
//     in practice)
//
// Public / external hosts return ErrNoClassifierOpinion so the LLM
// (or no opinion → policy default) handles them.
func ruleHTTPLocalNetwork(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionHTTPFetch && desc.Action != ActionWebhook {
		return Classification{}, ErrNoClassifierOpinion
	}
	raw := desc.Payload.URL
	if raw == "" {
		return Classification{}, ErrNoClassifierOpinion
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return Classification{}, ErrNoClassifierOpinion
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return Classification{}, ErrNoClassifierOpinion
	}
	// Plain hostname matches.
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return cls(TierHigh,
			fmt.Sprintf("network egress to local/mDNS host %q (SSRF / exfiltration risk)", host),
			CategoryExfiltration, CategoryNetworkEgress), nil
	}
	// IP matches.
	if ip := net.ParseIP(host); ip != nil && isPrivateOrLocal(ip) {
		return cls(TierHigh,
			fmt.Sprintf("network egress to private/local IP %s (SSRF / metadata-service / exfiltration risk)", ip),
			CategoryExfiltration, CategoryNetworkEgress), nil
	}
	return Classification{}, ErrNoClassifierOpinion
}

// isPrivateOrLocal returns true for IPs the classifier treats as
// "local / private / metadata" -- everything an agent contacting
// the public internet shouldn't be reaching unless explicitly
// approved. Uses the stdlib classifiers for the common cases.
func isPrivateOrLocal(ip net.IP) bool {
	switch {
	case ip.IsLoopback():
		return true
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return true
	case ip.IsPrivate():
		return true
	case ip.IsUnspecified():
		return true
	case ip.IsMulticast():
		return true
	}
	// Cloud metadata service IPv6 alias.
	if ip.To4() == nil && ip.String() == "fd00:ec2::254" {
		return true
	}
	return false
}

// NewWebhookHostAllowlistRule returns a Rule that denies any
// http_fetch / webhook whose hostname is not on `allowedHosts`
// (case-insensitive exact match, same semantics as the existing
// `MemQLEngine.validateWebhookHost`). An empty `allowedHosts` means
// "no allowlist configured" -- the rule never denies (matches the
// engine's existing "empty list = allow all" semantics).
//
// The returned Rule has a stable ID (`http.webhook_host_allowlist`)
// so audit + telemetry can grep it. Wire from #229 at gate
// construction time with the engine's configured hosts:
//
//	rules := append(safety.DefaultRules(),
//	    safety.NewWebhookHostAllowlistRule(cfg.WebhookAllowedHosts))
//	classifier := safety.NewRuleClassifier(rules...)
func NewWebhookHostAllowlistRule(allowedHosts []string) Rule {
	allowed := make(map[string]struct{}, len(allowedHosts))
	for _, h := range allowedHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" {
			allowed[h] = struct{}{}
		}
	}
	return Rule{
		ID: "http.webhook_host_allowlist",
		Fn: func(desc ActionDescriptor) (Classification, error) {
			if len(allowed) == 0 {
				// Empty allowlist == not configured == allow all
				// (parity with MemQLEngine.validateWebhookHost).
				return Classification{}, ErrNoClassifierOpinion
			}
			if desc.Action != ActionWebhook && desc.Action != ActionHTTPFetch {
				return Classification{}, ErrNoClassifierOpinion
			}
			raw := desc.Payload.URL
			if raw == "" {
				return Classification{}, ErrNoClassifierOpinion
			}
			u, err := url.Parse(raw)
			if err != nil || u.Host == "" {
				return Classification{}, ErrNoClassifierOpinion
			}
			host := strings.ToLower(u.Host)
			if _, ok := allowed[host]; ok {
				return Classification{}, ErrNoClassifierOpinion
			}
			return cls(TierHigh,
				fmt.Sprintf("webhook/http host %q not on the configured allowlist", host),
				CategoryNetworkEgress), nil
		},
	}
}
