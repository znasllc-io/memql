package memql

// integration_executor_audit.go -- load-time validation of
// `@executor("integration.X.Y")` builtins (memql#3614 item 6).
//
// `initBuiltinExecutorHandlers` refuses an unknown NATIVE executor loudly, but
// skipped everything with an `integration.` prefix outright -- 70 builtins
// across 26 integrations bypassing the gate. A typo could not be caught before
// somebody called it, and then only as `unknown builtin executor` at the call
// site, deep inside a tool loop.
//
// The obvious fix -- resolve every integration executor against the registry
// and fail boot on a miss -- would brick most of the fleet. Every binary loads
// the SAME DSL tree while registering only the integrations its build tags
// compile in, so a planner node legitimately holds no `integration.cognition.*`
// handler. A hard failure there is not a caught typo, it is an outage.
//
// So the audit splits the question into the part that is decidable per-binary
// and the part that is not:
//
//	provider registered, capability missing  -> ERROR. The integration IS in
//	   this binary and does not offer that capability. A provider's
//	   Capabilities() list carries no build tags anywhere in the tree, so this
//	   is a typo in every build, and the binary that can see it is the one that
//	   should say so.
//
//	provider not registered at all           -> WARN. Indistinguishable, from
//	   inside this process, between "the build tags left it out" (normal, on
//	   every node) and "the integration name is misspelled". Aggregated into a
//	   single warning naming every affected builtin, because one line per
//	   builtin on every boot is noise that gets filtered, and filtered warnings
//	   are the same as no warning.
//
// The shape check (`integration.<name>.<capability>`, both segments non-empty)
// is build-tag-independent and lives in initBuiltinExecutorHandlers, where it
// runs before any integration has had a chance to register.

import (
	"fmt"
	"sort"
	"strings"
)

const integrationExecutorPrefix = "integration."

// validateIntegrationExecutorShape checks that executor is a well-formed
// integration FQN: exactly `integration.<name>.<capability>` with both
// segments non-empty. No registry consulted -- a malformed FQN is wrong in
// every binary.
func validateIntegrationExecutorShape(executor string) error {
	rest, ok := strings.CutPrefix(executor, integrationExecutorPrefix)
	if !ok {
		return fmt.Errorf("executor %q is not an integration executor", executor)
	}
	name, capability, found := strings.Cut(rest, ".")
	if !found {
		return fmt.Errorf("declares malformed integration executor %q: expected integration.<integration>.<capability>, found no capability segment", executor)
	}
	if name == "" {
		return fmt.Errorf("declares malformed integration executor %q: the integration name segment is empty", executor)
	}
	if capability == "" {
		return fmt.Errorf("declares malformed integration executor %q: the capability segment is empty", executor)
	}
	if strings.Contains(capability, ".") {
		return fmt.Errorf("declares malformed integration executor %q: expected exactly integration.<integration>.<capability>, found %d extra segment(s)", executor, strings.Count(capability, "."))
	}
	return nil
}

// integrationExecutorName returns the integration-name segment of a
// well-formed integration FQN.
func integrationExecutorName(executor string) string {
	rest, ok := strings.CutPrefix(executor, integrationExecutorPrefix)
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, ".")
	return name
}

// AuditIntegrationExecutors resolves every enabled `integration.*` builtin
// against the integrations registered in THIS binary.
//
// Call it once the integrations phase has completed -- earlier and every
// executor looks unregistered. Returns an error naming every builtin whose
// integration IS present but does not offer the named capability; logs one
// aggregated warning for the builtins whose integration is absent from this
// build.
func (e *MemQLEngine) AuditIntegrationExecutors() error {
	if e == nil || e.functions == nil {
		return nil
	}

	registeredProviders := map[string]bool{}
	if e.integrations != nil {
		for _, name := range e.integrations.ProviderNames() {
			registeredProviders[name] = true
		}
	}

	type miss struct {
		builtin  string
		executor string
	}
	var typos []miss
	var absent []miss

	for _, fn := range e.functions.Snapshot() {
		if fn == nil || !fn.IsBuiltin() || !fn.Enabled {
			continue
		}
		if !strings.HasPrefix(fn.Executor, integrationExecutorPrefix) {
			continue
		}
		if e.integrations != nil {
			if _, ok := e.integrations.Get(fn.Executor); ok {
				continue
			}
		}
		integration := integrationExecutorName(fn.Executor)
		if registeredProviders[integration] {
			typos = append(typos, miss{builtin: fn.Name, executor: fn.Executor})
			continue
		}
		absent = append(absent, miss{builtin: fn.Name, executor: fn.Executor})
	}

	// Logger is promoted through the embedded *component.Component, so the
	// Component nil-check is load-bearing on a Component-less engine (#2674).
	if len(absent) > 0 && e.Component != nil && e.Logger != nil {
		sort.Slice(absent, func(i, j int) bool { return absent[i].executor < absent[j].executor })
		lines := make([]string, 0, len(absent))
		integrations := map[string]bool{}
		for _, m := range absent {
			lines = append(lines, m.builtin+" -> "+m.executor)
			integrations[integrationExecutorName(m.executor)] = true
		}
		names := make([]string, 0, len(integrations))
		for n := range integrations {
			names = append(names, n)
		}
		sort.Strings(names)
		e.Logger.Warn("builtins declare integration executors this binary does not register -- calling one fails at run time with `unknown builtin executor`",
			"builtins", len(absent),
			"integrations", strings.Join(names, ", "),
			"expected", "normal when the build tags exclude the integration; a misspelled integration name is indistinguishable from here, so check this list against app/plugins_core.go and the build-tagged app/integrations_*.go for this node type",
			"detail", strings.Join(lines, "; "))
	}

	if len(typos) > 0 {
		sort.Slice(typos, func(i, j int) bool { return typos[i].executor < typos[j].executor })
		var b strings.Builder
		for i, m := range typos {
			if i > 0 {
				b.WriteString("\n")
			}
			integration := integrationExecutorName(m.executor)
			available := ""
			if e.integrations != nil {
				var offered []string
				for _, fqn := range e.integrations.List() {
					if integrationExecutorName(fqn) == integration {
						offered = append(offered, fqn)
					}
				}
				if len(offered) > 0 {
					available = " -- " + integration + " offers: " + strings.Join(offered, ", ")
				}
			}
			fmt.Fprintf(&b, "  - builtin %q declares executor %q, but integration %q IS registered in this binary and has no such capability%s",
				m.builtin, m.executor, integration, available)
		}
		return fmt.Errorf("integration executor audit failed: %d builtin(s) name a capability their (present) integration does not offer\n%s", len(typos), b.String())
	}

	return nil
}
