package memql

import (
	"sort"
	"strings"
	"sync"
)

// pack_bindings.go associates an integration plugin registration with the
// pack domain that owns it (module-registry design,
// docs/superpowers/specs/2026-08-20-module-registry-design.md section 4.3).
//
// A pack is one domain with two halves registered through two primitives --
// dsl.RegisterTree for the .memql tree and RegisterPlugin* for the Go
// factory -- and before this file nothing linked them. The link matters
// twice:
//
//   - materializePlugins (app/plugins.go) must skip the Go factories of a
//     pack the instance has disabled (v1:platform:packState), and "which
//     factories are the harness pack's" is exactly this table.
//   - the module inventory folds a pack's integrations under its pack row
//     instead of listing them as free-standing integrations.
//
// An UNBOUND plugin is an ordinary integration; binding is opt-in and only
// packs do it. The 27 core RegisterPlugin callers are deliberately not
// touched -- they are integrations, not packs, and a default binding would
// invent pack-ness for them.
//
// Registration shape mirrors the sibling init()-time registries
// (RegisterPlugin, RegisterCapabilitySlug): package-level map, mutex,
// panic on a CONFLICTING duplicate. Re-binding the same (plugin, domain)
// pair is a no-op rather than a panic so a test that re-runs an init path
// stays well-defined; only a binding that would CHANGE an existing answer
// is a programming error worth dying for.

var (
	packBindingsMu sync.RWMutex
	// packBindings maps plugin registration name -> owning pack domain.
	packBindings = map[string]string{}
)

// BindPluginToPack records that the integration plugin registered under
// pluginName belongs to the pack that owns packDomain. Call it from the
// same init() that calls RegisterPlugin / RegisterPluginForContract.
//
// Panics on an empty name/domain or on a binding that conflicts with an
// existing one; re-binding the identical pair is a no-op.
func BindPluginToPack(pluginName, packDomain string) {
	name := strings.TrimSpace(pluginName)
	domain := strings.TrimSpace(packDomain)
	if name == "" {
		panic("memql.BindPluginToPack: plugin name must be non-empty")
	}
	if domain == "" {
		panic("memql.BindPluginToPack: pack domain must be non-empty")
	}
	packBindingsMu.Lock()
	defer packBindingsMu.Unlock()
	if existing, ok := packBindings[name]; ok {
		if existing == domain {
			return
		}
		panic("memql.BindPluginToPack: plugin " + name + " is already bound to pack domain " + existing +
			"; a plugin belongs to exactly one pack")
	}
	packBindings[name] = domain
}

// PackDomainForPlugin returns the pack domain the plugin is bound to, and
// whether a binding exists. An unbound plugin is an ordinary integration.
func PackDomainForPlugin(pluginName string) (string, bool) {
	packBindingsMu.RLock()
	defer packBindingsMu.RUnlock()
	domain, ok := packBindings[strings.TrimSpace(pluginName)]
	return domain, ok
}

// PluginsForPackDomain returns the plugin registration names bound to the
// pack domain, sorted for deterministic reporting. Empty when the pack has
// no Go half (a DSL-only pack is legal).
func PluginsForPackDomain(packDomain string) []string {
	domain := strings.TrimSpace(packDomain)
	packBindingsMu.RLock()
	defer packBindingsMu.RUnlock()
	var out []string
	for name, bound := range packBindings {
		if bound == domain {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// unbindPluginFromPackForTest removes a binding. Test teardown only --
// production bindings are init()-time and permanent, exactly like the
// plugin registrations they annotate.
func unbindPluginFromPackForTest(pluginName string) {
	packBindingsMu.Lock()
	defer packBindingsMu.Unlock()
	delete(packBindings, strings.TrimSpace(pluginName))
}
