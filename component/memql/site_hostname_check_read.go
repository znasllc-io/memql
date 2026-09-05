package memql

import (
	"context"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// The two ADDRESS CHECKS (2026-09-05 design, D7): whether a hostname a person
// is typing, or one the OS just generated, could be claimed right now.
//
// # A check, never a gate
//
// Both answer the question the write guards will ask -- the same shape rules,
// the same uniqueness probes, in the same code -- and neither reserves
// anything. A name that checks out here and is taken a second later is
// refused by the guard on the write, exactly as it always was; the guard is
// the authority and this is the courtesy that stops a person walking a whole
// flow to learn at the end that the first thing they typed was taken.
//
// # What it discloses, and what it does not
//
// `available: false` with `reason: "taken"` says that SOME row holds the name.
// The write guard's refusal names the holding site's id, because whoever is
// refused has to know what to go and look at; a check that is asked on every
// keystroke does not, so it says "taken" and nothing about who. The shape
// half's sentence is the policy's own, verbatim, because it names the rule
// that was broken and a paraphrase would drop the fact that helps.
//
// # Virtual, like every other engine-native read
//
// One row per call, never persisted, the dataOrigins / providerVerify shape.

// SiteHostnameCheckConcept is the virtual concept a site address check answers in.
const SiteHostnameCheckConcept = "v1:platform:siteHostnameCheck"

// CustomDomainCheckConcept is the virtual concept a custom-domain check answers in.
const CustomDomainCheckConcept = "v1:platform:customDomainCheck"

// Reasons an address is not available. A closed set, so the OS can key a
// sentence on the reason and render the policy's own sentence beneath it.
const (
	hostnameCheckOk      = "ok"
	hostnameCheckInvalid = "invalid"
	hostnameCheckTaken   = "taken"
)

// evaluateSiteHostnameCheckExpression answers whether THIS caller could
// create a site at `hostname` right now: the user-claimable shape (waived for
// a privileged caller exactly as the write guard waives it) and cluster-wide
// uniqueness against every live site.
func (e *MemQLEngine) evaluateSiteHostnameCheckExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	hostname := strings.ToLower(strings.TrimSpace(stringArg(args, "hostname")))
	if hostname == "" {
		return nil, fmt.Errorf("siteHostnameCheck: a hostname is required")
	}
	actor := strings.TrimSpace(rowAuthzActorUserId(ctx))
	if !siteWritePrivileged(ctx, actor) {
		if err := validateUserSiteHostname(hostname, siteHostnamePolicyDomain()); err != nil {
			return hostnameCheckRow(SiteHostnameCheckConcept, hostname, hostnameCheckInvalid, err.Error())
		}
	}
	holders, err := e.liveSiteIdsForHostname(ctx, hostname)
	if err != nil {
		return nil, fmt.Errorf("siteHostnameCheck: cannot check %q: %w", hostname, err)
	}
	if len(holders) > 0 {
		return hostnameCheckRow(SiteHostnameCheckConcept, hostname, hostnameCheckTaken,
			fmt.Sprintf("%s is already taken by another deployable in this cluster. Pick another name.", hostname))
	}
	return hostnameCheckRow(SiteHostnameCheckConcept, hostname, hostnameCheckOk, "")
}

// evaluateCustomDomainCheckExpression answers whether a client's own domain
// could be bound right now: the custom-domain shape rules and uniqueness
// against both live sites and live bindings. Cluster-owner only, as the
// concept it speaks for is.
func (e *MemQLEngine) evaluateCustomDomainCheckExpression(ctx context.Context, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if e == nil {
		return nil, fmt.Errorf("engine is nil")
	}
	if !rowAuthzIsClusterOwner(ctx) {
		return nil, fmt.Errorf("customDomainCheck is a cluster owner's question: binding a client's own domain is a cluster owner's act")
	}
	hostname := strings.ToLower(strings.TrimSpace(stringArg(args, "hostname")))
	if hostname == "" {
		return nil, fmt.Errorf("customDomainCheck: a hostname is required")
	}
	if err := validateCustomDomainHostname(hostname, customDomainPolicyDomain()); err != nil {
		return hostnameCheckRow(CustomDomainCheckConcept, hostname, hostnameCheckInvalid, err.Error())
	}
	sites, err := e.liveSiteIdsForHostname(ctx, hostname)
	if err != nil {
		return nil, fmt.Errorf("customDomainCheck: cannot check %q: %w", hostname, err)
	}
	if len(sites) > 0 {
		return hostnameCheckRow(CustomDomainCheckConcept, hostname, hostnameCheckTaken,
			fmt.Sprintf("%s is already served by a deployable in this cluster.", hostname))
	}
	bindings, err := e.liveCustomDomainIdsForHostname(ctx, hostname)
	if err != nil {
		return nil, fmt.Errorf("customDomainCheck: cannot check %q: %w", hostname, err)
	}
	if len(bindings) > 0 {
		return hostnameCheckRow(CustomDomainCheckConcept, hostname, hostnameCheckTaken,
			fmt.Sprintf("%s is already bound to a deployable in this cluster. Remove that binding first.", hostname))
	}
	return hostnameCheckRow(CustomDomainCheckConcept, hostname, hostnameCheckOk, "")
}

func hostnameCheckRow(concept, hostname, reason, problem string) ([]memorynodes.MemoryNode, error) {
	return singleVirtualRow(concept, hostname, map[string]any{
		"hostname":  hostname,
		"available": reason == hostnameCheckOk,
		"reason":    reason,
		"problem":   problem,
	})
}
