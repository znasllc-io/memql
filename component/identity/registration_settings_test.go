package identity

import (
	"errors"
	"testing"
)

func TestDefaultJoiningPolicyIsInvitationOnly(t *testing.T) {
	t.Setenv("MEMQL_IDENTITY_ENABLED", "false")
	t.Setenv("MEMQL_IDENTITY_REGISTRATION_MODE", "")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistrationMode != RegistrationModeInviteOnly {
		t.Fatalf("default registration mode = %s", cfg.RegistrationMode)
	}
}

func TestRegistrationPolicyDoesNotFallBackOnStorageFailure(t *testing.T) {
	store := &Store{Engine: &bootstrapFakeEngine{settingsErr: errors.New("unavailable")}}
	_, err := store.RegistrationConfig(t.Context(), Config{RegistrationMode: RegistrationModeOpen})
	if err == nil {
		t.Fatal("a failed saved-policy read reopened registration from boot defaults")
	}
}

func TestRegistrationListsRejectInvalidOrMissingDomains(t *testing.T) {
	for _, test := range []struct{ mode, approved, internal, notify string }{
		{"domain_restricted", "", "", ""},
		{"open", "", "https://example.com", ""},
		{"open", "", "*.example.com", ""},
		{"open", "bad..test", "", ""},
		{"waitlist", "", "", "someone"},
	} {
		if err := ValidateRegistrationLists(test.mode, test.approved, test.internal, test.notify); err == nil {
			t.Errorf("accepted invalid joining policy: %+v", test)
		}
	}
	if err := ValidateRegistrationLists("domain_restricted", " Example.com, team.test ", "example.com", "ops@example.com"); err != nil {
		t.Fatal(err)
	}
}
