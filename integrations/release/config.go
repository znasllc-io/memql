package release

import (
	"context"
	"os"
	"strings"
)

// config.go -- what to cut, and with what.
//
// TWO VALUES, TWO KINDS, AND THE SPLIT IS NOT COSMETIC. The repository is
// plaintext configuration (a globalVariable); the token is a credential (a
// globalSecret, encrypted at rest and never returned to any client). Seeding
// the token as a variable would work and would put a repository-write
// credential in a plaintext row, so the resolver looks for the secret FIRST
// and only then falls back -- the fallback exists for the bootstrap window
// (see below), not as a blessing.
//
// THE ENGINE CARRIES NO REPOSITORY DEFAULT. Not "no default yet" -- none, ever.
// The engine is product-agnostic (platform consolidation, #2472) and shipping
// an organization's `owner/name` as a Go literal would put a product name in
// every node image, which is what product-neutrality forbids and what
// TestEngineIsProductNeutral exists to catch one class of. An instance that
// wants this button seeds MEMQL_RELEASE_REPO; an instance that does not gets
// release_repo_unconfigured stated on the card, which is a true sentence about
// how that installation is set up.
//
// WHY ENV IS IN THE CHAIN AT ALL. Same reason ai_providers.go's resolver has
// it: `make up-refresh` wipes the database before re-seeding, so on first boot
// concept storage is empty and the env is the only thing carrying the value.
// A cut attempted in that window with the row absent and the env set should
// work rather than report a missing credential.

const (
	// SecretName is the globalSecret / env key carrying the GitHub token.
	//
	// The token wants Contents: read/write on ONE repository (fine-grained
	// PAT or a GitHub App installation token). It needs Pull requests:
	// read/write ONLY if bumpExtensionPin is ever used, which is why the
	// pin bump degrades to a note rather than failing the cut: a token
	// scoped for cutting alone is a correct token, and a release that
	// published should not report failure because a follow-on PR could not.
	SecretName = "MEMQL_GITHUB_RELEASE_TOKEN"

	// RepoVariableName is the globalVariable / env key naming the
	// repository, in `owner/name` form.
	RepoVariableName = "MEMQL_RELEASE_REPO"
)

// resolver is the narrow slice of PluginContext this package needs. An
// interface rather than the struct so the tests can drive the chain directly
// -- and so the order below is testable, which matters because "secret first"
// is a security property and not an implementation detail.
type resolver struct {
	systemSecret   func(ctx context.Context, name string) (string, error)
	systemVariable func(ctx context.Context, name string) (string, error)
	// env is os.Getenv in production; a map lookup in tests. Injected so
	// a test never has to mutate the process environment, which makes
	// parallel tests flaky in a way that looks like a resolver bug.
	env func(name string) string
}

// resolve walks globalSecret -> globalVariable -> env and returns the first
// non-blank value.
//
// A resolver error is treated exactly like a miss and the walk continues. That
// is deliberate: the two ways to "not find it" are an absent row and an
// unreachable store, and on a node whose database is still coming up the
// second is transient. Stopping the walk on it would report
// credential_unavailable for a token the env has, during precisely the window
// the env fallback exists for.
func (r resolver) resolve(ctx context.Context, name string) string {
	if r.systemSecret != nil {
		if v, err := r.systemSecret(ctx, name); err == nil {
			if t := strings.TrimSpace(v); t != "" {
				return t
			}
		}
	}
	if r.systemVariable != nil {
		if v, err := r.systemVariable(ctx, name); err == nil {
			if t := strings.TrimSpace(v); t != "" {
				return t
			}
		}
	}
	if r.env != nil {
		if t := strings.TrimSpace(r.env(name)); t != "" {
			return t
		}
	}
	return ""
}

// osEnv is the production env reader.
func osEnv(name string) string { return os.Getenv(name) }

// repoRef is a parsed `owner/name`.
type repoRef struct {
	Owner string
	Name  string
}

func (r repoRef) String() string { return r.Owner + "/" + r.Name }

// parseRepo reads the `owner/name` form.
//
// Strict about the shape rather than forgiving: a value like
// "https://github.com/owner/name" or "owner/name.git" would half-work --
// composing URLs that 404 with a message about a missing repository, which
// sends the operator looking at permissions instead of at the value they
// typed. A refusal naming the expected form is the shorter path to a fix.
func parseRepo(s string) (repoRef, bool) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(s), "/"), "/")
	if len(parts) != 2 {
		return repoRef{}, false
	}
	owner, name := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if owner == "" || name == "" {
		return repoRef{}, false
	}
	// A path segment cannot carry these, and a value that does is a URL or
	// a shell fragment rather than a repository.
	for _, bad := range []string{":", "?", "#", " ", "@"} {
		if strings.Contains(owner, bad) || strings.Contains(name, bad) {
			return repoRef{}, false
		}
	}
	return repoRef{Owner: owner, Name: name}, true
}

// settings is the resolved configuration for one call.
type settings struct {
	repo  repoRef
	token string
}

// loadSettings resolves both values, refusing with the code whose setup step
// the card renders.
//
// THE REPOSITORY IS CHECKED FIRST, and the order is not arbitrary: an instance
// that has seeded neither should be told what to cut before it is told what to
// cut it with, because naming the repository is the decision and minting the
// token is the consequence of having made it.
func (r resolver) loadSettings(ctx context.Context) (settings, error) {
	raw := r.resolve(ctx, RepoVariableName)
	if raw == "" {
		return settings{}, refuse(CodeReleaseRepoUnconfigured,
			"no repository is configured to cut releases of. Seed %s with the owner/name of the repository (for example: acme/widget) as a global variable.",
			RepoVariableName)
	}
	repo, ok := parseRepo(raw)
	if !ok {
		return settings{}, refuse(CodeReleaseRepoUnconfigured,
			"%s must be owner/name (for example: acme/widget); it is not a URL and carries no .git suffix.",
			RepoVariableName)
	}
	token := r.resolve(ctx, SecretName)
	if token == "" {
		return settings{}, refuse(CodeCredentialUnavailable,
			"no GitHub credential is available. Seed %s as a global secret with a fine-grained token holding Contents: read/write on %s.",
			SecretName, repo)
	}
	return settings{repo: repo, token: token}, nil
}
