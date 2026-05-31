package main

import "testing"

// TestRunMigrateSubcommand_Help exits 0 for the help flags without touching
// the database (the arg loop returns before app.Build).
func TestRunMigrateSubcommand_Help(t *testing.T) {
	for _, arg := range []string{"--help", "-h", "help"} {
		if code := runMigrateSubcommand([]string{arg}); code != 0 {
			t.Errorf("migrate %s: got exit %d, want 0", arg, code)
		}
	}
}

// TestRunMigrateSubcommand_UnexpectedArg rejects unknown args with exit 2
// (a usage error) rather than proceeding to build + migrate.
func TestRunMigrateSubcommand_UnexpectedArg(t *testing.T) {
	if code := runMigrateSubcommand([]string{"--bogus"}); code != 2 {
		t.Errorf("migrate --bogus: got exit %d, want 2", code)
	}
}
