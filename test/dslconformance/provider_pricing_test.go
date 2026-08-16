package dslconformance

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Provider pricing metadata (memql#3854).
//
// # Why a gate, and why this particular one
//
// `inputCostPerMillion` / `outputCostPerMillion` on a provider are read by
// component/memql's Pricing and surfaced through the router. Under memQL Cloud
// (epic memql#3852) they are also the AI cost of goods that every tier margin
// is computed against.
//
// An understated cost does not present as an error. It presents as a business
// that looks profitable. `chat54Mini` carried $0.15 / $0.60 -- the gpt-4o-mini
// rates -- carried forward through two model renames while the model underneath
// changed twice, against an official gpt-5.4-mini list of $0.75 / $4.50. Five
// times understated on input, seven on output, silently, for as long as it took
// somebody to attach money to it.
//
// # What can actually be gated, and what cannot
//
// A test cannot know a vendor's list price; there is no offline source of truth
// for that, and a hardcoded table here would rot exactly the way the provider
// params did. What it CAN check is INTERNAL CONSISTENCY -- and that turns out
// to be where the defect actually lived.
//
// `chat54Mini` and `stream54Mini` are the same `@model`. So are `chat54` and
// `stream54`, `claudeOpus` / `streamClaudeOpus` / `reasoningClaudeOpus`, and
// several more. Two providers for one model that disagree about what that model
// costs is a defect no reading of either file alone reveals -- and it is
// precisely what an incomplete correction produces, because the natural fix is
// to change the provider somebody named in the ticket and stop.
//
// That is the whole of this gate's claim. It does not verify prices against the
// world. It verifies that this tree tells one story about each model, which
// makes a partial correction fail loudly instead of leaving the two halves of a
// model disagreeing.

var (
	providerDecl = regexp.MustCompile(`(?m)^provider\s+(\w+)\s*\{`)
	modelAttr    = regexp.MustCompile(`(?m)^@model\("([^"]+)"\)`)
	costParam    = regexp.MustCompile(`(?m)^\s*(inputCostPerMillion|outputCostPerMillion|cachedInputCostPerMillion)\s+([0-9.]+)\s*$`)
)

type providerPricing struct {
	name  string
	model string
	costs map[string]float64
}

// parseProviders reads dsl/providers/providers.memql into per-provider pricing.
//
// A line-scanner rather than the DSL loader, deliberately: the loader gives
// registered providers, and a provider that fails to register is exactly the
// state where the numbers are least trustworthy. This reads what is authored.
func parseProviders(t *testing.T) []providerPricing {
	t.Helper()
	path := filepath.Join(dslPath(t, "."), "providers", "providers.memql")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	// Split on provider declarations, keeping the @model preamble that precedes
	// each one -- the attribute sits ABOVE the declaration, so a naive split on
	// `provider ` puts it in the previous block.
	lines := strings.Split(string(b), "\n")
	var out []providerPricing
	var pendingModel string
	var cur *providerPricing

	for _, line := range lines {
		if m := modelAttr.FindStringSubmatch(line); m != nil {
			pendingModel = m[1]
			continue
		}
		if m := providerDecl.FindStringSubmatch(line); m != nil {
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &providerPricing{name: m[1], model: pendingModel, costs: map[string]float64{}}
			pendingModel = ""
			continue
		}
		if cur == nil {
			continue
		}
		if m := costParam.FindStringSubmatch(line); m != nil {
			v, err := strconv.ParseFloat(m[2], 64)
			if err != nil {
				t.Errorf("provider %s: %s is not a number: %q", cur.name, m[1], m[2])
				continue
			}
			cur.costs[m[1]] = v
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	if len(out) == 0 {
		t.Fatal("parsed no providers -- either providers.memql moved or this parse stopped matching, and either way this gate is watching nothing")
	}
	return out
}

// pricingAliasExemptions names providers that DELIBERATELY price themselves as
// something other than the model they currently declare, with the reason.
//
// One case today, and it is a real one rather than a shrug. `chat54Pro` and
// `stream54Pro` are the pro tier, and pro pricing ($5 / $20) is what they are
// for; they carry `@model("gpt-5.4")` only because `gpt-5.4-pro` is
// responses-API-only and rejects a v1/chat/completions request with a 404. The
// provider comments say so and say the @model goes back when this codebase adds
// Responses API support.
//
// So the disagreement this gate reports is TRUE, TEMPORARY, and pointed the
// SAFE way: they overstate cost of goods against the model actually being
// called, which makes a margin look worse than it is. Repricing them to the
// flagship rate would be correct today and wrong again the moment the alias is
// removed -- and wrong in the direction that flatters the business.
//
// The exemption goes STALE when the alias does. A provider listed here whose
// pricing has come to AGREE with its model's other providers fails the gate,
// because that is the state after somebody switched the @model back and this
// entry became a claim about nothing. An exemption list nobody prunes becomes a
// list of claims nobody checks.
var pricingAliasExemptions = map[string]string{
	"chat54Pro":   "pro-tier pricing on a flagship @model alias; gpt-5.4-pro is responses-API only (see the provider comment). Overstates COGS, which is the safe direction, and reverts when the alias does.",
	"stream54Pro": "the streaming half of the same alias, for the same reason.",
}

// TestProvidersForOneModelAgreeOnItsPrice.
//
// Two providers naming the same @model must report the same cost. They are the
// same tokens billed by the same vendor at the same rate; the only thing that
// differs is whether we stream the response.
//
// This is the gate that would have caught memql#3854's defect at half-fix time:
// correcting `chat54Mini` and not `stream54Mini` leaves the streaming provider
// -- the one the agent reply path actually uses -- still reporting a fifth of
// the real input cost.
func TestProvidersForOneModelAgreeOnItsPrice(t *testing.T) {
	byModel := map[string][]providerPricing{}
	for _, p := range parseProviders(t) {
		if p.model == "" || len(p.costs) == 0 {
			// A provider with no @model is a base vendor record; one with no
			// cost params has not had pricing attached. Neither is this gate's
			// business -- see TestPricedProvidersPriceBothDirections for the
			// half-priced case.
			continue
		}
		byModel[p.model] = append(byModel[p.model], p)
	}

	var compared int
	// exemptionUsed records which entries actually excused a disagreement, so a
	// stale one can be reported below.
	exemptionUsed := map[string]bool{}

	for model, group := range byModel {
		// Compare only the providers that are NOT deliberately mispriced, so a
		// group of three with one alias still checks the other two against each
		// other rather than being excused wholesale.
		var plain []providerPricing
		for _, p := range group {
			if _, exempt := pricingAliasExemptions[p.name]; exempt {
				continue
			}
			plain = append(plain, p)
		}
		// Note the alias's disagreement as USED only when there is something for
		// it to disagree with -- an alias alone in its group is excusing nothing.
		if len(plain) > 0 && len(plain) < len(group) {
			for _, p := range group {
				if _, exempt := pricingAliasExemptions[p.name]; exempt && !samePricing(p, plain[0]) {
					exemptionUsed[p.name] = true
				}
			}
		}
		if len(plain) < 2 {
			continue
		}
		compared++
		first := plain[0]
		for _, other := range plain[1:] {
			for _, key := range []string{"inputCostPerMillion", "outputCostPerMillion", "cachedInputCostPerMillion"} {
				a, aok := first.costs[key]
				b, bok := other.costs[key]
				if !aok && !bok {
					continue
				}
				if aok != bok {
					t.Errorf("model %q: provider %s declares %s and %s does not. Same model, same vendor, same rate -- one of them is reporting a cost the other says does not exist.",
						model, pick(aok, first.name, other.name), key, pick(aok, other.name, first.name))
					continue
				}
				if a != b {
					t.Errorf("model %q: %s says %s=%g, %s says %g. Two providers for one model cannot disagree about what that model costs -- this is what a half-applied price correction looks like.",
						model, first.name, key, a, other.name, b)
				}
			}
		}
	}
	if compared == 0 {
		t.Fatal("no model is served by two providers, so this gate compared nothing. Either the tree changed shape or the parse broke.")
	}

	// A stale exemption is a claim about nothing, and the state that produces
	// one is the state where the gate should start watching again: somebody
	// switched the @model back and the alias's pricing is now simply correct.
	for name, reason := range pricingAliasExemptions {
		if !exemptionUsed[name] {
			t.Errorf("provider %s is listed in pricingAliasExemptions but no longer disagrees with the other providers for its model. Remove the entry -- it excuses nothing now, and leaving it there means this gate stops watching %s for real. Recorded reason was: %s", name, name, reason)
		}
	}
}

// samePricing reports whether two providers declare identical cost params.
func samePricing(a, b providerPricing) bool {
	if len(a.costs) != len(b.costs) {
		return false
	}
	for k, v := range a.costs {
		if other, ok := b.costs[k]; !ok || other != v {
			return false
		}
	}
	return true
}

// TestPricedProvidersPriceBothDirections.
//
// A provider that declares an input cost and no output cost (or the reverse)
// reports a total that is wrong by however much the missing half would have
// been -- and reports it as a NUMBER, which reads as measured rather than
// missing. Zero is a real value here (a free tier), so the check is on presence
// rather than on non-zero: what must not happen is one direction being priced
// and the other simply absent.
func TestPricedProvidersPriceBothDirections(t *testing.T) {
	for _, p := range parseProviders(t) {
		_, in := p.costs["inputCostPerMillion"]
		_, out := p.costs["outputCostPerMillion"]
		if in != out {
			missing, present := "outputCostPerMillion", "inputCostPerMillion"
			if !in {
				missing, present = present, missing
			}
			t.Errorf("provider %s declares %s but not %s. A half-priced provider reports a total that is understated by the missing half, as a number, which reads as measured rather than absent.",
				p.name, present, missing)
		}
	}
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}
