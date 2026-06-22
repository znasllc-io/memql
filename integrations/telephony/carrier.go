// Package telephony provides carrier-agnostic PSTN number + trunk management
// for memQL voice agents. A phone call is a LiveKit room participant; this
// package owns the carrier (SIP trunk) side of the edge: programmatic DID
// search / purchase / inbound-routing, abstracted behind CarrierProvider so a
// first carrier (Telnyx) can be swapped for another with one new package and
// no changes to callers.
//
// Telephony is CORE and product-agnostic: numbers and calls bind to a
// generic partition (memql.PartitionScope), never to a CoPresent space.
package telephony

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// NumberType classifies a DID by origination class. It drives both search
// filters and per-minute cost (toll-free origination is dearer than local).
type NumberType string

const (
	NumberTypeLocal    NumberType = "local"
	NumberTypeTollFree NumberType = "tollfree"
	NumberTypeMobile   NumberType = "mobile"
)

// Number is a carrier-agnostic view of a DID. Carrier-specific identifiers
// (order ids, connection ids) are carried opaquely in ProviderID so a caller
// never has to know the carrier's data model.
type Number struct {
	// ProviderID is the carrier's stable identifier for the number (the id
	// BuyNumber consumes for purchase, and ReleaseNumber/ConfigureInbound
	// resolve against). Opaque to callers.
	ProviderID string `json:"providerId"`
	// E164 is the canonical +<countrycode><number> form, e.g. "+14155550100".
	E164 string `json:"e164"`
	// Carrier is the provider name that owns this number ("telnyx", ...).
	Carrier string `json:"carrier"`
	// Type is the origination class (local / tollfree / mobile).
	Type NumberType `json:"type"`
	// Region is a human locality hint (area code, rate center, or country).
	Region string `json:"region"`
	// MonthlyCost is the recurring per-number cost in USD, when the carrier
	// reports it (0 when unknown).
	MonthlyCost float64 `json:"monthlyCost"`
	// Capabilities lists the features the number supports ("voice", "sms").
	Capabilities []string `json:"capabilities,omitempty"`
}

// NumberQuery filters an available-number search. Zero-valued fields are
// unconstrained; Limit defaults to a carrier-chosen page size when 0.
type NumberQuery struct {
	// CountryCode is the ISO-3166 alpha-2 country, e.g. "US". Defaults to "US".
	CountryCode string
	// AreaCode is the national destination / area code, e.g. "415".
	AreaCode string
	// Contains restricts to numbers containing this digit substring.
	Contains string
	// Type restricts by origination class. Empty matches local.
	Type NumberType
	// Limit caps the result count.
	Limit int
}

// CarrierProvider is the carrier-agnostic contract for DID lifecycle and
// inbound routing. Each carrier is exactly one implementation; adding a
// carrier never touches callers.
type CarrierProvider interface {
	// Name returns the carrier's stable identifier ("telnyx", "skyetel", ...).
	Name() string
	// SearchNumbers returns purchasable DIDs matching the query.
	SearchNumbers(ctx context.Context, q NumberQuery) ([]Number, error)
	// BuyNumber purchases the number identified by providerID (a value
	// returned in Number.ProviderID by SearchNumbers) and returns the owned
	// Number.
	BuyNumber(ctx context.Context, providerID string) (Number, error)
	// ReleaseNumber relinquishes an owned DID by its E.164 address.
	ReleaseNumber(ctx context.Context, e164 string) error
	// ConfigureInbound points an owned DID at our SIP edge so inbound PSTN
	// calls route to livekit/sip. sipEdgeURI is the carrier-reachable SIP
	// signaling target (e.g. "sip:edge.example.com:5061;transport=tls").
	ConfigureInbound(ctx context.Context, e164, sipEdgeURI string) error
	// ListNumbers returns every DID currently owned on this carrier.
	ListNumbers(ctx context.Context) ([]Number, error)
}

// Factory builds a CarrierProvider. It is resolved lazily (at carrier
// selection) so credential resolution can fail loudly at first use rather
// than at process start.
type Factory func() (CarrierProvider, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// DefaultCarrier is selected when MEMQL_TELEPHONY_CARRIER is unset.
const DefaultCarrier = "telnyx"

// RegisterCarrier registers a carrier factory under name. Duplicate
// registration of the same name panics — it is always a build-time bug.
func RegisterCarrier(name string, f Factory) {
	if name == "" || f == nil {
		panic("telephony: RegisterCarrier requires a non-empty name and factory")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("telephony: carrier %q already registered", name))
	}
	registry[name] = f
}

// SelectCarrier builds the carrier named by `name`, falling back to
// DefaultCarrier when name is empty. Returns a typed error naming the
// available carriers when the requested one is unknown.
func SelectCarrier(name string) (CarrierProvider, error) {
	if name == "" {
		name = DefaultCarrier
	}
	registryMu.RLock()
	f, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("telephony: unknown carrier %q (available: %v)", name, Carriers())
	}
	return f()
}

// Carriers returns the sorted list of registered carrier names.
func Carriers() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// resetRegistryForTest clears the registry. Test-only.
func resetRegistryForTest() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Factory{}
}
