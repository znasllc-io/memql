package memql

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/maketargets"
)

// citesNoDeadMakeTarget asserts that text directs the reader at no `make`
// target the Makefile lacks.
//
// This is the assertion memql#4405 exists to generalise. Two tests in this
// file asserted strings.Contains(err.Error(), "make secret-set") as a proxy
// for "the seeding hint survived" -- so the assertion passed BECAUSE the
// falsehood was there, and the only automated opinion on the subject was
// voting for the bug. memql#4338 flipped them to assert the absence of that
// one literal, which is better but still names a single historical mistake.
// Reading the real target set covers every future dead name instead.
func citesNoDeadMakeTarget(t *testing.T, what, text string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	real := maketargets.Targets(string(raw))
	// REACHABLE POSITIVE. Without this, a Makefile this test failed to parse
	// yields an empty target set, every citation resolves against nothing --
	// and the assertion below would pass over a text full of dead targets.
	if len(real) == 0 {
		t.Fatal("parsed no targets from the Makefile, so the citation check below would pass over anything")
	}
	if dead := maketargets.UnknownTargets(text, real); len(dead) > 0 {
		t.Fatalf("%s cites make target(s) the Makefile does not have %v:\n%s", what, dead, text)
	}
}

// Tests for the Anthropic credential decision (memql#4334).
//
// The branch that matters most here is the one with no observable behaviour
// in production: a HALF-configured federation must REFUSE rather than fall
// back to the key. Nothing downstream can catch that regression -- a fallback
// works everywhere until the cutover deletes the key it fell back to.

// fixtureIdentityToken writes an unsigned JWS-shaped token carrying the given
// claims and returns its path. Unsigned is right: nothing in the engine
// verifies this signature (Anthropic does, against the cluster's JWKS), and
// preflight is about the token's SHAPE.
func fixtureIdentityToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	token := header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
		t.Fatalf("write token: %v", err)
	}
	return path
}

// validIdentityToken is the shape a correctly-projected Kubernetes token has.
func validIdentityToken(t *testing.T) string {
	t.Helper()
	return fixtureIdentityToken(t, map[string]any{
		"aud": []any{anthropicAudience},
		"sub": "system:serviceaccount:memql:memql-engine",
		"exp": 4102444800,
	})
}

func federatedAuth(tokenFile string) map[string]string {
	return map[string]string{
		authKeyFederationRuleID: "fdrl_test",
		authKeyOrganizationID:   "11111111-2222-3333-4444-555555555555",
		authKeyServiceAccountID: "svac_test",
		// workspaceId deliberately absent: it is optional on Anthropic's side
		// and must not be part of the "all four" that selects federation.
		authKeyIdentityTokenFile: tokenFile,
	}
}

func TestAnthropicCredentialChoosesFederationWhenAllFourAreSet(t *testing.T) {
	tokenFile := validIdentityToken(t)
	cfg := ProviderConfig{Name: "claudeTest", Auth: federatedAuth(tokenFile)}

	opts, path, err := anthropicCredential(cfg, guardedHTTPClient(nil))
	if err != nil {
		t.Fatalf("anthropicCredential: %v", err)
	}
	if path != credentialPathFederation {
		t.Fatalf("credential path = %q, want %q", path, credentialPathFederation)
	}
	if len(opts) == 0 {
		t.Fatal("no request options returned")
	}
}

func TestAnthropicCredentialWorkspaceIdIsOptional(t *testing.T) {
	tokenFile := validIdentityToken(t)
	auth := federatedAuth(tokenFile)
	auth[authKeyWorkspaceID] = "wrkspc_test"
	cfg := ProviderConfig{Name: "claudeTest", Auth: auth}

	_, path, err := anthropicCredential(cfg, guardedHTTPClient(nil))
	if err != nil {
		t.Fatalf("anthropicCredential with workspace id: %v", err)
	}
	if path != credentialPathFederation {
		t.Fatalf("credential path = %q, want %q", path, credentialPathFederation)
	}
}

func TestAnthropicCredentialRefusesPartialFederation(t *testing.T) {
	tokenFile := validIdentityToken(t)
	full := federatedAuth(tokenFile)

	// Every proper non-empty subset of the four required keys must refuse.
	// Enumerated rather than sampled: the whole point is that no partial
	// combination has a working path, and a spot-check would leave the one
	// that does undiscovered.
	keys := []string{
		authKeyFederationRuleID,
		authKeyOrganizationID,
		authKeyServiceAccountID,
		authKeyIdentityTokenFile,
	}
	for mask := 1; mask < (1<<len(keys))-1; mask++ {
		auth := map[string]string{}
		var set []string
		for i, k := range keys {
			if mask&(1<<i) != 0 {
				auth[k] = full[k]
				set = append(set, k)
			}
		}
		cfg := ProviderConfig{Name: "claudeTest", Auth: auth}
		_, _, err := anthropicCredential(cfg, guardedHTTPClient(nil))
		if err == nil {
			t.Fatalf("partial federation config %v was ACCEPTED; it must refuse", set)
		}
		if !strings.Contains(err.Error(), "HALF-CONFIGURED") {
			t.Fatalf("partial config %v: error does not say it is half-configured: %v", set, err)
		}
		// The message must name the missing variables -- an operator mid-cutover
		// needs to know which of the four they still owe.
		for i, k := range keys {
			if mask&(1<<i) == 0 {
				var envName string
				switch k {
				case authKeyFederationRuleID:
					envName = envAnthropicFederationRuleID
				case authKeyOrganizationID:
					envName = envAnthropicOrganizationID
				case authKeyServiceAccountID:
					envName = envAnthropicServiceAccountID
				case authKeyIdentityTokenFile:
					envName = envAnthropicIdentityTokenFile
				}
				if !strings.Contains(err.Error(), envName) {
					t.Fatalf("partial config %v: error does not name missing %s: %v", set, envName, err)
				}
			}
		}
	}
}

func TestAnthropicCredentialPartialFederationRefusesEvenWithAKey(t *testing.T) {
	// The regression this guards: falling back to the key when federation is
	// half-configured. It would pass every test, boot every node, and fail the
	// hour the cutover removed the key.
	cfg := ProviderConfig{Name: "claudeTest", Auth: map[string]string{
		authKeyAPIKey:           "sk-ant-test",
		authKeyFederationRuleID: "fdrl_test",
	}}
	_, _, err := anthropicCredential(cfg, guardedHTTPClient(nil))
	if err == nil {
		t.Fatal("half-configured federation fell back to the API key; it must refuse")
	}
}

func TestAnthropicCredentialFederationWinsOverKey(t *testing.T) {
	tokenFile := validIdentityToken(t)
	auth := federatedAuth(tokenFile)
	auth[authKeyAPIKey] = "sk-ant-test"
	cfg := ProviderConfig{Name: "claudeTest", Auth: auth}

	_, path, err := anthropicCredential(cfg, guardedHTTPClient(nil))
	if err != nil {
		t.Fatalf("anthropicCredential: %v", err)
	}
	if path != credentialPathFederation {
		t.Fatalf("with both configured the path is %q, want %q", path, credentialPathFederation)
	}
}

func TestAnthropicCredentialUsesTheKeyWhenNoFederation(t *testing.T) {
	cfg := ProviderConfig{Name: "claudeTest", Auth: map[string]string{authKeyAPIKey: "sk-ant-test"}}
	_, path, err := anthropicCredential(cfg, guardedHTTPClient(nil))
	if err != nil {
		t.Fatalf("anthropicCredential: %v", err)
	}
	if path != credentialPathAPIKey {
		t.Fatalf("credential path = %q, want %q", path, credentialPathAPIKey)
	}
}

func TestAnthropicCredentialRefusesWithNoCredentialAtAll(t *testing.T) {
	cfg := ProviderConfig{Name: "claudeTest", Auth: map[string]string{}}
	_, _, err := anthropicCredential(cfg, guardedHTTPClient(nil))
	if err == nil {
		t.Fatal("a provider with no credential was accepted")
	}
	// The seeding hint the auth resolver used to print has to survive here,
	// because the resolver no longer errors on this name.
	//
	// Asserted as a PROPERTY -- names the variable, names a real destination,
	// and directs the operator at no make target that does not exist -- rather
	// than as a literal, which is what let this assertion keep passing while
	// pointing operators at a target the Makefile has never had (memql#4338,
	// generalised in memql#4405).
	if !strings.Contains(err.Error(), envAnthropicAPIKey) {
		t.Fatalf("no-credential error does not name the variable to seed: %v", err)
	}
	if !strings.Contains(err.Error(), "globalSecret") {
		t.Fatalf("no-credential error lost the seeding hint: %v", err)
	}
	citesNoDeadMakeTarget(t, "the no-credential error", err.Error())
}

// TestAnthropicConstructorsAttachTheGuardedClient covers spec 6.1's last
// clause. Both constructors must route through the LLM circuit breaker in
// every credential branch -- the federation branch included, which is the one
// that could plausibly have been written to bypass it.
func TestAnthropicConstructorsAttachTheGuardedClient(t *testing.T) {
	tokenFile := validIdentityToken(t)
	for _, tc := range []struct {
		name string
		auth map[string]string
	}{
		{"federation", federatedAuth(tokenFile)},
		{"apiKey", map[string]string{authKeyAPIKey: "sk-ant-test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ProviderConfig{Name: "claudeTest", Model: "claude-test", Auth: tc.auth}

			if _, err := newAnthropicProvider(cfg); err != nil {
				t.Fatalf("newAnthropicProvider: %v", err)
			}
			if _, err := newAnthropicStreamProvider(cfg); err != nil {
				t.Fatalf("newAnthropicStreamProvider: %v", err)
			}

			// The credential helper is the single seam both constructors use,
			// so asserting on it covers both. guardedHTTPClient is what the
			// constructors pass; assert the shape it produces is the guarded
			// transport rather than a bare one.
			guarded := guardedHTTPClient(nil)
			if _, ok := guarded.Transport.(*guardedTransport); !ok {
				t.Fatalf("guardedHTTPClient did not produce a guarded transport: %T", guarded.Transport)
			}
			if _, _, err := anthropicCredential(cfg, guarded); err != nil {
				t.Fatalf("anthropicCredential: %v", err)
			}
		})
	}
}

// --- preflight (spec 6.3) --------------------------------------------------

func TestPreflightIdentityTokenAcceptsAProjectedToken(t *testing.T) {
	if err := preflightIdentityToken(validIdentityToken(t)); err != nil {
		t.Fatalf("a well-formed projected token was refused: %v", err)
	}
}

func TestPreflightIdentityTokenRefusals(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")

	notAJWT := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(notAJWT, []byte("not-a-jwt"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	wrongAudience := fixtureIdentityToken(t, map[string]any{
		// What a pod gets when the projected volume's `audience` field is
		// wrong or absent: a perfectly valid token for the API server.
		"aud": []any{"https://kubernetes.default.svc.cluster.local"},
		"sub": "system:serviceaccount:memql:memql-engine",
	})

	notAServiceAccount := fixtureIdentityToken(t, map[string]any{
		"aud": []any{anthropicAudience},
		"sub": "some-human@example.com",
	})

	for _, tc := range []struct {
		name     string
		path     string
		wantSaid string
	}{
		{"missing file", missing, "cannot read the projected identity token"},
		{"not a jwt", notAJWT, "is not a JWT"},
		{"wrong audience", wrongAudience, anthropicAudience},
		{"subject is not a service account", notAServiceAccount, "system:serviceaccount:"},
		{"empty path", "", envAnthropicIdentityTokenFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := preflightIdentityToken(tc.path)
			if err == nil {
				t.Fatal("preflight accepted a token it must refuse")
			}
			if !strings.Contains(err.Error(), tc.wantSaid) {
				t.Fatalf("preflight message does not name the problem (%q): %v", tc.wantSaid, err)
			}
		})
	}
}

// TestPreflightRefusesAtConstruction proves the check runs at construction
// rather than at first call: a provider whose token file is wrong must not
// register successfully and fail hours later on a user turn.
func TestPreflightRefusesAtConstruction(t *testing.T) {
	auth := federatedAuth(filepath.Join(t.TempDir(), "absent"))
	cfg := ProviderConfig{Name: "claudeTest", Model: "claude-test", Auth: auth}
	if _, err := newAnthropicProvider(cfg); err == nil {
		t.Fatal("newAnthropicProvider accepted a federation config whose token file does not exist")
	}
	if _, err := newAnthropicStreamProvider(cfg); err == nil {
		t.Fatal("newAnthropicStreamProvider accepted a federation config whose token file does not exist")
	}
}

func TestJWTAudienceAcceptsBothRFC7519Shapes(t *testing.T) {
	// RFC 7519 allows `aud` to be a string OR an array. Kubernetes emits the
	// array; another issuer may not, and reading only one shape would work in
	// the cluster and fail everywhere else.
	single := map[string]any{"aud": anthropicAudience}
	if !jwtAudienceContains(single, anthropicAudience) {
		t.Fatal("single-string aud not recognized")
	}
	array := map[string]any{"aud": []any{"other", anthropicAudience}}
	if !jwtAudienceContains(array, anthropicAudience) {
		t.Fatal("array aud not recognized")
	}
	if jwtAudienceContains(map[string]any{"aud": []any{"other"}}, anthropicAudience) {
		t.Fatal("a token for another audience was accepted")
	}
}

// --- auth placeholder resolution (spec 6, AC 1 of #4334) -------------------

// TestAnthropicAuthPlaceholdersAllResolve is the acceptance criterion that the
// parser needs no change: all six keys of the provider's auth block travel
// through resolveAuthPlaceholders unchanged in kind.
func TestAnthropicAuthPlaceholdersAllResolve(t *testing.T) {
	names := map[string]string{
		authKeyAPIKey:            envAnthropicAPIKey,
		authKeyFederationRuleID:  envAnthropicFederationRuleID,
		authKeyOrganizationID:    envAnthropicOrganizationID,
		authKeyServiceAccountID:  envAnthropicServiceAccountID,
		authKeyWorkspaceID:       envAnthropicWorkspaceID,
		authKeyIdentityTokenFile: envAnthropicIdentityTokenFile,
	}
	values := map[string]string{}
	for key, envName := range names {
		values[key] = "${" + envName + "}"
		t.Setenv(envName, "value-of-"+envName)
	}

	resolved, err := resolveAuthPlaceholders(values)
	if err != nil {
		t.Fatalf("resolveAuthPlaceholders: %v", err)
	}
	for key, envName := range names {
		if got, want := resolved[key], "value-of-"+envName; got != want {
			t.Fatalf("auth[%q] = %q, want %q", key, got, want)
		}
	}
}

// TestOptionalAuthPlaceholdersResolveToAbsent is the other half, and the one
// that keeps `make up` working: on a local cluster the four federation ids are
// unset, and an unresolved placeholder normally takes its whole provider out
// of the registry.
func TestOptionalAuthPlaceholdersResolveToAbsent(t *testing.T) {
	for _, envName := range optionalAuthEnvNameList() {
		t.Setenv(envName, "")
	}
	values := map[string]string{
		authKeyAPIKey:            "${" + envAnthropicAPIKey + "}",
		authKeyFederationRuleID:  "${" + envAnthropicFederationRuleID + "}",
		authKeyOrganizationID:    "${" + envAnthropicOrganizationID + "}",
		authKeyServiceAccountID:  "${" + envAnthropicServiceAccountID + "}",
		authKeyWorkspaceID:       "${" + envAnthropicWorkspaceID + "}",
		authKeyIdentityTokenFile: "${" + envAnthropicIdentityTokenFile + "}",
	}
	resolved, err := resolveAuthPlaceholders(values)
	if err != nil {
		t.Fatalf("unset optional placeholders failed the provider: %v", err)
	}
	for key := range values {
		if v, ok := resolved[key]; ok {
			t.Fatalf("auth[%q] resolved to %q; an unset optional placeholder must be absent", key, v)
		}
	}
}

// TestNonOptionalPlaceholdersStillFail holds the line on the other 190-odd
// registry names: optionality is an allow-list, not a new default.
func TestNonOptionalPlaceholdersStillFail(t *testing.T) {
	t.Setenv("MEMQL_AI_OPENAI_API_KEY", "")
	_, err := resolveAuthPlaceholders(map[string]string{"apiKey": "${MEMQL_AI_OPENAI_API_KEY}"})
	if err == nil {
		t.Fatal("an unset non-optional placeholder resolved silently")
	}
	// Same property, same reason as above (memql#4338 / memql#4405): a real
	// destination, and never a make target that does not exist.
	if !strings.Contains(err.Error(), "globalSecret") {
		t.Fatalf("the resolver's seeding hint was lost: %v", err)
	}
	citesNoDeadMakeTarget(t, "the resolver's seeding hint", err.Error())
}

// TestOptionalAuthNamesAreAnthropicCredentialOnly pins the allow-list. Adding
// a name here makes an absent value silent for that variable, which is only
// ever right where a constructor takes over the decision.
func TestOptionalAuthNamesAreAnthropicCredentialOnly(t *testing.T) {
	want := []string{
		envAnthropicAPIKey,
		envAnthropicFederationRuleID,
		envAnthropicIdentityTokenFile,
		envAnthropicOrganizationID,
		envAnthropicServiceAccountID,
		envAnthropicWorkspaceID,
	}
	got := optionalAuthEnvNameList()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("optional auth names = %v, want %v", got, want)
	}
}
