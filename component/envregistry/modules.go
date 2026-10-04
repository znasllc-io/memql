package envregistry

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// A readiness module: a named set of lanes over registry entries, or a named
// evaluator (design record 2026-09-06-configuration-readiness, section 4.2).
type Module struct {
	Name string `yaml:"name"`
	Core bool   `yaml:"core,omitempty"`
	// Optional marks a module nothing needs: a feature an owner may choose to
	// set up (pipelines program design record, D15). Never core -- the
	// first-run wizard walks core modules, and an optional one is by
	// definition not a step there.
	Optional bool `yaml:"optional,omitempty"`
	// Dismissable lets a person answer "Not now" to an optional module's
	// setup prompt. It requires Optional: a module something needs cannot be
	// waved away.
	Dismissable bool     `yaml:"dismissable,omitempty"`
	Description string   `yaml:"description"`
	Evaluator   string   `yaml:"evaluator,omitempty"`
	HostedBy    HostedBy `yaml:"hostedBy,omitempty"`
	Lanes       []Lane   `yaml:"lanes,omitempty"`
}

// HostedBy names the nodes that report a verdict for a module. Both lists
// empty means every node evaluates it.
type HostedBy struct {
	Integrations []string `yaml:"integrations,omitempty"`
	NodeTypes    []string `yaml:"nodeTypes,omitempty"`
}

// Lane is one way to configure a module: complete when every slot is present.
type Lane struct {
	Name             string   `yaml:"name"`
	ConfigurableFrom string   `yaml:"configurableFrom"`
	Slots            []string `yaml:"slots"`
	OptionalSlots    []string `yaml:"optionalSlots,omitempty"`
}

const (
	ConfigurableFromOS         = "os"
	ConfigurableFromDeployment = "deployment"
	// EvaluatorInferenceStatus asks the provider registry (any door open).
	EvaluatorInferenceStatus = "inferenceStatus"
	// EvaluatorIntegrationPrefix asks integration.<name>.status in-process.
	EvaluatorIntegrationPrefix = "integration:"
)

// Everywhere reports whether the module carries no hostedBy restriction.
func (h HostedBy) Everywhere() bool {
	return len(h.Integrations) == 0 && len(h.NodeTypes) == 0
}

// strictDocument decodes the whole file with unknown keys REFUSED inside the
// modules block. The entry lists are typed as maps so their own keys are not
// judged here; LoadManifestFromBytes stays lenient for them, which is the
// unshipped C5 of the integration-config record, deliberately not widened by
// this file.
type strictDocument struct {
	Secrets   []map[string]any `yaml:"secrets"`
	Variables []map[string]any `yaml:"variables"`
	Modules   []Module         `yaml:"modules"`
}

// DecodeModulesStrict decodes the modules block refusing unknown keys.
func DecodeModulesStrict(data []byte) ([]Module, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc strictDocument
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("manifest modules: %w", err)
	}
	return doc.Modules, nil
}

// IsSecret reports whether name is declared under `secrets`.
func (m *Manifest) IsSecret(name string) bool {
	for _, e := range m.Secrets {
		if e.Name == name {
			return true
		}
	}
	return false
}

// Module returns the declared module by name.
func (m *Manifest) Module(name string) (Module, bool) {
	for _, mod := range m.Modules {
		if mod.Name == name {
			return mod, true
		}
	}
	return Module{}, false
}

// ValidateModules refuses a declaration the evaluator could not act on: a
// nameless or duplicate module, a module with both lanes and an evaluator or
// neither, an unknown evaluator, a lane with an unknown configurableFrom, and
// a slot naming no registry entry. The last is the one that matters most: a
// typo in a slot would otherwise read as a module nobody can ever configure.
//
// It also refuses the two flag combinations that contradict themselves:
// dismissable without optional (a module something needs cannot be waved
// away) and optional with core (the first-run wizard would walk a module the
// manifest says nobody needs).
func (m *Manifest) ValidateModules() error {
	seen := map[string]bool{}
	for _, mod := range m.Modules {
		name := strings.TrimSpace(mod.Name)
		if name == "" {
			return fmt.Errorf("manifest modules: a module has no name")
		}
		if seen[name] {
			return fmt.Errorf("manifest modules: module %q is declared twice", name)
		}
		seen[name] = true
		if strings.TrimSpace(mod.Description) == "" {
			return fmt.Errorf("manifest modules: module %q has no description; the setup surface renders it", name)
		}
		if mod.Optional && mod.Core {
			return fmt.Errorf("manifest modules: module %q is both optional and core; the first-run wizard walks every core module, so a module nothing needs cannot be one", name)
		}
		if mod.Dismissable && !mod.Optional {
			return fmt.Errorf("manifest modules: module %q is dismissable and not optional; only a module nothing needs may be dismissed", name)
		}
		hasEval := strings.TrimSpace(mod.Evaluator) != ""
		if hasEval == (len(mod.Lanes) > 0) {
			return fmt.Errorf("manifest modules: module %q must declare lanes or an evaluator, not both and not neither", name)
		}
		if hasEval && mod.Evaluator != EvaluatorInferenceStatus &&
			!strings.HasPrefix(mod.Evaluator, EvaluatorIntegrationPrefix) {
			return fmt.Errorf("manifest modules: module %q names unknown evaluator %q", name, mod.Evaluator)
		}
		lanes := map[string]bool{}
		for _, lane := range mod.Lanes {
			if strings.TrimSpace(lane.Name) == "" {
				return fmt.Errorf("manifest modules: module %q has a lane with no name", name)
			}
			if lanes[lane.Name] {
				return fmt.Errorf("manifest modules: module %q declares lane %q twice", name, lane.Name)
			}
			lanes[lane.Name] = true
			if lane.ConfigurableFrom != ConfigurableFromOS && lane.ConfigurableFrom != ConfigurableFromDeployment {
				return fmt.Errorf("manifest modules: module %q lane %q: configurableFrom %q is not os or deployment", name, lane.Name, lane.ConfigurableFrom)
			}
			if len(lane.Slots) == 0 {
				return fmt.Errorf("manifest modules: module %q lane %q declares no slots", name, lane.Name)
			}
			slots := map[string]bool{}
			for _, slot := range append(append([]string{}, lane.Slots...), lane.OptionalSlots...) {
				if slots[slot] {
					return fmt.Errorf("manifest modules: module %q lane %q lists slot %q twice", name, lane.Name, slot)
				}
				slots[slot] = true
				if _, ok := m.Lookup(slot); !ok {
					return fmt.Errorf("manifest modules: module %q lane %q names slot %q, which is not a registry entry", name, lane.Name, slot)
				}
			}
		}
	}
	return nil
}
