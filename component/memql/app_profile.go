package memql

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// app_profile.go -- loader for the ACTIVE app profile (the one the product
// pack registered via RegisterAppProfile in app_profile_registry.go). The
// engine embeds no profile content of its own; content comes from the pack's
// registration, with an optional on-disk override for deployment setups that
// mount profiles from a volume (ops push / per-customer branding / etc.).

// appProfileCache is a small in-process cache so we don't hit disk on
// every operator turn. Keys are the app-profile name. Values are the full
// profile markdown. Cache is populated on first read and never invalidated
// -- profiles change with deploys, not at runtime. Clear by restarting.
var (
	appProfileCache   = map[string]string{}
	appProfileCacheMu sync.RWMutex
)

// appProfilesDir points at the directory holding app-profile markdown
// files when `MEMQL_OPERATOR_APP_PROFILES_DIR` is set. The on-disk file
// (<dir>/<name>.md) wins over the pack-registered content.
func appProfilesDir() string {
	return strings.TrimSpace(os.Getenv("MEMQL_OPERATOR_APP_PROFILES_DIR"))
}

// LoadActiveAppProfile resolves the registered app profile and its content.
// Returns (zero, "", false) when no pack registered one -- the caller
// proceeds without the block (an app without a profile just means the agent
// falls back to pure manifest + uiState discovery). Content resolution:
// the MEMQL_OPERATOR_APP_PROFILES_DIR override when set and readable, else
// the registration's Content().
func LoadActiveAppProfile() (AppProfile, string, bool) {
	p, ok := ActiveAppProfile()
	if !ok {
		return AppProfile{}, "", false
	}
	return p, loadAppProfileContent(p), true
}

// loadAppProfileContent resolves + caches the profile markdown. Name
// validation is strict: only [a-z0-9-] to prevent directory traversal;
// names longer than 64 chars are rejected (returns "").
func loadAppProfileContent(p AppProfile) string {
	name := strings.TrimSpace(p.Name)
	if name == "" || len(name) > 64 {
		return ""
	}
	for _, r := range name {
		isLetter := r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		isDash := r == '-'
		if !isLetter && !isDigit && !isDash {
			return ""
		}
	}

	appProfileCacheMu.RLock()
	if cached, ok := appProfileCache[name]; ok {
		appProfileCacheMu.RUnlock()
		return cached
	}
	appProfileCacheMu.RUnlock()

	profile := ""
	if dir := appProfilesDir(); dir != "" {
		content, err := os.ReadFile(filepath.Join(dir, name+".md"))
		if err == nil {
			profile = strings.TrimSpace(string(content))
		}
	}
	if profile == "" && p.Content != nil {
		profile = strings.TrimSpace(p.Content())
	}

	// Cache the resolution (hit or miss) so repeated turns don't spam
	// the filesystem.
	appProfileCacheMu.Lock()
	appProfileCache[name] = profile
	appProfileCacheMu.Unlock()
	return profile
}
