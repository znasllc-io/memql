package identity

import (
	"context"
	"errors"
	"testing"
)

// fakeGuardStore is a hand-rolled BootstrapGuardStore. Each field
// drives one read; the *Err fields inject a DB failure so the
// fail-safe path can be exercised.
type fakeGuardStore struct {
	bootstrapped    bool
	bootstrappedErr error

	hasOwner    bool
	hasOwnerErr error

	// memql#3591: an owner who has actually authenticated, as opposed to one a
	// bootstrap merely named.
	hasClaimedOwner    bool
	hasClaimedOwnerErr error

	settings    *ClusterSettingsRow
	settingsErr error
}

func (f *fakeGuardStore) IsClusterBootstrappedE(_ context.Context) (bool, error) {
	return f.bootstrapped, f.bootstrappedErr
}

func (f *fakeGuardStore) HasOwnerUser(_ context.Context) (bool, error) {
	return f.hasOwner, f.hasOwnerErr
}

func (f *fakeGuardStore) HasClaimedOwner(_ context.Context) (bool, error) {
	return f.hasClaimedOwner, f.hasClaimedOwnerErr
}

func (f *fakeGuardStore) ReadClusterSettings(_ context.Context) (*ClusterSettingsRow, error) {
	return f.settings, f.settingsErr
}

func TestEvaluateAutoBootstrap(t *testing.T) {
	someErr := errors.New("boom: 53300 connection storm")

	cases := []struct {
		name       string
		store      *fakeGuardStore
		wantAction BootstrapAction
		wantErr    bool
	}{
		{
			// Steady state: bootstrappedAt stamped -> nothing to do.
			name:       "already bootstrapped -> skip, no email",
			store:      &fakeGuardStore{bootstrapped: true},
			wantAction: BootstrapActionSkip,
		},
		{
			// memql#1864 core case: the owner has signed in, stamp went
			// missing. Must self-heal (stamp) and NOT email.
			//
			// Keyed on hasClaimedOwner since memql#3591: an owner ROW is now
			// written by the env bootstrap, so only an owner who has actually
			// authenticated proves the cluster was claimed.
			name:       "owner has signed in + bootstrappedAt empty -> self-heal, no email",
			store:      &fakeGuardStore{bootstrapped: false, hasClaimedOwner: true},
			wantAction: BootstrapActionSelfHeal,
		},
		{
			// memql#1829 idempotency: row exists, no owner yet. Email
			// already sent on the boot that created the row; suppress.
			name: "clusterSettings row present, nobody has signed in -> suppress, no email",
			store: &fakeGuardStore{
				bootstrapped:    false,
				hasClaimedOwner: false,
				settings:        &ClusterSettingsRow{BootstrapEmail: "owner@example.com"},
			},
			wantAction: BootstrapActionSuppress,
		},
		{
			// Genuine first boot: no stamp, no owner, no row, reads ok.
			name:       "truly fresh cluster -> send exactly one email",
			store:      &fakeGuardStore{bootstrapped: false, hasClaimedOwner: false, settings: nil},
			wantAction: BootstrapActionSend,
		},
		{
			// Fail-safe: bootstrapped read errors -> NO email.
			name:       "IsClusterBootstrappedE error -> fail-safe, no email",
			store:      &fakeGuardStore{bootstrappedErr: someErr},
			wantAction: BootstrapActionSkip,
			wantErr:    true,
		},
		{
			// Fail-safe: the claim check errors -> NO email.
			name:       "HasClaimedOwner error -> fail-safe, no email",
			store:      &fakeGuardStore{hasClaimedOwnerErr: someErr},
			wantAction: BootstrapActionSkip,
			wantErr:    true,
		},
		{
			// Fail-safe: ReadClusterSettings errors -> NO email.
			name:       "ReadClusterSettings error -> fail-safe, no email",
			store:      &fakeGuardStore{settingsErr: someErr},
			wantAction: BootstrapActionSkip,
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action, err := EvaluateAutoBootstrap(context.Background(), tc.store)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error (fail-safe), got nil; action=%v", action)
				}
				// On error the caller must never send the email; the
				// returned action must be the inert Skip.
				if action != BootstrapActionSkip {
					t.Fatalf("on error want action Skip, got %v", action)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if action != tc.wantAction {
				t.Fatalf("action = %v, want %v", action, tc.wantAction)
			}
		})
	}
}

// TestEvaluateAutoBootstrap_NilStore guards the defensive nil check.
func TestEvaluateAutoBootstrap_NilStore(t *testing.T) {
	action, err := EvaluateAutoBootstrap(context.Background(), nil)
	if err == nil {
		t.Fatalf("expected error for nil store")
	}
	if action != BootstrapActionSkip {
		t.Fatalf("nil store want Skip, got %v", action)
	}
}

// TestEvaluateAutoBootstrapDistinguishesANamedOwnerFromAClaimedOne --
// znasllc-io/memql#3591.
//
// The install now writes the owner USER row when it bootstraps from env, so the
// cluster has a named owner from the moment it is created and a passkey-enrolment
// link can be minted for them. That breaks the assumption this guard was built
// on: it read "an owner user exists" as *definitional proof the cluster was
// claimed*, which was sound only while the sole way a user row could appear was
// somebody logging in.
//
// A row that a bootstrap wrote is not a claim. Treating it as one would stamp
// `bootstrappedAt` on the next boot -- marking the cluster claimed before anybody
// had claimed it, taking /setup away as a fallback, and doing it silently.
//
// The predicate is therefore CREDENTIALS, not rows: an owner who has a magic-link
// or passkey identity has authenticated, by either route, and an owner who has
// none has never signed in. That is the fact the guard actually wanted.
func TestEvaluateAutoBootstrapDistinguishesANamedOwnerFromAClaimedOne(t *testing.T) {
	cases := []struct {
		name       string
		store      *fakeGuardStore
		wantAction BootstrapAction
	}{
		{
			// THE NEW CASE. The install named the owner and issued the link; nobody
			// has clicked it. The claim email was already sent by the boot that
			// wrote the clusterSettings row, so this must SUPPRESS -- not self-heal
			// (the cluster is not claimed) and not send again (memql#1829).
			name:       "owner named by the bootstrap, never signed in -> suppress",
			store:      &fakeGuardStore{bootstrapped: false, hasClaimedOwner: false, settings: &ClusterSettingsRow{}},
			wantAction: BootstrapActionSuppress,
		},
		{
			// memql#1864 unchanged: a CLAIMED owner with the stamp missing still
			// self-heals. This is the case the guard exists for, and the fix must
			// not cost it.
			name:       "owner has signed in, stamp missing -> self-heal",
			store:      &fakeGuardStore{bootstrapped: false, hasClaimedOwner: true},
			wantAction: BootstrapActionSelfHeal,
		},
		{
			// A genuinely fresh cluster: nothing written at all.
			name:       "nothing yet -> send",
			store:      &fakeGuardStore{bootstrapped: false, hasClaimedOwner: false},
			wantAction: BootstrapActionSend,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvaluateAutoBootstrap(context.Background(), tc.store)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantAction {
				t.Errorf("action = %v, want %v", got, tc.wantAction)
			}
		})
	}
}

// The fail-safe contract covers the new read too: "cannot tell" must never
// become "fresh cluster, send the email".
func TestEvaluateAutoBootstrapFailsSafeWhenTheClaimReadErrors(t *testing.T) {
	store := &fakeGuardStore{bootstrapped: false, hasClaimedOwnerErr: errors.New("boom: 53300")}
	got, err := EvaluateAutoBootstrap(context.Background(), store)
	if err == nil {
		t.Fatalf("action = %v with no error; a failed read must surface so the caller does not email", got)
	}
	if got != BootstrapActionSkip {
		t.Errorf("action = %v, want Skip alongside the error", got)
	}
}
