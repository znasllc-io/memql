package worker

import "fmt"

// ActionContracts identifies the request semantics a worker implements. Exact
// matching prevents an old worker silently ignoring execution-critical fields;
// a future version is not assumed compatible either. This reports the binary,
// independently of the owner's consent to a particular action or repository.
type ActionContracts map[string]int

func (c ActionContracts) Supports(action string, version int) bool {
	return version > 0 && c[action] == version
}
func (c ActionContracts) validate() error {
	if len(c) > maxCapabilityDescriptorActions {
		return fmt.Errorf("too many action contracts")
	}
	for action, version := range c {
		if !capabilityNamePattern.MatchString(action) || version < 1 || version > 65535 {
			return fmt.Errorf("invalid action contract for %q", action)
		}
	}
	return nil
}
func ActionContractsFromMap(value any) ActionContracts {
	m, _ := value.(map[string]any)
	descriptor := capabilityDescriptorFromMap(m)
	if descriptor == nil {
		return nil
	}
	return descriptor.ActionContracts
}
