package memql

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	appprofiles "github.com/znasllc-io/memql/app-profiles"
)

// app_profile.go -- shared app-profile loader.
//
// This used to live in delegate_takeover.go alongside the cross-agent
// `delegateTakeover` handler. That handler (and its single-use record /
// knowledge helpers) was retired with the CoPresent Control v2 split
// (copresent#187: copresent_control -> copresent_takeover + copresent_guide;
// the GA holds both directly, so no specialist->GA hand-off is needed). The
// app-profile loader survives because the agent replier injects the profile
// on every operator-capable turn -- see LoadAppProfileByName below.

// appProfileCache is a small in-process cache so we don't hit disk on
// every operator turn. Keys are the app-profile name (e.g. "copresent").
// Values are the full profile markdown. Cache is populated on first
// read and never invalidated -- profiles change with deploys, not at
// runtime. Clear by restarting.
var (
	appProfileCache   = map[string]string{}
	appProfileCacheMu sync.RWMutex
)

// appProfilesDir points at the directory holding app-profile markdown
// files when `MEMQL_OPERATOR_APP_PROFILES_DIR` is set. Left in place for
// deployment setups that want to override the embedded profiles from
// a mounted volume (ops push / per-customer branding / etc.). In the
// default path we load from the embedded FS (see app-profiles/embed.go).
func appProfilesDir() string {
	return strings.TrimSpace(os.Getenv("MEMQL_OPERATOR_APP_PROFILES_DIR"))
}

// LoadAppProfileByName is the exported form of loadAppProfile.
// Callers outside this package (e.g. the agent replier that injects
// the profile on turns where the acting agent has CoPresent operator
// capability) use this entry point.
func LoadAppProfileByName(name string) string {
	return loadAppProfile(name)
}

// loadAppProfile returns the named app profile, preferring the
// filesystem override (MEMQL_OPERATOR_APP_PROFILES_DIR) when set and
// falling back to the embedded profiles baked into the binary.
// Returns "" on any error (missing file, read failure, invalid name)
// so the caller can proceed without the block -- an app without a
// profile just means the agent falls back to pure manifest + uiState
// discovery.
//
// Name validation is strict: only [a-z0-9-] to prevent directory
// traversal. Names longer than 64 chars are rejected.
func loadAppProfile(name string) string {
	name = strings.TrimSpace(name)
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
	if profile == "" {
		profile = appprofiles.Load(name)
	}

	// Cache the resolution (hit or miss) so repeated turns don't spam
	// the filesystem or embed FS lookup.
	appProfileCacheMu.Lock()
	appProfileCache[name] = profile
	appProfileCacheMu.Unlock()
	return profile
}
