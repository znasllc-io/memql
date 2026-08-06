package memql

import "encoding/json"

// secretToolArgFields returns the tool's argument names marked x-secret in its
// own input schema.
//
// Read from tool.InputSchema rather than from the originating function, which
// matters for two reasons. validateToolArgs holds a *Tool and no function
// handle, so reaching back would mean threading the registry through a
// validation path that is otherwise registry-free. And an AUTHORED tool that
// declares a secret argument is covered by the same code, rather than only the
// auto-generated twins memql#3117 was filed about.
//
// Returns nil when the tool declares none, which is the common case and makes
// the redaction a no-op by construction.
func secretToolArgFields(tool *Tool) map[string]bool {
	if tool == nil || len(tool.InputSchema) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]struct {
			Secret bool `json:"x-secret"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
		return nil
	}
	var out map[string]bool
	for name, prop := range schema.Properties {
		if !prop.Secret {
			continue
		}
		if out == nil {
			out = make(map[string]bool, 1)
		}
		out[name] = true
	}
	return out
}

// redactSecretArgValues returns a shallow copy of args with every secret key's
// value replaced.
//
// A COPY, deliberately. args is the same map validateToolArgs' caller
// (streaming.go) hands to ExecuteTool and that coerceArgsToSchema already
// mutates in place -- redacting it in place would destroy the real argument
// before dispatch, turning a log-hygiene fix into a functional bug. The copy is
// shallow because only top-level keys are replaced; a nested value under a
// secret key is dropped wholesale with its parent.
//
// Returns args unchanged when nothing is secret, so the common path allocates
// nothing.
func redactSecretArgValues(args map[string]any, secret map[string]bool) map[string]any {
	if len(secret) == 0 || len(args) == 0 {
		return args
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if secret[k] {
			// The SAME marker memql#3036 uses in function_validator.go. One
			// spelling, so an operator grepping logs for redactions finds both
			// surfaces and a reader does not have to learn two.
			out[k] = redactedArgValue
			continue
		}
		out[k] = v
	}
	return out
}
