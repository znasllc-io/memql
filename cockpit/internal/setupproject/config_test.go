package setupproject

import (
	"reflect"
	"testing"
)

func TestConfigValidate(t *testing.T) {
	base := Config{Product: "acme", ProductOrg: "acme-io", CreateRepos: "none"}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid", func(*Config) {}, false},
		{"valid with domain", func(c *Config) { c.Domain = "acme.local" }, false},
		{"valid github create", func(c *Config) { c.CreateRepos = "github" }, false},
		{"empty product", func(c *Config) { c.Product = "" }, true},
		{"uppercase product", func(c *Config) { c.Product = "Acme" }, true},
		{"leading digit product", func(c *Config) { c.Product = "1acme" }, true},
		{"underscore product", func(c *Config) { c.Product = "ac_me" }, true},
		{"reserved product memql", func(c *Config) { c.Product = "memql" }, true},
		{"reserved product cockpit", func(c *Config) { c.Product = "memql-cockpit" }, true},
		{"empty org", func(c *Config) { c.ProductOrg = "" }, true},
		{"bad org char", func(c *Config) { c.ProductOrg = "acme/io" }, true},
		{"bad domain", func(c *Config) { c.Domain = "not a domain" }, true},
		{"bad create-repos", func(c *Config) { c.CreateRepos = "gitlab" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base
			tt.mutate(&c)
			err := c.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestConfigWithDefaults(t *testing.T) {
	got := Config{Product: "acme", ProductOrg: "acme-io"}.withDefaults()
	if got.Domain != defaultDomain {
		t.Errorf("Domain default = %q, want %q", got.Domain, defaultDomain)
	}
	if got.CreateRepos != "none" {
		t.Errorf("CreateRepos default = %q, want none", got.CreateRepos)
	}
	if got.TemplateRepo != defaultTemplateRepo {
		t.Errorf("TemplateRepo default = %q, want %q", got.TemplateRepo, defaultTemplateRepo)
	}

	// Explicit values are preserved.
	explicit := Config{
		Product: "acme", ProductOrg: "acme-io",
		Domain: "x.local", CreateRepos: "github", TemplateRepo: "file:///tmp/t",
	}.withDefaults()
	if explicit.Domain != "x.local" || explicit.CreateRepos != "github" || explicit.TemplateRepo != "file:///tmp/t" {
		t.Errorf("withDefaults clobbered explicit values: %+v", explicit)
	}
}

func TestBootstrapArgs(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want []string
	}{
		{
			name: "minimal",
			cfg:  Config{Product: "acme", ProductOrg: "acme-io", Domain: "memql.localhost", CreateRepos: "none"},
			want: []string{"--product=acme", "--product-org=acme-io", "--domain=memql.localhost", "--create-repos=none"},
		},
		{
			name: "engine version pinned",
			cfg:  Config{Product: "acme", ProductOrg: "acme-io", Domain: "d", EngineVersion: "v1.2.3", CreateRepos: "none"},
			want: []string{"--product=acme", "--product-org=acme-io", "--domain=d", "--engine-version=v1.2.3", "--create-repos=none"},
		},
		{
			name: "all flags",
			cfg: Config{
				Product: "acme", ProductOrg: "acme-io", Domain: "d", EngineVersion: "main",
				CreateRepos: "github", Shallow: true, DryRun: true,
			},
			want: []string{
				"--product=acme", "--product-org=acme-io", "--domain=d",
				"--engine-version=main", "--create-repos=github", "--shallow", "--dry-run",
			},
		},
		{
			name: "cockpit-only flags are NOT forwarded",
			cfg: Config{
				Product: "acme", ProductOrg: "acme-io", Domain: "d", CreateRepos: "none",
				Dir: "/tmp/ws", TemplateRef: "v9", TemplateRepo: "file:///x",
			},
			want: []string{"--product=acme", "--product-org=acme-io", "--domain=d", "--create-repos=none"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.BootstrapArgs()
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("BootstrapArgs()\n got=%v\nwant=%v", got, tt.want)
			}
		})
	}
}

func TestFrontDoorURLs(t *testing.T) {
	got := FrontDoorURLs("acme.local")
	want := []string{"https://identity.acme.local", "https://bff.acme.local", "https://app.acme.local"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FrontDoorURLs = %v, want %v", got, want)
	}
	// Empty domain falls back to the default.
	if got := FrontDoorURLs(""); got[0] != "https://identity."+defaultDomain {
		t.Errorf("FrontDoorURLs(\"\")[0] = %q, want default-based", got[0])
	}
}
