package setupproject

import (
	"testing"
)

func TestParseProjectFlags(t *testing.T) {
	t.Run("equals and space forms", func(t *testing.T) {
		cfg, help, err := parseProjectFlags([]string{
			"--product=acme", "--product-org", "acme-io",
			"--domain=acme.local", "--engine-version", "v1.2.3",
			"--dir=/tmp/ws", "--create-repos=github",
			"--template-ref", "v9", "--template-repo=file:///t",
			"--shallow", "--dry-run",
		})
		if err != nil || help {
			t.Fatalf("unexpected err=%v help=%v", err, help)
		}
		want := Config{
			Product: "acme", ProductOrg: "acme-io", Domain: "acme.local",
			EngineVersion: "v1.2.3", Dir: "/tmp/ws", CreateRepos: "github",
			TemplateRef: "v9", TemplateRepo: "file:///t", Shallow: true, DryRun: true,
		}
		if cfg != want {
			t.Errorf("cfg\n got=%+v\nwant=%+v", cfg, want)
		}
	})

	t.Run("help", func(t *testing.T) {
		_, help, err := parseProjectFlags([]string{"--help"})
		if err != nil || !help {
			t.Fatalf("--help: err=%v help=%v", err, help)
		}
	})

	t.Run("unknown flag", func(t *testing.T) {
		if _, _, err := parseProjectFlags([]string{"--nope"}); err == nil {
			t.Fatalf("expected error for unknown flag")
		}
	})

	t.Run("missing value", func(t *testing.T) {
		if _, _, err := parseProjectFlags([]string{"--product"}); err == nil {
			t.Fatalf("expected error for flag missing its value")
		}
	})
}

func TestDispatch(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no args", nil, exitUsage},
		{"help", []string{"help"}, exitOK},
		{"help flag", []string{"--help"}, exitOK},
		{"unknown target", []string{"widget"}, exitUsage},
		{"project help", []string{"project", "--help"}, exitOK},
		{"project unknown flag", []string{"project", "--nope"}, exitUsage},
		{"project bad create-repos", []string{"project", "--product=acme", "--product-org=acme-io", "--create-repos=gitlab"}, exitUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dispatch(tt.args); got != tt.want {
				t.Errorf("dispatch(%v) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

func TestNonInteractiveWithoutTTYIsUsageError(t *testing.T) {
	// Force the non-TTY branch regardless of how the test is run.
	orig := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	defer func() { stdinIsTTY = orig }()

	// Required flags absent + non-TTY => usage error, never blocks on a prompt.
	if got := handleProject([]string{"--domain=x.local"}); got != exitUsage {
		t.Errorf("handleProject without required flags on non-TTY = %d, want %d", got, exitUsage)
	}
}
