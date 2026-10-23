package setupproject

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Outcome is what runWizard returns. The tcell wizard this file
// replaced reported the same three states, so handleProject's control
// flow did not change when the TUI left (memql#4552 / #4550).
type Outcome int

const (
	OutcomePending   Outcome = iota // still collecting (internal)
	OutcomeConfirmed                // user confirmed -- run the flow
	OutcomeCanceled                 // user backed out
)

// runWizard collects the missing Config fields with plain stdin
// prompts -- the headless successor to the retired tcell wizard. Same
// seed-in/Config-out contract: values already supplied by flags are
// presented as defaults, and Enter keeps them.
func runWizard(seed Config) (Config, Outcome, int) {
	in := bufio.NewReader(os.Stdin)
	cfg := seed

	fmt.Println("Stamp a new product workspace. Enter keeps the [default].")
	fmt.Println()
	cfg.Product = promptValue(in, "Product slug", cfg.Product, true)
	cfg.ProductOrg = promptValue(in, "GitHub org/user", cfg.ProductOrg, true)
	domainDefault := strings.TrimSpace(cfg.Domain)
	if domainDefault == "" {
		domainDefault = defaultDomain
	}
	cfg.Domain = promptValue(in, "Local front-door domain", domainDefault, false)
	cfg.EngineVersion = promptValue(in, "Engine version (blank = latest)", cfg.EngineVersion, false)

	engine := strings.TrimSpace(cfg.EngineVersion)
	if engine == "" {
		engine = "latest"
	}
	fmt.Println()
	fmt.Printf("  product: %s\n", cfg.Product)
	fmt.Printf("  org:     %s\n", cfg.ProductOrg)
	fmt.Printf("  domain:  %s\n", cfg.Domain)
	fmt.Printf("  engine:  %s\n", engine)
	fmt.Println()
	if !promptYes(in, "Proceed?") {
		return cfg, OutcomeCanceled, exitOK
	}
	return cfg, OutcomeConfirmed, exitOK
}

// promptValue reads one line for the named field. Required fields
// re-prompt until non-empty; optional fields accept blank (which
// keeps def, including a blank def). EOF mid-prompt keeps whatever
// was already collected -- downstream validation reports what is
// still missing.
func promptValue(in *bufio.Reader, label, def string, required bool) string {
	for {
		if def != "" {
			fmt.Printf("%s [%s]: ", label, def)
		} else {
			fmt.Printf("%s: ", label)
		}
		line, err := in.ReadString('\n')
		if err != nil && line == "" {
			return def
		}
		v := strings.TrimSpace(line)
		if v == "" {
			v = def
		}
		if v != "" || !required {
			return v
		}
		fmt.Println("  (required)")
	}
}

func promptYes(in *bufio.Reader, label string) bool {
	fmt.Printf("%s [y/N]: ", label)
	line, _ := in.ReadString('\n')
	v := strings.ToLower(strings.TrimSpace(line))
	return v == "y" || v == "yes"
}
