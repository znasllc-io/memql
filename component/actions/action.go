// Package actions is the registry + runtime for AUTHORED actions
// (memql#2218, behavioral-constructs ADR §2.3).
//
// An authored action is the DSL's external-side-effect primitive: it names
// exactly ONE external capability (shell.* / fs.* / http.* / integration.* /
// mcp.*), declares a typed input schema (params), and renders an argTemplate
// from those params. It never touches the MemQL graph. At run time an
// automation `action("name@1")` step binds the step args to the action's
// params, renders the argTemplate, and invokes the single capability on the
// resolved surface -- token-free and fingerprint-verifiable on identical
// input.
//
// This package is deliberately a leaf: it depends only on the language
// frontend (parser + ast) and the embedded DSL tree, so the loader/registry
// stay reusable from the engine, the step executor, and tooling without an
// import cycle. The capability backend (shell/fs/http/integration dispatch)
// lives next to the step executor (component/automations/steps), not here --
// keeping this package free of the engine.
//
// The replay-CAPTURE action machinery (component/harness/action*, the
// `v1:actions:action` graph rows) is a SEPARATE model kept for the library /
// replay use-case; this package is only the AUTHORED path.
package actions

// Param is one declared input of an authored action (a params-block field).
type Param struct {
	Name        string
	Type        string // author-surface type: "string", "object", "[]string", ...
	Required    bool
	Description string
}

// ArgEntry is one rendered capability argument (an argTemplate entry):
// Key is the capability argument name, Template is the raw template string
// ("$params.<name>" references are substituted from bound params at render).
type ArgEntry struct {
	Key      string
	Template string
}

// Action is a loaded, registered authored action.
type Action struct {
	Name        string // declaration name; the reference name in "name@version"
	Version     int    // authored actions are version 1 today (pin-by-default)
	Capability  string // the single external capability verb
	Intent      string // natural-language description (future embedding source)
	Kind        string // "primitive" (the only authored kind today)
	SideEffect  string // "read" | "write" | "exec" | "" (from @sideEffect; authoritative class lives on the capability, ADR §7)
	Params      []Param
	ArgTemplate []ArgEntry
	Description string // from @description
	Enabled     bool   // @disabled -> false; default true
	Origin      string // source path:name, for diagnostics
}

// paramByName returns the declared param of the given name, or false.
func (a *Action) paramByName(name string) (Param, bool) {
	for _, p := range a.Params {
		if p.Name == name {
			return p, true
		}
	}
	return Param{}, false
}
