package identity

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/identity/githubconnect"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/secret"
)

// store_githubapp.go -- the cluster's GitHub App, as six rows (design record
// 2026-09-20-github-app-setup, D1).
//
// A registration made from the product lands here: the six values, under their
// six environment names, in the two instance-wide stores every node already
// reads -- v1:platform:globalVariable for the three identifiers and
// v1:platform:globalSecret, sealed under MEMQL_MASTER_KEY, for the three
// credentials. githubconnect.Resolve is the reader; this file is the writer,
// and the two agree on the names and on which store each name lives in because
// both ask githubconnect (EnvNames, IsSecretName).
//
// # The row ids are the SEEDER's
//
// `var-global-<slug>` and `secret-global-<slug>`, the derivation
// scripts/secrets, component/memql/default_injector.go and
// integrations/email/configure.go already share. A deterministic id is what
// makes a second registration REPLACE the first rather than sit beside it --
// two rows under one name, and a resolver that reads whichever came back first
// -- and re-using the seeder's means a value seeded by an operator and a value
// written here are the same row. integrations/email/configure.go records why a
// fourth derivation would be the one that disagrees.
//
// # No internal-origin stamp on the WRITES
//
// setGlobalVariable and setGlobalSecret are not @serverOnly: what they store is
// already sealed or already public, and what authorizes the write is the
// caller. Here that caller is either a cluster owner (githubAppRemove, under
// their own actor) or the identity service's own owner-role system actor (the
// app-setup callback, which a browser redirect reaches with no MemQL bearer --
// see SystemActorMiddleware). Neither needs the gate opened, so neither opens
// it. The one stamp in this file is RecomputeGithubAppReadiness, which says why.

// githubAppRowDescription is what each row says about itself in the stores'
// own surfaces, where an operator looking at cluster configuration will meet
// it beside values they seeded themselves.
const githubAppRowDescription = "The cluster's GitHub App, registered from MemQL OS. Replace or remove it there."

// githubAppRowKind groups the three credential rows in v1:platform:globalSecret.
const githubAppRowKind = "github_app"

func githubAppRowId(envName string) string {
	prefix := "var-global-"
	if githubconnect.IsSecretName(envName) {
		prefix = "secret-global-"
	}
	return prefix + strings.ToLower(strings.ReplaceAll(strings.TrimSpace(envName), "_", "-"))
}

// WriteGithubAppRegistration stores the six values of one registration.
//
// ALL SIX OR NOTHING IS ATTEMPTED: a Config that is not whole is refused before
// the first write, because the resolver treats a partial set as no app and a
// registration that silently produced "no app" would send the owner round the
// flow again with nothing saying why.
//
// THE CREDENTIALS ARE SEALED HERE, in the frame that holds them, and the
// plaintext goes no further: not into an error (secret.Encrypt's are about the
// key, never the value), not into a log, not into the statement -- what the
// statement carries is ciphertext and a four-character fingerprint.
//
// The writes are six statements, not one transaction, and the ORDER is the
// mitigation: the three identifiers first, the private key LAST. Resolve needs
// all six, so an app becomes visible to the cluster only when the final write
// lands; a failure part-way leaves a partial set, which reads as no app, and
// the owner's retry overwrites every row by id.
func (s *Store) WriteGithubAppRegistration(ctx context.Context, cfg githubconnect.Config, addedBy string) error {
	if s == nil || s.Engine == nil {
		return errNoEngine
	}
	if missing := cfg.Missing(); len(missing) > 0 {
		return fmt.Errorf("identity.store: a GitHub App registration needs all six values; missing %s", strings.Join(missing, ", "))
	}
	values := map[string]string{
		githubconnect.EnvAppID:         cfg.AppID,
		githubconnect.EnvAppSlug:       cfg.AppSlug,
		githubconnect.EnvClientID:      cfg.ClientID,
		githubconnect.EnvClientSecret:  cfg.ClientSecret,
		githubconnect.EnvWebhookSecret: cfg.WebhookSecret,
		githubconnect.EnvPrivateKeyB64: cfg.PrivateKeyB64,
	}
	for _, name := range githubAppWriteOrder() {
		if err := s.writeGithubAppValue(ctx, name, values[name], addedBy); err != nil {
			return err
		}
	}
	return nil
}

// githubAppWriteOrder is identifiers, then credentials, the private key last.
func githubAppWriteOrder() []string {
	return []string{
		githubconnect.EnvAppID,
		githubconnect.EnvAppSlug,
		githubconnect.EnvClientID,
		githubconnect.EnvClientSecret,
		githubconnect.EnvWebhookSecret,
		githubconnect.EnvPrivateKeyB64,
	}
}

func (s *Store) writeGithubAppValue(ctx context.Context, envName, value, addedBy string) error {
	if !githubconnect.IsSecretName(envName) {
		query := fmt.Sprintf(`mutation setGlobalVariable(id: %s, name: %s, value: %s, description: %s, active: true)`,
			langparser.QuoteString(githubAppRowId(envName)),
			langparser.QuoteString(envName),
			langparser.QuoteString(strings.TrimSpace(value)),
			langparser.QuoteString(githubAppRowDescription),
		)
		if _, err := s.Engine.Execute(ctx, query); err != nil {
			return fmt.Errorf("identity.store: write %s: %w", envName, err)
		}
		return nil
	}
	ciphertext, fingerprint, err := secret.Encrypt(value)
	if err != nil {
		// Never wrap the plaintext into an error: this string reaches a log.
		return fmt.Errorf("identity.store: seal %s: %w", envName, err)
	}
	query := fmt.Sprintf(`mutation setGlobalSecret(id: %s, name: %s, encryptedValue: %s, fingerprint: %s, kind: %s, description: %s, addedBy: %s, active: true)`,
		langparser.QuoteString(githubAppRowId(envName)),
		langparser.QuoteString(envName),
		langparser.QuoteString(ciphertext),
		langparser.QuoteString(fingerprint),
		langparser.QuoteString(githubAppRowKind),
		langparser.QuoteString(githubAppRowDescription),
		langparser.QuoteString(strings.TrimSpace(addedBy)),
	)
	if _, err := s.Engine.Execute(ctx, query); err != nil {
		return fmt.Errorf("identity.store: write %s: %w", envName, err)
	}
	return nil
}

// ClearGithubAppRegistration removes the app a registration stored.
//
// IT OVERWRITES WITH BLANK, because these stores have no delete and need none:
// the rows are versioned, a blank value is what every reader already treats as
// absent (githubconnect.Resolve and readiness_eval.go both trim and test for
// empty), and the overwrite is one more version under the same id. A credential
// row is cleared with an EMPTY ciphertext rather than a sealed empty string --
// secret.Encrypt refuses to seal nothing, correctly -- which the secret reader
// answers with an error, and an unreadable row is an absent row.
//
// The private key FIRST, the mirror of the write order: the app stops being
// resolvable at the first statement rather than the last.
//
// WHAT THIS DOES NOT DO is take the credentials out of the world. Earlier
// versions of a row keep their sealed ciphertext, as every rotated secret on
// this platform does, and the app still exists at GitHub. An owner removing an
// app because its credentials may have leaked deletes it at GITHUB, which is
// what makes them worthless; the operator guide says so where it describes
// this call.
func (s *Store) ClearGithubAppRegistration(ctx context.Context, clearedBy string) error {
	if s == nil || s.Engine == nil {
		return errNoEngine
	}
	order := githubAppWriteOrder()
	for i := len(order) - 1; i >= 0; i-- {
		envName := order[i]
		var query string
		if githubconnect.IsSecretName(envName) {
			query = fmt.Sprintf(`mutation setGlobalSecret(id: %s, name: %s, encryptedValue: "", fingerprint: "", kind: %s, description: %s, addedBy: %s, active: false)`,
				langparser.QuoteString(githubAppRowId(envName)),
				langparser.QuoteString(envName),
				langparser.QuoteString(githubAppRowKind),
				langparser.QuoteString("Cleared from MemQL OS."),
				langparser.QuoteString(strings.TrimSpace(clearedBy)),
			)
		} else {
			query = fmt.Sprintf(`mutation setGlobalVariable(id: %s, name: %s, value: "", description: %s, active: false)`,
				langparser.QuoteString(githubAppRowId(envName)),
				langparser.QuoteString(envName),
				langparser.QuoteString("Cleared from MemQL OS."),
			)
		}
		if _, err := s.Engine.Execute(ctx, query); err != nil {
			return fmt.Errorf("identity.store: clear %s: %w", envName, err)
		}
	}
	return nil
}

// RecomputeGithubAppReadiness asks this node to re-evaluate its readiness rows,
// because the `githubApp` module's answer just changed.
//
// INTERNAL ORIGIN, and this is the one stamp in the file. readinessRecompute is
// internal-or-cluster-owner, deliberately: it makes a node rewrite rows, and no
// signed-in person should be able to ask for that in a loop. The caller here is
// the app-setup callback, which a browser redirect reaches with no MemQL
// bearer, so there is no owner to be -- and the act is this cluster describing
// itself after its own write, which is what the gate's internal arm is for.
// Stamped inline, as the argument to the one Execute, so the marked context
// dies at this call.
//
// A failure is the caller's to LOG and nobody's to surface: the registration
// is stored either way, and the module's mark catches up on the next
// readiness safety-net pass.
func (s *Store) RecomputeGithubAppReadiness(ctx context.Context) error {
	if s == nil || s.Engine == nil {
		return errNoEngine
	}
	if _, err := s.Engine.Execute(auth.ContextWithInternalOrigin(ctx), "builtin readinessRecompute()"); err != nil {
		return fmt.Errorf("identity.store: recompute readiness: %w", err)
	}
	return nil
}
