package setupproject

import (
	"fmt"
	"regexp"
	"strings"
)

// defaultTemplateRepo is the memql-project template the command clones and
// stamps. The template repo is the single source of truth for the payload
// (owner decision, memql#2448): the cockpit CLONES it rather than embedding a
// go:embed payload, so template fixes ship without a cockpit release.
const defaultTemplateRepo = "https://github.com/znasllc-io/memql-project.git"

// defaultDomain is the local front-door domain a stamped workspace defaults to
// (mkcert wildcard). It is passed explicitly to the template bootstrap.
const defaultDomain = "memql.localhost"

// Validation regexes mirror scripts/bootstrap.sh's validate_params so the two
// front-ends (script for machines/CI, cockpit for humans) reject the same
// inputs. Keep these in sync with the template repo's bootstrap.
var (
	productRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	orgRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	domainRe  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)
)

// reservedProducts collides with the workspace checkout names bootstrap.sh
// clones as siblings; a product may not take one of them.
var reservedProducts = map[string]struct{}{
	"memql":         {},
	"memql-cockpit": {},
}

// Config is the fully-resolved input to one `setup project` run. The four
// product identity fields (Product / ProductOrg / Domain / EngineVersion) are
// what the wizard collects; the rest govern how the template is cloned and how
// bootstrap is driven.
type Config struct {
	Product       string // product slug (^[a-z][a-z0-9-]*$)
	ProductOrg    string // GitHub org/user owning the stamped product repos
	Domain        string // local front-door domain
	EngineVersion string // engine ref to pin; empty => bootstrap resolves latest tag/main

	Dir          string // target workspace dir for a real run; empty => cwd
	CreateRepos  string // none | github
	TemplateRef  string // memql-project ref to clone (branch/tag/sha); empty => default branch
	TemplateRepo string // memql-project clone URL (override for tests / forks)
	Shallow      bool   // shallow-clone the template + engine/cockpit (CI smoke)
	DryRun       bool   // report the stamp plan without changing anything
}

// withDefaults returns a copy of the config with the non-required fields filled
// in. Called once before validation / execution so callers and the wizard see
// the same effective values.
func (c Config) withDefaults() Config {
	if strings.TrimSpace(c.Domain) == "" {
		c.Domain = defaultDomain
	}
	if strings.TrimSpace(c.CreateRepos) == "" {
		c.CreateRepos = "none"
	}
	if strings.TrimSpace(c.TemplateRepo) == "" {
		c.TemplateRepo = defaultTemplateRepo
	}
	return c
}

// Validate reports the first reason the config would be rejected, or nil. It
// mirrors scripts/bootstrap.sh's validate_params so the cockpit refuses the
// same inputs the script would, before any clone happens.
func (c Config) Validate() error {
	if err := ValidateProduct(c.Product); err != nil {
		return err
	}
	if err := ValidateOrg(c.ProductOrg); err != nil {
		return err
	}
	if err := ValidateDomain(c.Domain); err != nil {
		return err
	}
	switch c.CreateRepos {
	case "none", "github":
	default:
		return fmt.Errorf("invalid --create-repos %q (want none|github)", c.CreateRepos)
	}
	return nil
}

// ValidateProduct checks the product slug in isolation (reused by the wizard's
// per-field validation so a bad slug is caught before advancing a step).
func ValidateProduct(product string) error {
	if strings.TrimSpace(product) == "" {
		return fmt.Errorf("--product is required")
	}
	if !productRe.MatchString(product) {
		return fmt.Errorf("invalid --product %q (want ^[a-z][a-z0-9-]*$)", product)
	}
	if _, reserved := reservedProducts[product]; reserved {
		return fmt.Errorf("--product %q collides with a reserved checkout name", product)
	}
	return nil
}

// ValidateOrg checks the product org/user slug in isolation.
func ValidateOrg(org string) error {
	if strings.TrimSpace(org) == "" {
		return fmt.Errorf("--product-org is required")
	}
	if !orgRe.MatchString(org) {
		return fmt.Errorf("invalid --product-org %q", org)
	}
	return nil
}

// ValidateDomain checks the front-door domain in isolation. Empty is allowed
// here (the caller substitutes the default); a non-empty value must look like a
// hostname.
func ValidateDomain(domain string) error {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return nil
	}
	if !domainRe.MatchString(domain) {
		return fmt.Errorf("invalid --domain %q (want a hostname like memql.localhost)", domain)
	}
	return nil
}

// BootstrapArgs maps the config onto scripts/bootstrap.sh's flag surface. Only
// the flags the cockpit owns get forwarded: --dir / --template-ref /
// --template-repo govern the cockpit's own clone of the template and are NOT
// bootstrap flags; --engine-repo / --cockpit-repo keep bootstrap's defaults.
// bootstrap always runs non-interactively (it is a capability script).
func (c Config) BootstrapArgs() []string {
	args := []string{
		"--product=" + c.Product,
		"--product-org=" + c.ProductOrg,
		"--domain=" + c.Domain,
	}
	if strings.TrimSpace(c.EngineVersion) != "" {
		args = append(args, "--engine-version="+c.EngineVersion)
	}
	args = append(args, "--create-repos="+c.CreateRepos)
	if c.Shallow {
		args = append(args, "--shallow")
	}
	if c.DryRun {
		args = append(args, "--dry-run")
	}
	return args
}

// FrontDoorURLs composes the three front-door URLs a stamped workspace serves
// locally, mirroring the domain -> URL convention used across the cockpit
// (cli/app.go's autoSeedLocalFromGenesis) and the template README.
func FrontDoorURLs(domain string) []string {
	if strings.TrimSpace(domain) == "" {
		domain = defaultDomain
	}
	return []string{
		"https://identity." + domain,
		"https://bff." + domain,
		"https://app." + domain,
	}
}
