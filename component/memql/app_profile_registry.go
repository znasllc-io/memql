package memql

import (
	"fmt"
	"sync"
)

// app_profile_registry.go -- the product extension point for the operator
// app profile. A product pack registers, at init() from its registration-
// anchor package, the "app map" markdown the agent replier injects into
// operator-capable turns plus the knowledge-domain ids that ride along with
// it. The engine carries no product profile of its own: a pure-engine binary
// has an empty registry and the replier's injection block silently skips.
// Modeled on the suggest-domain registry (suggest_registry.go, memql#1959).

// AppProfile declares one product's operator app profile.
type AppProfile struct {
	// Name is the profile id and the filename stem consulted for the
	// MEMQL_OPERATOR_APP_PROFILES_DIR on-disk override (<dir>/<Name>.md).
	// Strict [a-z0-9-], max 64 chars.
	Name string
	// Content lazily returns the pack-embedded profile markdown.
	Content func() string
	// OperatorDomainIds are the knowledge-domain ids force-attached to the
	// RAG retrieval set on every operator-enabled turn, and classified as
	// app-structure (system-owned, no audible citation).
	OperatorDomainIds []string
}

var (
	appProfileRegistryMu sync.RWMutex
	activeAppProfile     *AppProfile
)

// RegisterAppProfile installs the product's app profile. Call from init();
// exactly ONE profile per binary is supported (one product per carrier
// image), so a second registration panics -- the same loud programmer-error
// contract as RegisterSuggestDomain.
func RegisterAppProfile(p AppProfile) {
	if p.Name == "" {
		panic("memql.RegisterAppProfile: empty Name")
	}
	if p.Content == nil {
		panic("memql.RegisterAppProfile: nil Content")
	}
	appProfileRegistryMu.Lock()
	defer appProfileRegistryMu.Unlock()
	if activeAppProfile != nil {
		panic(fmt.Sprintf("memql.RegisterAppProfile: %q already registered; one app profile per binary", activeAppProfile.Name))
	}
	cp := p
	activeAppProfile = &cp
}

// ActiveAppProfile returns the registered profile, if any.
func ActiveAppProfile() (AppProfile, bool) {
	appProfileRegistryMu.RLock()
	defer appProfileRegistryMu.RUnlock()
	if activeAppProfile == nil {
		return AppProfile{}, false
	}
	return *activeAppProfile, true
}

// setActiveAppProfileForTest swaps the active profile and returns a restore
// func. Engine tests use it to exercise the seam without a product pack.
func setActiveAppProfileForTest(p *AppProfile) func() {
	appProfileRegistryMu.Lock()
	prev := activeAppProfile
	activeAppProfile = p
	appProfileRegistryMu.Unlock()
	return func() {
		appProfileRegistryMu.Lock()
		activeAppProfile = prev
		appProfileRegistryMu.Unlock()
	}
}
