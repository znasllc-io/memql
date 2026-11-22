package memql

// A vendor's text record says what it is FOR (design record
// docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// R1): the levels it serves and its rank among the records that serve them.
//
//	params {
//	  levels "strong reasoning"
//	  rank   800
//	}
//
// WHY. "The strongest vendor model" had no meaning: no record said how strong
// it was or which job it was for, so the selector returned records in name
// order and Haiku was tried before Opus. A level and a rank are the two facts
// a selector needs, and a record is where they belong: they are facts about
// the model, set where its price and window already are.
//
// THE RULES, checked when the record is loaded (validateProviderJob):
//
//   - `levels` is one to three DISTINCT words from fast, strong and reasoning,
//     separated by spaces. Any other word, a repeated word, an empty value, or
//     `embeddings` refuses the record. The embedding model is the cluster's,
//     bound on its own, and no selector chooses one.
//   - `rank` is a whole number from 1 to 1000. Higher is stronger.
//   - Either param on a record that is not a text model of Anthropic or OpenAI
//     refuses the record: it would declare a job no selector reads.
//   - A record that declares neither is legal. It resolves by name as it
//     always did, and the vendor selectors pass it over.
//
// A refused record is reported as a skip on the load report, the way the
// loader reports any record it cannot use, and is not registered.
//
// Regression tests: provider_job_test.go.

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/num"
)

const (
	providerParamLevels = "levels"
	providerParamRank   = "rank"

	minProviderRank = 1
	maxProviderRank = 1000
)

// jobLevels are the levels a vendor text record may declare, in the order
// Levels reports them.
var jobLevels = []airoute.Level{airoute.LevelFast, airoute.LevelStrong, airoute.LevelReasoning}

// Levels returns the levels this record declares it serves, in the order fast,
// strong, reasoning. Nil when it declares none. A word that is not one of the
// three is not reported: a record carrying one is refused when it is loaded.
func (c ProviderConfig) Levels() []airoute.Level {
	raw, ok := c.Params[providerParamLevels].(string)
	if !ok {
		return nil
	}
	declared := map[airoute.Level]bool{}
	for _, word := range strings.Fields(raw) {
		declared[airoute.Level(strings.ToLower(word))] = true
	}
	var out []airoute.Level
	for _, level := range jobLevels {
		if declared[level] {
			out = append(out, level)
		}
	}
	return out
}

// ServesLevel reports whether this record declares the level.
func (c ProviderConfig) ServesLevel(l airoute.Level) bool {
	for _, level := range c.Levels() {
		if level == l {
			return true
		}
	}
	return false
}

// Rank returns the record's declared rank, or 0 when it declares none. Higher
// is stronger. A value outside 1 to 1000 is refused when the record is loaded.
func (c ProviderConfig) Rank() int {
	rank, _ := wholeParam(c.Params[providerParamRank])
	return rank
}

// wholeParam reads a param that must be a whole number. A fraction, an
// infinity and a NaN are not one.
//
// narrowing: SATURATE -- a rank is an ORDERING, and its one validator
// (validateProviderJob) refuses anything outside 1 to 1000. A number too large
// for an int must therefore stay too large and be refused for what it is; a
// bare conversion would hand back the platform's indefinite value, a huge
// negative, and the record would be refused as "below 1" or, read by Rank
// alone, sorted last for a reason nobody wrote.
func wholeParam(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int64:
		return num.ClampInt64(x), true
	case float64:
		if x != math.Trunc(x) || math.IsInf(x, 0) || math.IsNaN(x) {
			return 0, false
		}
		return num.ClampFloat64(x), true
	}
	return 0, false
}

// vendorOfType names the vendor a record's type belongs to, for the types
// whose text models an account pays for. "" for every other type.
func vendorOfType(providerType string) string {
	switch strings.ToLower(strings.TrimSpace(providerType)) {
	case "anthropic", "anthropicchat":
		return VendorAnthropic
	case "openai", "openaichat":
		return VendorOpenAI
	}
	return ""
}

// isVendorTextRecord reports whether a record, by what it declares, is a text
// model of Anthropic or OpenAI: the records the vendor selectors choose among.
// It reads the declaration only, so it answers before any client is built.
func isVendorTextRecord(cfg ProviderConfig) bool {
	return !cfg.Base && cfg.ResolvedModality() == ModalityText && vendorOfType(cfg.Type) != ""
}

// validateProviderJob checks the two params a record declares its job with.
// cfg must already carry the type it inherits from its base.
func validateProviderJob(cfg ProviderConfig) error {
	levels, hasLevels := cfg.Params[providerParamLevels]
	rank, hasRank := cfg.Params[providerParamRank]
	if !hasLevels && !hasRank {
		return nil
	}
	refuse := func(param, format string, args ...any) error {
		return fmt.Errorf("provider %q: params.%s %s", cfg.Name, param, fmt.Sprintf(format, args...))
	}
	if !isVendorTextRecord(cfg) {
		param := providerParamLevels
		if !hasLevels {
			param = providerParamRank
		}
		return refuse(param, "is set on a record that is not a text model of Anthropic or OpenAI, "+
			"so no selector would read it. Remove it")
	}

	if hasLevels {
		raw, ok := levels.(string)
		if !ok {
			return refuse(providerParamLevels, "must be words in quotes, one or more of fast, strong and reasoning, separated by spaces")
		}
		words := strings.Fields(raw)
		if len(words) == 0 {
			return refuse(providerParamLevels, "is empty. Name one or more of fast, strong and reasoning, or leave the param out")
		}
		seen := map[string]bool{}
		for _, word := range words {
			lower := strings.ToLower(word)
			switch {
			case airoute.Level(lower) == airoute.LevelEmbeddings:
				return refuse(providerParamLevels, "names %q. The embedding model is the cluster's and is bound on its own; "+
					"a text record serves fast, strong or reasoning", word)
			case !isJobLevel(lower):
				return refuse(providerParamLevels, "names %q, which is not a level. Use fast, strong or reasoning", word)
			case seen[lower]:
				return refuse(providerParamLevels, "names %q twice", word)
			}
			seen[lower] = true
		}
	}
	if hasRank {
		value, whole := wholeParam(rank)
		if !whole || value < minProviderRank || value > maxProviderRank {
			return refuse(providerParamRank, "must be a whole number from %d to %d, where higher is stronger", minProviderRank, maxProviderRank)
		}
	}
	return nil
}

func isJobLevel(word string) bool {
	for _, level := range jobLevels {
		if string(level) == word {
			return true
		}
	}
	return false
}

// The two questions a vendor selector asks of the catalog (design record R2).
const (
	// VendorSelectorStrongest orders by declared rank, highest first.
	VendorSelectorStrongest = "strongest"
	// VendorSelectorCheapest orders by declared input plus output price per
	// million tokens, lowest first. A record with no price is last.
	VendorSelectorCheapest = "cheapest"
)

// VendorRecordsFor orders the vendor text records for one selector at one
// level (design record R2): the answer to `federation:strongest` and
// `federation:cheapest` for a call that asks for that level.
//
// THE SET is every registered text model of Anthropic or OpenAI that DECLARES
// the level. Whether the cluster has an account for a record is not asked
// here: a record the cluster cannot pay for may be paid by a person's API key,
// and the walk asks each candidate's accounts in order. Neither are the
// context floor and the modality: the selector orders, the walk filters.
//
// THE ORDER. strongest: highest rank first. cheapest: lowest input plus output
// price first, a record with no price last, because an unpriced record read as
// zero would be the cheapest thing in the catalog. Every tie is broken by
// name, so two replicas holding the same catalog answer alike whatever order
// their records were registered in.
//
// passedOver names the vendor text records left out and why: one that declares
// no level, and one that serves other levels. A record that is not a vendor's
// text model is neither chosen nor named; a selector was never about it.
//
// A level no vendor text record may declare (embeddings, or none) answers
// nothing and names nothing.
func (r *ProviderRegistry) VendorRecordsFor(level airoute.Level, selector string) (ordered []string, passedOver map[string]string) {
	if r == nil || !isJobLevel(string(level)) {
		return nil, nil
	}
	type record struct {
		name    string
		rank    int
		price   float64
		unknown bool
	}
	var chosen []record
	passedOver = map[string]string{}
	r.mu.RLock()
	for name, e := range r.byName {
		if e == nil || !isVendorTextRecord(e.Config) {
			continue
		}
		declared := e.Config.Levels()
		switch {
		case len(declared) == 0:
			passedOver[name] = "declares no level, so a vendor selector does not choose it"
		case !e.Config.ServesLevel(level):
			words := make([]string, 0, len(declared))
			for _, l := range declared {
				words = append(words, string(l))
			}
			passedOver[name] = fmt.Sprintf("serves %s, not %s", strings.Join(words, " and "), level)
		default:
			p := e.Config.Pricing()
			chosen = append(chosen, record{
				name:    name,
				rank:    e.Config.Rank(),
				price:   p.InputPerMillion + p.OutputPerMillion,
				unknown: p.InputPerMillion <= 0 && p.OutputPerMillion <= 0,
			})
		}
	}
	r.mu.RUnlock()

	sort.Slice(chosen, func(i, j int) bool {
		a, b := chosen[i], chosen[j]
		if selector == VendorSelectorCheapest {
			if a.unknown != b.unknown {
				return !a.unknown
			}
			if a.price != b.price {
				return a.price < b.price
			}
			return a.name < b.name
		}
		if a.rank != b.rank {
			return a.rank > b.rank
		}
		return a.name < b.name
	})
	ordered = make([]string, 0, len(chosen))
	for _, c := range chosen {
		ordered = append(ordered, c.name)
	}
	return ordered, passedOver
}
