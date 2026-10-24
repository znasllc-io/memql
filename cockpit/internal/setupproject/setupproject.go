// Package setupproject implements `memql setup project` -- the
// interactive front-end for the memql-project template (memql#2448, epic
// #2438 WS-B). It stamps a new product workspace by cloning the template repo
// at a pinned ref, resetting it to fresh git history, and driving the
// template's scripts/bootstrap.sh capability script with the collected params.
//
// Two modes:
//
//  1. Non-interactive (flags): all required params on the command line. This is
//     what CI exercises.
//
//     memql setup project --product=acme --product-org=acme-io \
//     [--domain=...] [--engine-version=...] [--dir=<target>] \
//     [--create-repos=none|github] [--template-ref=<ref>] [--dry-run]
//
//  2. Interactive wizard: when the required flags are missing AND stdin is a
//     TTY, a tcell wizard (genesis pattern) collects product / org / domain /
//     engine version, shows a confirm screen with the composed front-door
//     URLs, and then runs the same flow. A non-TTY invocation missing the
//     required flags is a usage error (exit 2) -- it never blocks on a prompt.
//
// The cockpit CLONES the template (no go:embed payload): the template repo is
// the single source of truth, so template fixes ship without a cockpit release.
package setupproject

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/znasllc-io/memql/cockpit/internal/crash"
)

// stdinIsTTY reports whether stdin is an interactive terminal. A package var so
// tests can force the non-interactive branch deterministically.
var stdinIsTTY = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// HandleCommand is the entry point the cockpit binary calls for
// `memql setup ...`. Returns the process exit code.
func HandleCommand(args []string) int {
	exit := 0
	if rep := crash.Catch("setupproject:HandleCommand", func() {
		exit = dispatch(args)
	}); rep != nil {
		fmt.Fprint(os.Stderr, crash.UserMessage(rep))
		return exitFailure
	}
	return exit
}

func dispatch(args []string) int {
	if len(args) == 0 {
		printSetupUsage()
		return exitUsage
	}
	switch args[0] {
	case "project":
		return handleProject(args[1:])
	case "help", "-h", "--help":
		printSetupUsage()
		return exitOK
	default:
		fmt.Fprintf(os.Stderr, "ERROR: unknown setup target %q\n\n", args[0])
		printSetupUsage()
		return exitUsage
	}
}

// handleProject parses the flags, decides interactive vs non-interactive, and
// runs the flow.
func handleProject(args []string) int {
	cfg, help, err := parseProjectFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n\n", err)
		printProjectUsage()
		return exitUsage
	}
	if help {
		printProjectUsage()
		return exitOK
	}

	haveRequired := strings.TrimSpace(cfg.Product) != "" && strings.TrimSpace(cfg.ProductOrg) != ""

	if !haveRequired {
		// Missing required params: only a TTY may be prompted; anything else is
		// a usage error (never block on a prompt in CI / a pipe).
		if !stdinIsTTY() {
			fmt.Fprintln(os.Stderr, "ERROR: --product and --product-org are required in non-interactive mode")
			printProjectUsage()
			return exitUsage
		}
		wcfg, outcome, code := runWizard(cfg)
		if code != exitOK {
			return code
		}
		if outcome != OutcomeConfirmed {
			fmt.Fprintln(os.Stderr, "Canceled.")
			return exitFailure
		}
		cfg = wcfg
	}

	if err := cfg.withDefaults().Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		return exitUsage
	}
	flow := &Flow{Cfg: cfg}
	return flow.Run()
}

// parseProjectFlags parses `setup project` flags into a Config. It reports
// help=true for --help/-h and an error for an unknown flag. It does NOT
// validate values (the caller validates after the interactive step fills the
// required fields).
func parseProjectFlags(args []string) (cfg Config, help bool, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		var val string
		var hasVal bool
		if eq := strings.IndexByte(a, '='); strings.HasPrefix(a, "--") && eq >= 0 {
			val, hasVal = a[eq+1:], true
			a = a[:eq]
		}
		next := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 < len(args) {
				i++
				return args[i], nil
			}
			return "", fmt.Errorf("flag %q needs a value", a)
		}
		switch a {
		case "--product":
			cfg.Product, err = next()
		case "--product-org":
			cfg.ProductOrg, err = next()
		case "--domain":
			cfg.Domain, err = next()
		case "--engine-version":
			cfg.EngineVersion, err = next()
		case "--dir":
			cfg.Dir, err = next()
		case "--create-repos":
			cfg.CreateRepos, err = next()
		case "--template-ref":
			cfg.TemplateRef, err = next()
		case "--template-repo":
			cfg.TemplateRepo, err = next()
		case "--shallow":
			cfg.Shallow = true
		case "--dry-run":
			cfg.DryRun = true
		case "--help", "-h":
			help = true
		default:
			return cfg, false, fmt.Errorf("unknown flag %q", a)
		}
		if err != nil {
			return cfg, false, err
		}
	}
	return cfg, help, nil
}

func printSetupUsage() {
	fmt.Println("Usage: memql setup <target> [flags]")
	fmt.Println()
	fmt.Println("Targets:")
	fmt.Println("  project   Stamp a new memQL product workspace from the memql-project template")
	fmt.Println()
	fmt.Println("Run `memql setup project --help` for the project flags.")
}

func printProjectUsage() {
	fmt.Println("Usage: memql setup project [flags]")
	fmt.Println()
	fmt.Println("Stamp a new memQL product workspace: clone the memql-project template at a")
	fmt.Println("pinned ref, reset it to fresh git history, and run its scripts/bootstrap.sh to")
	fmt.Println("scaffold the product carrier + client repos and regenerate go.work.")
	fmt.Println()
	fmt.Println("With --product and --product-org this runs non-interactively. Without them, on")
	fmt.Println("a TTY, an interactive wizard collects the inputs; on a non-TTY it is a usage")
	fmt.Println("error.")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  --product <slug>        Product name (^[a-z][a-z0-9-]*$) (required non-interactively)")
	fmt.Println("  --product-org <org>     GitHub org/user owning the stamped repos (required non-interactively)")
	fmt.Println("  --domain <domain>       Local front-door domain (default: " + defaultDomain + ")")
	fmt.Println("  --engine-version <ref>  Engine ref to pin (default: latest release tag, else main)")
	fmt.Println("  --dir <target>          Target workspace directory (default: current dir; must be empty/absent)")
	fmt.Println("  --create-repos <mode>   Create + push GitHub repos for the stamped repos: none|github (default: none)")
	fmt.Println("  --template-ref <ref>    memql-project ref to clone: branch/tag/sha (default: default branch)")
	fmt.Println("  --template-repo <url>   memql-project clone URL (default: the canonical GitHub repo)")
	fmt.Println("  --shallow               Shallow-clone the template + engine/cockpit (CI smoke)")
	fmt.Println("  --dry-run               Report the stamp plan without changing anything")
	fmt.Println("  --help, -h              Show this help")
	fmt.Println()
	fmt.Println("Exit codes: 0 success · 1 failure · 2 usage/bad params · 4 missing prerequisite (git)")
	fmt.Println("            (bootstrap's own exit code -- 2/3/4/5 -- is propagated when it runs)")
}
