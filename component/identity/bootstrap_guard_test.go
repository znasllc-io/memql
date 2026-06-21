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

	settings    *ClusterSettingsRow
	settingsErr error
}

func (f *fakeGuardStore) IsClusterBootstrappedE(_ context.Context) (bool, error) {
	return f.bootstrapped, f.bootstrappedErr
}

func (f *fakeGuardStore) HasOwnerUser(_ context.Context) (bool, error) {
	return f.hasOwner, f.hasOwnerErr
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
			// memql#1864 core case: owner exists, stamp went missing.
			// Must self-heal (stamp) and NOT email.
			name:       "owner exists + bootstrappedAt empty -> self-heal, no email",
			store:      &fakeGuardStore{bootstrapped: false, hasOwner: true},
			wantAction: BootstrapActionSelfHeal,
		},
		{
			// memql#1829 idempotency: row exists, no owner yet. Email
			// already sent on the boot that created the row; suppress.
			name: "clusterSettings row present, no owner -> suppress, no email",
			store: &fakeGuardStore{
				bootstrapped: false,
				hasOwner:     false,
				settings:     &ClusterSettingsRow{BootstrapEmail: "znas@znas.io"},
			},
			wantAction: BootstrapActionSuppress,
		},
		{
			// Genuine first boot: no stamp, no owner, no row, reads ok.
			name:       "truly fresh cluster -> send exactly one email",
			store:      &fakeGuardStore{bootstrapped: false, hasOwner: false, settings: nil},
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
			// Fail-safe: owner-check errors -> NO email.
			name:       "HasOwnerUser error -> fail-safe, no email",
			store:      &fakeGuardStore{hasOwnerErr: someErr},
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
