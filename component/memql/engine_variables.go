package memql

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
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

// secretResolverActor names the deployment in the resolver's own reads of the
// two secret concepts. It shows up as `system:secrets` wherever an actor id is
// recorded.
const secretResolverActor = "secrets"

// secretReadContext is the context a secret LOOKUP runs under: the deployment
// reading its own row, whatever context the resolver was handed.
//
// WHY THE LOOKUP DOES NOT RUN AS ITS CALLER. v1:platform:globalSecret and
// v1:platform:partitionSecret declare @rowAuthz(clusterOwner), and a lookup is
// a raw read, so the row gate decides it from the actor on the context. The
// resolver's callers hold whatever context they were handed: the edge resolves
// a storefront token inside a visitor's request, the inbound receiver resolves
// a signing secret with no identity at all, a send resolves its credentials
// under whoever triggered it, and provider load resolves at boot under
// context.Background(). Judged by its caller, every one of those answers "not
// found" -- which is the answer for a secret nobody stored, so it would read
// as a configuration problem and not as this.
//
// WHY A SYSTEM ACTOR AND NOT INTERNAL ORIGIN. Origin says the ENGINE is making
// the call; it is what a @serverOnly construct asks, and the read side of the
// cluster-owner tier does not consult it (rowAuthzAdmitsMode asks who the
// actor is). auth.SystemActor is the identity for the deployment acting on a
// row that belongs to the deployment and to no person, which is what a secret
// row is. The caller's origin is left as it arrived: nothing here widens what
// the caller's OWN calls may do.
//
// WHAT THIS DOES NOT DECIDE. Who may ask for a secret's VALUE is decided where
// it always was: by the Go code that calls ResolveSecret / ResolveSystemSecret,
// and by who may author a body that calls secret() or systemSecret(). The tier
// decides who may read the ROW -- the ciphertext and the fingerprint -- and
// this function is why declaring it took nothing away from the resolvers. The
// derived context lives for the one Execute below and is never returned.
//
// Regression: TestSecretsResolveWhateverContextTheResolverWasHanded
// (platform_secret_tier_db_test.go), which also holds that the caller issuing
// the lookup's own query string still reads no row; the outbound worker's half
// is app's TestOutboundSecretTargetsResolveUnderTheWorkersActor.
func secretReadContext(ctx context.Context) context.Context {
	return auth.ContextWithSystemActor(ctx, secretResolverActor)
}

// readSecret performs the common lookup against a *:secret concept
// (v1:platform:partitionSecret or v1:platform:globalSecret). Reads payload.encryptedValue
// and returns the decrypted plaintext via the secret package. Requires
// MEMQL_MASTER_KEY to be set.
//
// The lookup runs as the deployment (secretReadContext), so it resolves the
// same row for every caller; the plaintext it returns is the caller's to
// handle, and must not be logged or persisted.
func (e *MemQLEngine) readSecret(ctx context.Context, conceptName, name string) (string, error) {
	fields, err := e.readNamedRowFields(secretReadContext(ctx), conceptName, name, "secret")
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
		return "", undecryptableSecret(name, err)
	}
	return plaintext, nil
}

// readNamedRowFields executes a concept==CONCEPT&&payload.name==NAME
// query and returns the first row's payload field map. `kind` is used
// only in error messages ("variable" / "secret" / "apikey").
//
// The name is rendered with langparser.QuoteString, never pasted between
// literal quotes (memql#5625). Pasted, a name carrying a `"` closed the
// literal early and the rest of it was read as query: a name ending in
// `" || payload.name=="X` resolved the row named X, and a lone quote or
// backslash made the lookup unparseable. Names reach here from rows other
// people write (a store's token reference, a provider's auth field), so the
// name is data and only ever a literal. The concept is one of this file's
// four constants and stays bare.
func (e *MemQLEngine) readNamedRowFields(ctx context.Context, conceptName, name, kind string) (map[string]*structpb.Value, error) {
	query := fmt.Sprintf(`concept==%s&&payload.name==%s`, conceptName, langparser.QuoteString(name))
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

// ErrVariableNotFound is what every resolver miss matches with errors.Is: no
// row of the named variable or secret exists. Exported, with
// IsVariableNotFound, so a caller can tell "nobody stored it", which waits on
// an operator, from a lookup that failed and may pass on its own -- the
// outbound worker fails a secret target on the first and retries it on the
// second (memql#5480).
var ErrVariableNotFound = errors.New("variable not found")

// ErrSecretUndecryptable is what a secret matches when its row was found and
// its value could not be decrypted: MEMQL_MASTER_KEY unset or malformed, a
// malformed ciphertext, or a key that does not open it. Each waits on an
// operator, like a miss.
var ErrSecretUndecryptable = errors.New("secret cannot be decrypted")

// IsVariableNotFound reports whether err is a resolver miss: no row of the
// named variable or secret.
func IsVariableNotFound(err error) bool { return errors.Is(err, ErrVariableNotFound) }

// IsSecretUndecryptable reports whether err is a secret whose row was found
// and whose value could not be decrypted.
func IsSecretUndecryptable(err error) bool { return errors.Is(err, ErrSecretUndecryptable) }

// notFoundVariableError marks a lookup miss so ResolveVariable /
// ResolveSecret can distinguish a "try global next" case from a real
// error. The type stays unexported; callers outside the package match it
// with IsVariableNotFound.
type notFoundVariableError struct {
	name string
	kind string
}

func (e *notFoundVariableError) Error() string {
	return fmt.Sprintf("%s %q not found", e.kind, e.name)
}

func (e *notFoundVariableError) Is(target error) bool { return target == ErrVariableNotFound }

func isNotFoundVariable(err error) bool { return IsVariableNotFound(err) }

// undecryptableSecretError is a secret found and not decrypted. Its message
// is the one readSecret has always returned, so an operator reads no change.
type undecryptableSecretError struct {
	name string
	err  error
}

func undecryptableSecret(name string, err error) error {
	return &undecryptableSecretError{name: name, err: err}
}

func (e *undecryptableSecretError) Error() string {
	return fmt.Sprintf("secret %q: %v", e.name, e.err)
}

func (e *undecryptableSecretError) Unwrap() error { return e.err }

func (e *undecryptableSecretError) Is(target error) bool { return target == ErrSecretUndecryptable }
