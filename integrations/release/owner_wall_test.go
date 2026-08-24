package release

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
)

// owner_wall_test.go -- the load-bearing gate, proved twice over.
//
// ===========================================================================
// WHY THE ACTOR IS REAL AND THE ENGINE IS A TRIPWIRE
// ===========================================================================
// The actor is built with auth.ContextWithAccess and a real *auth.AccessContext
// -- the exact type and the exact constructor the gRPC middleware uses -- and
// the predicate under test is AccessContext.IsClusterOwner. Nothing here
// re-implements the role check, so a change to what "owner" means changes this
// test's verdict rather than being papered over by a stub that agrees with the
// old rule. That is the fake-engine-has-no-gates lesson: a suite that asserts
// against its own mock of the thing being gated proves only that the mock
// agrees with itself.
//
// The ENGINE, by contrast, is a tripwire rather than a fake: any call to it
// fails the test. That is what makes the negative assertion mean something.
// Asserting only "a non-owner gets an error" would pass if the handler cut the
// release, wrote the row, and then errored on its way out.
//
// ===========================================================================
// AND WHY GITHUB IS A TRIPWIRE TOO
// ===========================================================================
// The second half of the wall's contract is ORDER: the refusal happens before
// any network call, so a non-owner cannot learn from a timing difference or a
// refusal code whether this cluster has a credential seeded. A test that only
// checked the returned error would pass on a handler that resolved the
// credential, called GitHub, and refused afterwards -- which is a working gate
// with an information leak behind it. So the GitHub base URL points at a
// server that fails the test if it is ever reached.

// tripwireEngine fails the test on any Execute. It is the CONTROL for every
// "the engine was never reached" claim in this file.
type tripwireEngine struct {
	memql.IntegrationEngineAccess
	t *testing.T
}

func (e *tripwireEngine) Execute(_ context.Context, call string) (*memql.ExecuteResult, error) {
	e.t.Helper()
	e.t.Fatalf("the engine was reached by a caller the owner wall should have refused: %s", call)
	return nil, nil
}

// tripwireServer fails the test on any request.
func tripwireServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("a network call was made by a caller the owner wall should have refused: %s %s", r.Method, r.URL.Path)
	}))
}

// walledIntegration wires both tripwires plus a resolver that WOULD succeed.
//
// The resolver succeeding is deliberate: if the configuration were missing,
// a refusal would prove nothing about the owner wall, because
// release_repo_unconfigured would be the answer for an owner too.
func walledIntegration(t *testing.T) *Integration {
	t.Helper()
	server := tripwireServer(t)
	t.Cleanup(server.Close)
	i := NewIntegration(slog.New(slog.DiscardHandler), &tripwireEngine{t: t}, resolver{
		env: func(name string) string {
			switch name {
			case RepoVariableName:
				return "acme/widget"
			case SecretName:
				return "a-token-that-must-never-be-used"
			}
			return ""
		},
	})
	i.github = NewClient().WithBaseURL(server.URL)
	i.registry = NewRegistryChecker().WithBaseURL(server.URL)
	return i
}

// actorContext builds the real thing the middleware builds.
func actorContext(role auth.Role) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId:       "v1:identity:user:someone",
		PrimaryEmail: "someone@example.com",
		Role:         role,
	})
}

func TestCutRefusesEveryNonOwnerRoleBeforeAnythingHappens(t *testing.T) {
	// Every role in the model except owner, plus the two shapes that carry
	// no role at all. Enumerated rather than sampled: a gate written as
	// `!= reader` would pass a one-role test and admit everybody else.
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleDeveloper, auth.RoleWriter, auth.RoleReader, auth.Role("")} {
		t.Run("role="+string(role), func(t *testing.T) {
			i := walledIntegration(t)
			_, err := i.Cut(actorContext(role), CutRequest{Bump: "patch"})
			if err == nil {
				t.Fatalf("role %q was allowed to cut a release", role)
			}
			if got := RefusalCode(err); got != CodeNotOwner {
				t.Fatalf("refusal code = %q, want %q (error: %v)", got, CodeNotOwner, err)
			}
		})
	}
}

func TestCutRefusesACallWithNoResolvedIdentity(t *testing.T) {
	// A bare context is what a caller with no authenticated session
	// produces. The gate must read that as "not an owner" rather than as
	// "an internal call we can trust" -- the fail-open reading is the one
	// that ships a release to an anonymous caller.
	i := walledIntegration(t)
	_, err := i.Cut(context.Background(), CutRequest{Bump: "patch"})
	if got := RefusalCode(err); got != CodeNotOwner {
		t.Fatalf("refusal code = %q, want %q (error: %v)", got, CodeNotOwner, err)
	}
}

func TestStatusRefusesANonOwnerToo(t *testing.T) {
	// The check reads the cut history, which the releaseCuts query gates
	// requiresOwner. A status check open to admins would be the back door
	// that gate closes at the front.
	i := walledIntegration(t)
	_, err := i.Status(actorContext(auth.RoleAdmin), "v1.2.3")
	if got := RefusalCode(err); got != CodeNotOwner {
		t.Fatalf("refusal code = %q, want %q (error: %v)", got, CodeNotOwner, err)
	}
}

// TestOwnerWallIsReachable is the POSITIVE CONTROL, and without it every test
// above is compatible with a handler that refuses everybody.
//
// It drives an OWNER through the same wall and asserts it gets past -- proved
// by the failure being about something downstream (the tripwire GitHub server
// returning nonsense) rather than about the role. A "the instrument could have
// moved" assertion: a wall that refused owners too would make the four tests
// above pass while the feature was entirely dead.
func TestOwnerWallIsReachable(t *testing.T) {
	i := NewIntegration(slog.New(slog.DiscardHandler), nil, resolver{
		env: func(name string) string {
			switch name {
			case RepoVariableName:
				return "acme/widget"
			case SecretName:
				return "token"
			}
			return ""
		},
	})
	// A server that is REACHED and answers unhelpfully. Getting here at all
	// is the proof.
	reached := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	i.github = NewClient().WithBaseURL(server.URL)

	_, err := i.Cut(actorContext(auth.RoleOwner), CutRequest{Bump: "patch"})
	if !reached {
		t.Fatal("an OWNER did not get past the wall -- the refusal tests above are therefore vacuous")
	}
	if got := RefusalCode(err); got != CodeGithubUnreachable {
		t.Fatalf("an owner reached GitHub and got %q; want %q, which is what a 500 means", got, CodeGithubUnreachable)
	}
}

// TestOwnerWallPredicateMatchesTheDSLGate pins the two walls together.
//
// The DSL half is `spec actorEnvelope requiresOwner { return role == "owner" }`
// on the releaseCuts query. If the Go half drifted to a different predicate --
// IsPrivilegedUser, say, which admits admin -- the read and the write would
// disagree about who may cut, and an admin would see a history for a button
// that refuses them. Neither gate's own tests would notice: each would still be
// internally correct.
func TestOwnerWallPredicateMatchesTheDSLGate(t *testing.T) {
	for _, tc := range []struct {
		role auth.Role
		want bool
	}{
		{auth.RoleOwner, true},
		{auth.RoleAdmin, false},
		{auth.RoleDeveloper, false},
		{auth.RoleWriter, false},
		{auth.RoleReader, false},
	} {
		ac := &auth.AccessContext{Role: tc.role}
		// The DSL spec's predicate, spelled as the DSL spells it.
		dsl := string(tc.role) == "owner"
		if dsl != tc.want {
			t.Fatalf("the table itself disagrees with `role == \"owner\"` for %q", tc.role)
		}
		if got := ac.IsClusterOwner(); got != dsl {
			t.Errorf("role %q: Go wall says %v, the DSL's `role == \"owner\"` says %v", tc.role, got, dsl)
		}
	}
}

// TestRefusalsNeverCarryTheCredential is a leak check over the whole refusal
// vocabulary.
//
// The rule refusals.go states -- "a refusal about a token names the variable to
// seed, never the value" -- is worth checking rather than trusting, because a
// refusal string is the one place a credential is most likely to be helpfully
// interpolated, and it lands in a row, a log line and a browser at once.
func TestRefusalsNeverCarryTheCredential(t *testing.T) {
	const secret = "ghp_averysecrettokenvalue"
	i := NewIntegration(slog.New(slog.DiscardHandler), nil, resolver{
		env: func(name string) string {
			switch name {
			case RepoVariableName:
				return "acme/widget"
			case SecretName:
				return secret
			}
			return ""
		},
	})
	// A server that refuses the credential -- the refusal most likely to
	// quote it back.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer server.Close()
	i.github = NewClient().WithBaseURL(server.URL)

	_, err := i.Cut(actorContext(auth.RoleOwner), CutRequest{Bump: "patch"})
	if err == nil {
		t.Fatal("a 401 was not refused")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the refusal quoted the credential: %v", err)
	}
	if !strings.Contains(err.Error(), SecretName) {
		t.Fatalf("the refusal does not name %s, which is the actionable half: %v", SecretName, err)
	}
	var r *Refusal
	if !errors.As(err, &r) || r.Code != CodeCredentialUnavailable {
		t.Fatalf("a 401 should be %s; got %v", CodeCredentialUnavailable, err)
	}
}
