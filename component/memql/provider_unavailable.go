package memql

import (
	"errors"
	"fmt"
)

// ProviderUnavailableError says a named AI provider is not configured on this
// cluster. It is a CONFIGURATION fact, not a failure of the call.
//
// It exists because callers must be able to tell the two apart and could not
// (memql#4693). The planner's goal-complexity triage names an OpenAI provider;
// on a Claude-only cluster every goal produced
//
//	WARN planner triage: classify failed; falling through to decompose loop
//	     ... provider "chat54Mini" not available
//
// which reads like a fault, was logged once per goal forever, and told an
// operator to investigate something that was working as designed. Meanwhile a
// genuine provider outage produces text a caller cannot distinguish from it.
//
// The shape follows the fleet refusal a few lines up in ai_runtime.go, which
// was made typed for the same reason and is matched with errors.As: a Task
// PARKS on an asleep laptop rather than failing the plan. Same principle here
// -- what a caller does about "not configured" is not what it does about
// "broken", so the difference has to survive the return.
type ProviderUnavailableError struct {
	// Provider is the provider name that did not resolve. "(default)" when the
	// call asked for whatever the cluster's default chat provider is and there
	// was none.
	Provider string
}

func (e *ProviderUnavailableError) Error() string {
	return fmt.Sprintf("provider %q not available", e.Provider)
}

// ErrProviderUnavailable wraps the named provider in the typed error.
func ErrProviderUnavailable(provider string) error {
	return &ProviderUnavailableError{Provider: provider}
}

// IsProviderUnavailable reports whether err is (or wraps) a provider that this
// cluster has not configured.
func IsProviderUnavailable(err error) bool {
	var target *ProviderUnavailableError
	return errors.As(err, &target)
}
