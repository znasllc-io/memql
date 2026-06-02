package main

import "testing"

// TestRunPATSubcommand_Help exits 0 for the help flags without building the app
// or touching the database (the switch returns before any bootstrap).
func TestRunPATSubcommand_Help(t *testing.T) {
	for _, arg := range []string{"--help", "-h", "help"} {
		if code := runPATSubcommand([]string{arg}); code != 0 {
			t.Errorf("pat %s: got exit %d, want 0", arg, code)
		}
	}
}

// TestRunPATSubcommand_NoArgs prints usage and exits 2.
func TestRunPATSubcommand_NoArgs(t *testing.T) {
	if code := runPATSubcommand(nil); code != 2 {
		t.Errorf("pat (no args): got exit %d, want 2", code)
	}
}

// TestRunPATSubcommand_Unknown rejects an unknown subcommand with exit 2.
func TestRunPATSubcommand_Unknown(t *testing.T) {
	if code := runPATSubcommand([]string{"bogus"}); code != 2 {
		t.Errorf("pat bogus: got exit %d, want 2", code)
	}
}

// TestRunPATMint_RequiresUserId fails arg validation (exit 2) BEFORE building
// the app, so no database is needed. This pins the "user-id is required" guard.
func TestRunPATMint_RequiresUserId(t *testing.T) {
	if code := runPATMint([]string{"--label", "smoke-test"}); code != 2 {
		t.Errorf("pat mint without --user-id: got exit %d, want 2", code)
	}
	// Routed through the dispatcher with no flags at all.
	if code := runPATSubcommand([]string{"mint"}); code != 2 {
		t.Errorf("pat mint (no flags): got exit %d, want 2", code)
	}
}

// TestRunPATRevoke_RequiresId fails arg validation (exit 2) before any bootstrap.
func TestRunPATRevoke_RequiresId(t *testing.T) {
	if code := runPATRevoke(nil); code != 2 {
		t.Errorf("pat revoke without --id: got exit %d, want 2", code)
	}
}
