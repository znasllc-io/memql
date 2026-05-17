package memql

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/secret"
)

func (e *MemQLEngine) ResolveVariable(ctx context.Context, name string) (string, error) {
	if !e.canResolve() {
		return "", ErrEngineNotInitialized
	}
	if v, err := e.readVariable(ctx, "v1:platform:partitionVariable", name); err == nil {
		return v, nil
	} else if !isNotFoundVariable(err) {
		return "", err
	}
	// Fall back to global.
	return e.readVariable(ctx, "v1:platform:globalVariable", name)
}

// ResolveSystemVariable fetches a plaintext variable value from the
// v1:platform:globalVariable (global) concept. No partition lookup; no
// fallback. Use this for instance-wide config.
func (e *MemQLEngine) ResolveSystemVariable(ctx context.Context, name string) (string, error) {
	if !e.canResolve() {
		return "", ErrEngineNotInitialized
	}
	return e.readVariable(ctx, "v1:platform:globalVariable", name)
}

// ResolveSecret fetches an encrypted secret value from the
// v1:platform:partitionSecret (partition-scoped) concept, falling back to
// v1:platform:globalSecret (global). The returned value is the decrypted
// plaintext -- callers must not log or persist it. Requires
// MEMQL_MASTER_KEY to be set.
func (e *MemQLEngine) ResolveSecret(ctx context.Context, name string) (string, error) {
	if !e.canResolve() {
		return "", ErrEngineNotInitialized
	}
	if v, err := e.readSecret(ctx, "v1:platform:partitionSecret", name); err == nil {
		return v, nil
	} else if !isNotFoundVariable(err) {
		return "", err
	}
	return e.readSecret(ctx, "v1:platform:globalSecret", name)
}

// ResolveSystemSecret fetches an encrypted secret value from the
// v1:platform:globalSecret (global) concept. No partition lookup; no
// fallback. Returns decrypted plaintext.
func (e *MemQLEngine) ResolveSystemSecret(ctx context.Context, name string) (string, error) {
	if !e.canResolve() {
		return "", ErrEngineNotInitialized
	}
	return e.readSecret(ctx, "v1:platform:globalSecret", name)
}

// canResolve reports whether the engine has enough wiring to run a
// concept lookup. Resolvers run during provider load (Phase 4b of the
// env-var refactor): the database is up and concepts are registered,
// but `e.initialized=true` hasn't fired yet. Allow the read in that
// in-between window so providers can pull their auth.apiKey from
// v1:platform:globalSecret instead of the OS env. Returns false only if the
// DB itself isn't connected.
func (e *MemQLEngine) canResolve() bool {
	if e == nil {
		return false
	}
	if e.initialized {
		return true
	}
	return e.database() != nil && e.concepts != nil
}

// ResolveVariableWithDefault fetches a variable value, returning the
// default if not found or if any lookup error occurs. Follows the same
// partition-first-then-global order as ResolveVariable.
func (e *MemQLEngine) ResolveVariableWithDefault(ctx context.Context, name, defaultValue string) string {
	value, err := e.ResolveVariable(ctx, name)
	if err != nil {
		return defaultValue
	}
	return value
}

// readVariable performs the common lookup against a *:variable concept
// (either v1:platform:partitionVariable or v1:platform:globalVariable). Returns the
// stringified payload.value.
func (e *MemQLEngine) readVariable(ctx context.Context, conceptName, name string) (string, error) {
	fields, err := e.readNamedRowFields(ctx, conceptName, name, "variable")
	if err != nil {
		return "", err
	}
	valueField, ok := fields["value"]
	if !ok || valueField == nil {
		return "", fmt.Errorf("variable %q has no value field", name)
	}
	return valueField.GetStringValue(), nil
}

// readSecret performs the common lookup against a *:secret concept
// (v1:platform:partitionSecret or v1:platform:globalSecret). Reads payload.encryptedValue
// and returns the decrypted plaintext via the secret package. Requires
// MEMQL_MASTER_KEY to be set.
func (e *MemQLEngine) readSecret(ctx context.Context, conceptName, name string) (string, error) {
	fields, err := e.readNamedRowFields(ctx, conceptName, name, "secret")
	if err != nil {
		return "", err
	}
	ctField, ok := fields["encryptedValue"]
	if !ok || ctField == nil {
		return "", fmt.Errorf("secret %q has no encryptedValue field", name)
	}
	ct := ctField.GetStringValue()
	plaintext, err := secret.Decrypt(ct)
	if err != nil {
		return "", fmt.Errorf("secret %q: %w", name, err)
	}
	return plaintext, nil
}

// readNamedRowFields executes a concept==CONCEPT;payload.name==NAME
// query and returns the first row's payload field map. `kind` is used
// only in error messages ("variable" / "secret" / "apikey").
func (e *MemQLEngine) readNamedRowFields(ctx context.Context, conceptName, name, kind string) (map[string]*structpb.Value, error) {
	query := fmt.Sprintf(`concept==%s;payload.name=="%s"`, conceptName, name)
	result, err := e.Execute(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to query %s %q: %w", kind, name, err)
	}
	if result == nil || result.Bundle == nil || len(result.Bundle.Nodes) == 0 {
		return nil, &notFoundVariableError{name: name, kind: kind}
	}
	node := result.Bundle.Nodes[0]
	if node == nil || node.Payload == nil {
		return nil, fmt.Errorf("%s %q has no payload", kind, name)
	}
	fields := node.Payload.GetFields()
	if fields == nil {
		return nil, fmt.Errorf("%s %q has empty payload", kind, name)
	}
	return fields, nil
}

// notFoundVariableError marks a lookup miss so ResolveVariable /
// ResolveSecret can distinguish a "try global next" case from a real
// error. Internal -- not exported.
type notFoundVariableError struct {
	name string
	kind string
}

func (e *notFoundVariableError) Error() string {
	return fmt.Sprintf("%s %q not found", e.kind, e.name)
}

func isNotFoundVariable(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*notFoundVariableError)
	return ok
}
