package memql

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/znasllc-io/memql/component/genesis"
	"github.com/znasllc-io/memql/component/secret"
)

// DefaultInjector performs the one-time, only-if-absent seed of the
// registry's DEFAULTED platform-scoped variables/secrets on first
// deploy (Epic 7 / memql#2109).
//
// The env-var registry (scripts/secrets/manifest.yaml, loaded via
// component/genesis) carries a `default` on a handful of entries -- the
// value memQL should run with when an operator never set one (e.g.
// MEMQL_EMAIL_FROM_NAME="memQL"). This injector walks the manifest, selects
// the entries that BOTH carry a default AND are concept-stored
// (scope global/partition; node-scoped entries are per-pod process env
// and are never concept-stored), and for each one writes the default as
// a v1:platform:globalVariable / globalSecret (or the partition-scoped
// variants) row -- but ONLY IF that value is absent.
//
// Idempotency / multi-node safety:
//
//   - It is safe to run on EVERY boot of EVERY node. The only-if-absent
//     existence check makes a re-deploy where an operator already set a
//     value a strict NO-OP: an operator edit is never overwritten.
//   - It is safe to run CONCURRENTLY from every replica in the mesh.
//     The platform set* mutations are deterministic-id upserts, so two
//     replicas racing to seed the same absent default converge on one
//     logical row (memql is time-series; a duplicate insert stamps a new
//     version of the same id, not a second row) carrying the identical
//     default value -- there is no operator value to clobber because the
//     check already found none.
//   - When in doubt, it treats a value as PRESENT and skips it. A failed
//     existence check (DB hiccup, decrypt error) never injects, so the
//     conservative outcome is "leave whatever is there alone."
//
// Secrets ride the SAME storage path as an operator-seeded secret: the
// default plaintext is encrypted via component/secret before the
// setGlobalSecret / setPartitionSecret mutation persists the ciphertext.
// No new secret store is invented.
type DefaultInjector struct {
	store    injectStore
	manifest *genesis.Manifest
	logr     *slog.Logger
}

// injectStore is the narrow read+write surface the injector needs: an
// existence check (only-if-absent) and a default insert. *MemQLEngine
// satisfies it via engineInjectStore; tests substitute a fake so
// idempotency is exercised without a database.
type injectStore interface {
	// valuePresent reports whether a named row already exists in the
	// platform concept. An error is treated by the injector as
	// "present" (conservative -- never overwrite on an inconclusive
	// check).
	valuePresent(ctx context.Context, conceptName, name string) (bool, error)
	// insertVariableDefault / insertSecretDefault persist the entry's
	// default as a plaintext variable / encrypted secret row.
	insertVariableDefault(ctx context.Context, c genesis.InjectableDefault) error
	insertSecretDefault(ctx context.Context, c genesis.InjectableDefault) error
}

// NewDefaultInjector wires an injector to the engine and the loaded
// registry manifest. Returns nil if either is nil -- the injector has
// nothing to do without a write path or a registry to read.
func NewDefaultInjector(engine *MemQLEngine, manifest *genesis.Manifest) *DefaultInjector {
	if engine == nil || manifest == nil {
		return nil
	}
	return &DefaultInjector{
		store:    &engineInjectStore{engine: engine},
		manifest: manifest,
		logr:     engine.Logger,
	}
}

// newDefaultInjectorWithStore builds an injector over an arbitrary
// store. Used by tests to inject a fake; production callers use
// NewDefaultInjector.
func newDefaultInjectorWithStore(store injectStore, manifest *genesis.Manifest, logr *slog.Logger) *DefaultInjector {
	return &DefaultInjector{store: store, manifest: manifest, logr: logr}
}

// InjectionReport summarizes one InjectDefaults sweep for logging.
type InjectionReport struct {
	Candidates int
	Injected   int
	Skipped    int // already present
	Failed     int
}

// InjectDefaults runs the only-if-absent seed across every injectable
// registry default. It is safe to call on every boot and from every
// node. Per-entry failures are logged and counted; they never abort the
// sweep or the boot.
func (di *DefaultInjector) InjectDefaults(ctx context.Context) (InjectionReport, error) {
	if di == nil || di.store == nil || di.manifest == nil {
		return InjectionReport{}, fmt.Errorf("default injector: store + manifest must be non-nil")
	}
	logger := di.logr

	candidates := di.manifest.InjectableDefaults()
	report := InjectionReport{Candidates: len(candidates)}

	for _, c := range candidates {
		injected, err := di.injectOne(ctx, c)
		switch {
		case err != nil:
			report.Failed++
			if logger != nil {
				logger.Warn("default injection failed for registry entry; leaving value untouched",
					"component", "memql.defaultInjector",
					"name", c.Entry.Name,
					"scope", c.Entry.Scope,
					"secret", c.IsSecret,
					"error", err.Error())
			}
		case injected:
			report.Injected++
			if logger != nil {
				logger.Info("seeded registry default (was absent)",
					"component", "memql.defaultInjector",
					"name", c.Entry.Name,
					"scope", c.Entry.Scope,
					"secret", c.IsSecret)
			}
		default:
			report.Skipped++
			if logger != nil {
				logger.Debug("registry default already present; skipping",
					"component", "memql.defaultInjector",
					"name", c.Entry.Name,
					"scope", c.Entry.Scope,
					"secret", c.IsSecret)
			}
		}
	}

	if logger != nil {
		logger.Info("registry default injection sweep complete",
			"component", "memql.defaultInjector",
			"candidates", report.Candidates,
			"injected", report.Injected,
			"skipped_present", report.Skipped,
			"failed", report.Failed)
	}
	return report, nil
}

// injectOne seeds a single registry default if it is absent. Returns
// (true, nil) when it inserted the default, (false, nil) when the value
// already existed (NO-OP), and (false, err) on an error -- in which case
// nothing is written (conservative: leave whatever is there alone).
func (di *DefaultInjector) injectOne(ctx context.Context, c genesis.InjectableDefault) (bool, error) {
	conceptName := injectConceptName(c)

	present, err := di.store.valuePresent(ctx, conceptName, c.Entry.Name)
	if err != nil {
		// Be conservative: an inconclusive existence check must NOT
		// trigger an insert that could clobber an operator value the
		// check simply failed to read.
		return false, fmt.Errorf("existence check: %w", err)
	}
	if present {
		return false, nil
	}

	if c.IsSecret {
		if err := di.store.insertSecretDefault(ctx, c); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := di.store.insertVariableDefault(ctx, c); err != nil {
		return false, err
	}
	return true, nil
}

// engineInjectStore is the production injectStore backed by the live
// MemQL engine: it queries the platform concepts for the only-if-absent
// check and dispatches the canonical platform set* mutations for the
// inserts, reusing the seed materializer's bare-key arg rendering +
// DB-surge retry so the write path matches `make secrets-seed`.
type engineInjectStore struct {
	engine *MemQLEngine
}

// valuePresent reports whether a named row already exists in the given
// platform concept under the active partition context. A query error is
// returned to the caller, which treats it as "present" (conservative).
func (s *engineInjectStore) valuePresent(ctx context.Context, conceptName, name string) (bool, error) {
	query := fmt.Sprintf(`concept==%s;payload.name=="%s"`, conceptName, name)
	result, err := s.engine.Execute(systemActorContext(ctx), query)
	if err != nil {
		return false, err
	}
	if result == nil || result.Bundle == nil {
		return false, nil
	}
	return len(result.Bundle.Nodes) > 0, nil
}

// insertVariableDefault writes the default as a plaintext
// global/partition variable row via the canonical set* mutation.
func (s *engineInjectStore) insertVariableDefault(ctx context.Context, c genesis.InjectableDefault) error {
	mutationName, id := variableMutationFor(c)
	args := map[string]any{
		"id":          id,
		"name":        c.Entry.Name,
		"value":       c.Entry.Default,
		"description": defaultInjectionDescription,
		"active":      true,
	}
	return s.runSetMutation(ctx, mutationName, args)
}

// insertSecretDefault encrypts the default plaintext and writes it as a
// global/partition secret row via the canonical set* mutation. The
// ciphertext + fingerprint are produced by component/secret.Encrypt --
// the same path an operator-seeded secret takes -- so no new secret
// store is introduced. Requires MEMQL_MASTER_KEY (Encrypt errors
// otherwise, which surfaces as a per-entry failure, not an insert).
func (s *engineInjectStore) insertSecretDefault(ctx context.Context, c genesis.InjectableDefault) error {
	ct, fp, err := secret.Encrypt(c.Entry.Default)
	if err != nil {
		return fmt.Errorf("encrypt default: %w", err)
	}
	mutationName, id := secretMutationFor(c)
	args := map[string]any{
		"id":             id,
		"name":           c.Entry.Name,
		"encryptedValue": ct,
		"fingerprint":    fp,
		"kind":           c.Entry.Kind,
		"description":    defaultInjectionDescription,
		"addedBy":        defaultInjectionActor,
		"active":         true,
	}
	return s.runSetMutation(ctx, mutationName, args)
}

// runSetMutation renders the bare-key arg object and dispatches the
// platform set* mutation under the synthetic system actor, with the
// shared DB-surge retry.
func (s *engineInjectStore) runSetMutation(ctx context.Context, mutationName string, args map[string]any) error {
	rendered, err := renderArgsObject(args)
	if err != nil {
		return fmt.Errorf("render args: %w", err)
	}
	query := fmt.Sprintf("%s(%s)", mutationName, rendered)
	var logr *slog.Logger
	if s.engine != nil {
		logr = s.engine.Logger
	}
	return retryOnDBSurge(ctx, logr, func() error {
		_, execErr := s.engine.Execute(systemActorContext(ctx), query)
		return execErr
	})
}

// defaultInjectionDescription / defaultInjectionActor stamp injected
// rows so an operator can tell a first-deploy default apart from a value
// they set themselves.
const (
	defaultInjectionDescription = "first-deploy default (registry #2109)"
	defaultInjectionActor       = "system:defaultInjector"
)

// injectConceptName resolves which v1:platform:* concept a candidate's
// existence check + write target. Secret vs variable picks the family;
// scope (global vs partition) picks the variant.
func injectConceptName(c genesis.InjectableDefault) string {
	if c.IsSecret {
		if c.Entry.Scope == "partition" {
			return "v1:platform:partitionSecret"
		}
		return "v1:platform:globalSecret"
	}
	if c.Entry.Scope == "partition" {
		return "v1:platform:partitionVariable"
	}
	return "v1:platform:globalVariable"
}

// variableMutationFor / secretMutationFor mirror the operator seed tool
// (scripts/secrets/main.go): global vs partition scope selects the
// upsert mutation and the deterministic row id. Keeping the id formula
// identical means the operator seed path and this injector converge on
// the SAME row rather than producing two -- so an operator who later
// runs `make secrets-seed` overwrites the injected default in place.
func variableMutationFor(c genesis.InjectableDefault) (mutation string, id string) {
	if c.Entry.Scope == "global" {
		return "setGlobalVariable", "var-global-" + slugifyInjectName(c.Entry.Name)
	}
	return "setPartitionVariable", "var-" + slugifyInjectName(c.Entry.Name)
}

func secretMutationFor(c genesis.InjectableDefault) (mutation string, id string) {
	if c.Entry.Scope == "global" {
		return "setGlobalSecret", "secret-global-" + slugifyInjectName(c.Entry.Name)
	}
	return "setPartitionSecret", "secret-" + slugifyInjectName(c.Entry.Name)
}

// slugifyInjectName lowercases an env-var name and swaps underscores for
// hyphens, matching scripts/secrets/main.go's slugify so the injected
// row id is identical to the operator-seeded one.
func slugifyInjectName(name string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), "_", "-"))
}
