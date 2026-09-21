package githubconnect

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// resolve_test.go -- which tier the cluster's GitHub App comes from (design
// record 2026-09-20-github-app-setup, D2).
//
// The rule is short and every half of it has a way to be wrong that still
// looks like it works: rows quietly overriding a deployment's environment, a
// partial environment patched up from rows, a secret read out of the plaintext
// store. Each gets a test that names the defect.

// clearEnv blanks the six for one test. t.Setenv rather than os.Unsetenv, so
// the values an outer environment really carries come back afterwards.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range EnvNames() {
		t.Setenv(name, "")
	}
}

// fakeRows is the two stores as two maps, recording what was asked of which.
type fakeRows struct {
	variables map[string]string
	secrets   map[string]string
	asked     []string
}

func (f *fakeRows) reader() RowReader {
	notFound := errors.New("not found")
	return RowReader{
		Variable: func(_ context.Context, name string) (string, error) {
			f.asked = append(f.asked, "variable:"+name)
			v, ok := f.variables[name]
			if !ok {
				return "", notFound
			}
			return v, nil
		},
		Secret: func(_ context.Context, name string) (string, error) {
			f.asked = append(f.asked, "secret:"+name)
			v, ok := f.secrets[name]
			if !ok {
				return "", notFound
			}
			return v, nil
		},
	}
}

func storedApp() *fakeRows {
	c := fullConfig()
	return &fakeRows{
		variables: map[string]string{EnvAppID: c.AppID, EnvAppSlug: c.AppSlug, EnvClientID: c.ClientID},
		secrets:   map[string]string{EnvClientSecret: c.ClientSecret, EnvPrivateKeyB64: c.PrivateKeyB64, EnvWebhookSecret: c.WebhookSecret},
	}
}

func TestNoEnvironmentAndNoRowsIsNoApp(t *testing.T) {
	clearEnv(t)
	cfg, source := Resolve(context.Background(), (&fakeRows{}).reader())
	if source != SourceNone || cfg.Configured() {
		t.Fatalf("a cluster with nothing anywhere resolved an app: source=%q configured=%v", source, cfg.Configured())
	}
}

func TestARegistrationInTheRowsIsTheApp(t *testing.T) {
	clearEnv(t)
	rows := storedApp()
	cfg, source := Resolve(context.Background(), rows.reader())
	if source != SourceCluster {
		t.Fatalf("source = %q, want %q", source, SourceCluster)
	}
	if cfg != fullConfig() {
		t.Errorf("the stored app did not come back whole: %+v", cfg.Present())
	}
}

// TestEachNameIsReadFromItsOwnStore: a credential read out of the plaintext
// store is the defect readiness_eval.go's resolveSlot refuses, and this must
// agree with it or the Set up card and the Connect button disagree.
func TestEachNameIsReadFromItsOwnStore(t *testing.T) {
	clearEnv(t)
	rows := storedApp()
	Resolve(context.Background(), rows.reader())
	sort.Strings(rows.asked)
	want := []string{
		"secret:" + EnvClientSecret, "secret:" + EnvPrivateKeyB64, "secret:" + EnvWebhookSecret,
		"variable:" + EnvClientID, "variable:" + EnvAppID, "variable:" + EnvAppSlug,
	}
	sort.Strings(want)
	if strings.Join(rows.asked, ",") != strings.Join(want, ",") {
		t.Errorf("asked %v, want %v", rows.asked, want)
	}

	// And a secret sitting in the WRONG store does not count.
	misfiled := storedApp()
	misfiled.variables[EnvClientSecret] = misfiled.secrets[EnvClientSecret]
	delete(misfiled.secrets, EnvClientSecret)
	if _, source := Resolve(context.Background(), misfiled.reader()); source != SourceNone {
		t.Errorf("a client secret filed as a plaintext variable resolved an app (source=%q)", source)
	}
}

func TestFiveStoredRowsAreNoApp(t *testing.T) {
	clearEnv(t)
	for _, name := range EnvNames() {
		rows := storedApp()
		delete(rows.variables, name)
		delete(rows.secrets, name)
		if cfg, source := Resolve(context.Background(), rows.reader()); source != SourceNone || cfg.Configured() {
			t.Errorf("without %s the rows still resolved an app (source=%q)", name, source)
		}
	}
}

// A cleared row is a BLANK row -- a removal overwrites rather than deletes --
// and blank has to read as absent.
func TestABlankRowIsAnAbsentRow(t *testing.T) {
	clearEnv(t)
	rows := storedApp()
	rows.secrets[EnvPrivateKeyB64] = "   "
	if _, source := Resolve(context.Background(), rows.reader()); source != SourceNone {
		t.Errorf("a blank private key resolved an app (source=%q)", source)
	}
}

// TestTheEnvironmentWinsWhole is the property the whole file rests on: rows
// written from a browser must not change which app a deployment that SET one
// talks to.
func TestTheEnvironmentWinsWhole(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAppID, "999")
	t.Setenv(EnvAppSlug, "from-the-deployment")
	t.Setenv(EnvClientID, "Iv1.deployment")
	t.Setenv(EnvClientSecret, "deployment-secret")
	t.Setenv(EnvPrivateKeyB64, "ZGVwbG95bWVudA==")
	t.Setenv(EnvWebhookSecret, "deployment-hook")

	rows := storedApp()
	cfg, source := Resolve(context.Background(), rows.reader())
	if source != SourceEnvironment {
		t.Fatalf("source = %q, want %q", source, SourceEnvironment)
	}
	if cfg.AppSlug != "from-the-deployment" || cfg.ClientSecret != "deployment-secret" {
		t.Errorf("the environment's app was not the one resolved: slug=%q", cfg.AppSlug)
	}
	if len(rows.asked) != 0 {
		t.Errorf("rows were read although the environment answered: %v", rows.asked)
	}
}

// TestAPartialEnvironmentIsNotPatchedFromRows: one value in the environment is
// a statement about this cluster's app, and the identity node refuses boot on
// a partial one (Validate). Completing it from the rows would turn that
// refusal into a Connect button backed by two different registrations.
func TestAPartialEnvironmentIsNotPatchedFromRows(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvAppID, "999")
	rows := storedApp()
	cfg, source := Resolve(context.Background(), rows.reader())
	if source != SourceEnvironment {
		t.Fatalf("source = %q, want %q", source, SourceEnvironment)
	}
	if cfg.Configured() {
		t.Error("one environment value plus five rows resolved a whole app")
	}
	if cfg.Validate() == nil {
		t.Error("the partial environment stopped refusing boot")
	}
	if len(rows.asked) != 0 {
		t.Errorf("rows were read to complete a partial environment: %v", rows.asked)
	}
}

func TestANilReaderIsNoApp(t *testing.T) {
	clearEnv(t)
	if _, source := Resolve(context.Background(), RowReader{}); source != SourceNone {
		t.Errorf("no readers at all resolved an app (source=%q)", source)
	}
	var r *Resolver
	if _, source := r.Current(context.Background()); source != SourceNone {
		t.Errorf("a nil Resolver resolved an app (source=%q)", source)
	}
	r.Invalidate() // must not panic
}

func TestTheResolverRemembersForTenSecondsAndNoLonger(t *testing.T) {
	clearEnv(t)
	rows := &fakeRows{}
	clock := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	r := &Resolver{Rows: rows.reader(), Now: func() time.Time { return clock }}

	if _, source := r.Current(context.Background()); source != SourceNone {
		t.Fatalf("source = %q before anything was registered", source)
	}
	reads := len(rows.asked)

	// A registration lands on ANOTHER node: this one has not heard.
	registered := storedApp()
	rows.variables, rows.secrets = registered.variables, registered.secrets
	clock = clock.Add(resolverTTL - time.Second)
	if _, source := r.Current(context.Background()); source != SourceNone {
		t.Errorf("inside the TTL the answer changed (source=%q)", source)
	}
	if len(rows.asked) != reads {
		t.Errorf("inside the TTL the rows were read again")
	}

	// ...and catches up on its own once the answer is stale.
	clock = clock.Add(2 * time.Second)
	if _, source := r.Current(context.Background()); source != SourceCluster {
		t.Errorf("past the TTL the registration was not seen (source=%q)", source)
	}
}

func TestInvalidateIsHowTheWritingNodeSeesItsOwnWrite(t *testing.T) {
	clearEnv(t)
	rows := &fakeRows{}
	r := &Resolver{Rows: rows.reader()}
	r.Current(context.Background())

	registered := storedApp()
	rows.variables, rows.secrets = registered.variables, registered.secrets
	r.Invalidate()
	if _, source := r.Current(context.Background()); source != SourceCluster {
		t.Errorf("after Invalidate the node still answered from memory (source=%q)", source)
	}
}

// TestTheSecretSplitMatchesTheRegistry pins secretEnvNames to the registry's
// own `secret: true`. This package cannot import the registry, so the split is
// restated -- and a restated fact is one that drifts unless something reads
// both. It reads the AUTHORED manifest, the file a person edits.
func TestTheSecretSplitMatchesTheRegistry(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	manifest := filepath.Join(filepath.Dir(here), "..", "..", "..", "scripts", "secrets", "manifest.yaml")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("read the env registry: %v", err)
	}
	entry := regexp.MustCompile(`(?m)^  - name: (MEMQL_GITHUB_APP_[A-Z0-9_]+)\n((?:    .*\n)+)`)
	found := map[string]bool{}
	for _, m := range entry.FindAllStringSubmatch(string(raw), -1) {
		found[m[1]] = regexp.MustCompile(`(?m)^    secret: true$`).MatchString(m[2])
	}
	if len(found) != len(EnvNames()) {
		t.Fatalf("the registry declares %d MEMQL_GITHUB_APP_* entries, this package names %d", len(found), len(EnvNames()))
	}
	for _, name := range EnvNames() {
		secret, ok := found[name]
		if !ok {
			t.Errorf("%s is not in the registry", name)
			continue
		}
		if secret != IsSecretName(name) {
			t.Errorf("%s: registry says secret=%v, this package says %v -- it would be read from the wrong store", name, secret, IsSecretName(name))
		}
	}
}
