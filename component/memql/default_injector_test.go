package memql

import (
	"context"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/genesis"
)

// fakeInjectStore is an in-memory injectStore. `present` seeds the
// only-if-absent check; every insert is recorded so the test can assert
// what was (and was NOT) written.
type fakeInjectStore struct {
	present       map[string]bool // keyed by entry name
	presentErr    error           // returned by valuePresent when non-nil
	insertErr     error           // returned by both inserts when non-nil
	insertedVars  []string
	insertedSecrs []string
}

func (f *fakeInjectStore) valuePresent(_ context.Context, _, name string) (bool, error) {
	if f.presentErr != nil {
		return false, f.presentErr
	}
	return f.present[name], nil
}

func (f *fakeInjectStore) insertVariableDefault(_ context.Context, c genesis.InjectableDefault) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.insertedVars = append(f.insertedVars, c.Entry.Name)
	return nil
}

func (f *fakeInjectStore) insertSecretDefault(_ context.Context, c genesis.InjectableDefault) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.insertedSecrs = append(f.insertedSecrs, c.Entry.Name)
	return nil
}

func testManifest() *genesis.Manifest {
	return &genesis.Manifest{
		Secrets: []genesis.ManifestEntry{
			{Name: "SECRET_DEFAULT", Scope: "global", Kind: "integration", Default: "s-default"},
			{Name: "SECRET_NO_DEFAULT", Scope: "global"}, // skipped: no default
		},
		Variables: []genesis.ManifestEntry{
			{Name: "VAR_DEFAULT", Scope: "global", Default: "v-default"},
			{Name: "NODE_VAR", Scope: "node", Default: "n"}, // skipped: node scope
		},
	}
}

// TestInjectDefaults_EmptyStore_InsertsAllDefaults verifies that against
// an empty store every injectable default (and only those) is inserted.
func TestInjectDefaults_EmptyStore_InsertsAllDefaults(t *testing.T) {
	store := &fakeInjectStore{present: map[string]bool{}}
	di := newDefaultInjectorWithStore(store, testManifest(), nil)

	rep, err := di.InjectDefaults(context.Background())
	if err != nil {
		t.Fatalf("InjectDefaults: %v", err)
	}

	if rep.Candidates != 2 {
		t.Errorf("candidates: got %d, want 2 (SECRET_DEFAULT + VAR_DEFAULT)", rep.Candidates)
	}
	if rep.Injected != 2 {
		t.Errorf("injected: got %d, want 2", rep.Injected)
	}
	if rep.Skipped != 0 || rep.Failed != 0 {
		t.Errorf("skipped/failed: got %d/%d, want 0/0", rep.Skipped, rep.Failed)
	}
	if len(store.insertedVars) != 1 || store.insertedVars[0] != "VAR_DEFAULT" {
		t.Errorf("inserted vars: got %v, want [VAR_DEFAULT]", store.insertedVars)
	}
	if len(store.insertedSecrs) != 1 || store.insertedSecrs[0] != "SECRET_DEFAULT" {
		t.Errorf("inserted secrets: got %v, want [SECRET_DEFAULT]", store.insertedSecrs)
	}
}

// TestInjectDefaults_ValuePresent_IsNoOp verifies the only-if-absent
// contract: an operator value that already exists is never overwritten.
func TestInjectDefaults_ValuePresent_IsNoOp(t *testing.T) {
	// Both candidates already have an operator-set value.
	store := &fakeInjectStore{present: map[string]bool{
		"SECRET_DEFAULT": true,
		"VAR_DEFAULT":    true,
	}}
	di := newDefaultInjectorWithStore(store, testManifest(), nil)

	rep, err := di.InjectDefaults(context.Background())
	if err != nil {
		t.Fatalf("InjectDefaults: %v", err)
	}

	if rep.Injected != 0 {
		t.Errorf("injected: got %d, want 0 (all present)", rep.Injected)
	}
	if rep.Skipped != 2 {
		t.Errorf("skipped: got %d, want 2", rep.Skipped)
	}
	if len(store.insertedVars) != 0 || len(store.insertedSecrs) != 0 {
		t.Errorf("expected zero inserts, got vars=%v secrets=%v", store.insertedVars, store.insertedSecrs)
	}
}

// TestInjectDefaults_Idempotent_SecondRunIsNoOp models a re-deploy: the
// first run inserts the defaults, then the store reports them present,
// so the second run inserts nothing.
func TestInjectDefaults_Idempotent_SecondRunIsNoOp(t *testing.T) {
	store := &fakeInjectStore{present: map[string]bool{}}
	di := newDefaultInjectorWithStore(store, testManifest(), nil)

	rep1, err := di.InjectDefaults(context.Background())
	if err != nil {
		t.Fatalf("first InjectDefaults: %v", err)
	}
	if rep1.Injected != 2 {
		t.Fatalf("first run injected: got %d, want 2", rep1.Injected)
	}

	// Simulate the rows now existing (what a real store would report on
	// the next boot).
	store.present["SECRET_DEFAULT"] = true
	store.present["VAR_DEFAULT"] = true
	store.insertedVars = nil
	store.insertedSecrs = nil

	rep2, err := di.InjectDefaults(context.Background())
	if err != nil {
		t.Fatalf("second InjectDefaults: %v", err)
	}
	if rep2.Injected != 0 {
		t.Errorf("second run injected: got %d, want 0 (idempotent)", rep2.Injected)
	}
	if rep2.Skipped != 2 {
		t.Errorf("second run skipped: got %d, want 2", rep2.Skipped)
	}
	if len(store.insertedVars) != 0 || len(store.insertedSecrs) != 0 {
		t.Errorf("second run wrote rows: vars=%v secrets=%v", store.insertedVars, store.insertedSecrs)
	}
}

// TestInjectDefaults_ExistenceCheckError_IsConservative verifies that an
// inconclusive existence check NEVER triggers an insert (could clobber a
// value the check just failed to read) -- it is counted as failed.
func TestInjectDefaults_ExistenceCheckError_IsConservative(t *testing.T) {
	store := &fakeInjectStore{
		present:    map[string]bool{},
		presentErr: errors.New("db hiccup"),
	}
	di := newDefaultInjectorWithStore(store, testManifest(), nil)

	rep, err := di.InjectDefaults(context.Background())
	if err != nil {
		t.Fatalf("InjectDefaults: %v", err)
	}
	if rep.Injected != 0 {
		t.Errorf("injected on existence-check error: got %d, want 0", rep.Injected)
	}
	if rep.Failed != 2 {
		t.Errorf("failed: got %d, want 2", rep.Failed)
	}
	if len(store.insertedVars) != 0 || len(store.insertedSecrs) != 0 {
		t.Errorf("conservative path still wrote rows: vars=%v secrets=%v", store.insertedVars, store.insertedSecrs)
	}
}

// TestInjectConceptName maps the (secret, scope) axes to the right
// v1:platform:* concept.
func TestInjectConceptName(t *testing.T) {
	cases := []struct {
		isSecret bool
		scope    string
		want     string
	}{
		{true, "global", "v1:platform:globalSecret"},
		{true, "partition", "v1:platform:partitionSecret"},
		{false, "global", "v1:platform:globalVariable"},
		{false, "partition", "v1:platform:partitionVariable"},
	}
	for _, c := range cases {
		got := injectConceptName(genesis.InjectableDefault{
			Entry:    genesis.ManifestEntry{Scope: c.scope},
			IsSecret: c.isSecret,
		})
		if got != c.want {
			t.Errorf("injectConceptName(secret=%v, scope=%q) = %q, want %q", c.isSecret, c.scope, got, c.want)
		}
	}
}

// TestInjectMutationIds confirms the injected row id matches the
// operator seed tool's id formula so the two paths converge on one row.
func TestInjectMutationIds(t *testing.T) {
	gv := genesis.InjectableDefault{Entry: genesis.ManifestEntry{Name: "EMAIL_FROM_NAME", Scope: "global"}}
	if mut, id := variableMutationFor(gv); mut != "setGlobalVariable" || id != "var-global-email-from-name" {
		t.Errorf("global var: got (%q,%q), want (setGlobalVariable, var-global-email-from-name)", mut, id)
	}
	pv := genesis.InjectableDefault{Entry: genesis.ManifestEntry{Name: "FOO_BAR", Scope: "partition"}}
	if mut, id := variableMutationFor(pv); mut != "setPartitionVariable" || id != "var-foo-bar" {
		t.Errorf("partition var: got (%q,%q), want (setPartitionVariable, var-foo-bar)", mut, id)
	}
	gs := genesis.InjectableDefault{Entry: genesis.ManifestEntry{Name: "SOME_SECRET", Scope: "global"}, IsSecret: true}
	if mut, id := secretMutationFor(gs); mut != "setGlobalSecret" || id != "secret-global-some-secret" {
		t.Errorf("global secret: got (%q,%q), want (setGlobalSecret, secret-global-some-secret)", mut, id)
	}
}
