package setupproject

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Exit codes the flow returns. The cockpit pre-flight (clone / git / validate)
// owns 1/2/4; the bootstrap phase's own exit code (0/2/3/4/5 per the capability
// contract) is propagated verbatim so a caller sees the honest reason.
const (
	exitOK       = 0 // success
	exitFailure  = 1 // cockpit-driven step failed (clone, git init, bad template)
	exitUsage    = 2 // bad params / non-empty target / non-interactive without required flags
	exitPrereq   = 4 // a required tool (git) is missing
	bootstrapRel = "scripts/bootstrap.sh"
)

// Flow drives one `setup project` execution end to end: resolve the target,
// clone the template at the pinned ref, reset it to fresh git history, run
// scripts/bootstrap.sh with the mapped flags, parse the single JSON envelope,
// and render the summary. It is the shared core behind both front-ends
// (non-interactive flags and the wizard).
type Flow struct {
	Cfg Config
	// Out receives the human-facing summary (defaults to os.Stdout).
	Out io.Writer
	// Err receives cockpit-side diagnostics AND the streamed bootstrap stderr
	// (defaults to os.Stderr).
	Err io.Writer
}

func (f *Flow) out() io.Writer {
	if f.Out != nil {
		return f.Out
	}
	return os.Stdout
}

func (f *Flow) errw() io.Writer {
	if f.Err != nil {
		return f.Err
	}
	return os.Stderr
}

// Run executes the flow and returns the process exit code. It validates the
// config first (defensive: callers already validate), then clones + stamps.
func (f *Flow) Run() int {
	cfg := f.Cfg.withDefaults()
	f.Cfg = cfg
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(f.errw(), "ERROR: %v\n", err)
		return exitUsage
	}
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Fprintln(f.errw(), "ERROR: git is not installed (required to clone the template)")
		return exitPrereq
	}

	// A dry run makes no changes and leaves nothing behind, so it clones the
	// template into a throwaway temp dir and never touches the target. A real
	// run clones into the resolved workspace directory (which must be empty or
	// not yet exist) and resets it to a fresh git history.
	workspace, cleanup, code := f.resolveWorkspace(cfg)
	if code != exitOK {
		return code
	}
	if cleanup != nil {
		defer cleanup()
	}

	if code := f.cloneTemplate(cfg, workspace); code != exitOK {
		return code
	}
	if !cfg.DryRun {
		if code := f.freshGitInit(workspace); code != exitOK {
			return code
		}
	}
	return f.runBootstrap(cfg, workspace)
}

// resolveWorkspace returns the directory bootstrap will run in, an optional
// cleanup func, and an exit code (exitOK to proceed).
func (f *Flow) resolveWorkspace(cfg Config) (string, func(), int) {
	if cfg.DryRun {
		tmp, err := os.MkdirTemp("", "memql-setup-dryrun-*")
		if err != nil {
			fmt.Fprintf(f.errw(), "ERROR: creating temp workspace: %v\n", err)
			return "", nil, exitFailure
		}
		return tmp, func() { _ = os.RemoveAll(tmp) }, exitOK
	}

	target := strings.TrimSpace(cfg.Dir)
	if target == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(f.errw(), "ERROR: resolving working directory: %v\n", err)
			return "", nil, exitFailure
		}
		target = cwd
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		fmt.Fprintf(f.errw(), "ERROR: resolving target %q: %v\n", target, err)
		return "", nil, exitUsage
	}
	if err := requireEmptyOrAbsent(abs); err != nil {
		fmt.Fprintf(f.errw(), "ERROR: %v\n", err)
		return "", nil, exitUsage
	}
	return abs, nil, exitOK
}

// requireEmptyOrAbsent enforces that the target dir does not exist or is empty
// -- stamping into a populated tree would silently merge history.
func requireEmptyOrAbsent(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // git clone creates it
		}
		return fmt.Errorf("inspecting target %q: %w", dir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("target directory %q is not empty (pass --dir=<new-dir> or clear it)", dir)
	}
	return nil
}

// cloneTemplate clones the template repo at the configured ref into workspace.
// Tags and branches clone directly with --branch; a bare SHA needs a full clone
// then a checkout (mirrors bootstrap.sh's clone_repo).
func (f *Flow) cloneTemplate(cfg Config, workspace string) int {
	ref := strings.TrimSpace(cfg.TemplateRef)
	fmt.Fprintf(f.errw(), "INFO: cloning %s%s -> %s\n", cfg.TemplateRepo, refSuffix(ref), workspace)

	base := []string{"clone", "--quiet"}
	if cfg.Shallow {
		base = append(base, "--depth", "1")
	}
	if ref != "" {
		args := append(append([]string{}, base...), "--branch", ref, cfg.TemplateRepo, workspace)
		if err := f.git("", args...); err == nil {
			return exitOK
		}
		// --branch fails for a bare SHA: fall back to a full clone + checkout.
		if err := os.RemoveAll(workspace); err != nil {
			fmt.Fprintf(f.errw(), "ERROR: clearing partial clone: %v\n", err)
			return exitFailure
		}
		if err := f.git("", "clone", "--quiet", cfg.TemplateRepo, workspace); err != nil {
			fmt.Fprintf(f.errw(), "ERROR: cloning template: %v\n", err)
			return exitFailure
		}
		if err := f.git(workspace, "checkout", "--quiet", ref); err != nil {
			fmt.Fprintf(f.errw(), "ERROR: checking out template ref %q: %v\n", ref, err)
			return exitFailure
		}
		return exitOK
	}

	args := append(append([]string{}, base...), cfg.TemplateRepo, workspace)
	if err := f.git("", args...); err != nil {
		fmt.Fprintf(f.errw(), "ERROR: cloning template: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// freshGitInit strips the template's git history and re-inits the workspace on
// a fresh `main` -- stamping means new history, like GitHub's "Use this
// template". The workspace is left uncommitted for the operator to own.
func (f *Flow) freshGitInit(workspace string) int {
	if err := os.RemoveAll(filepath.Join(workspace, ".git")); err != nil {
		fmt.Fprintf(f.errw(), "ERROR: removing template .git: %v\n", err)
		return exitFailure
	}
	if err := f.git(workspace, "init", "--quiet", "-b", "main"); err != nil {
		fmt.Fprintf(f.errw(), "ERROR: git init: %v\n", err)
		return exitFailure
	}
	return exitOK
}

// runBootstrap execs scripts/bootstrap.sh in the workspace, streaming its human
// logs (stderr) live while capturing its stdout to parse the single JSON
// envelope. bootstrap's exit code is propagated honestly.
func (f *Flow) runBootstrap(cfg Config, workspace string) int {
	script := filepath.Join(workspace, bootstrapRel)
	if _, err := os.Stat(script); err != nil {
		fmt.Fprintf(f.errw(), "ERROR: template is missing %s (got %v)\n", bootstrapRel, err)
		return exitFailure
	}
	// Ensure the script is executable even if the clone landed on a filesystem
	// or git config (core.fileMode=false) that dropped the committed +x bit.
	if err := os.Chmod(script, 0o755); err != nil {
		fmt.Fprintf(f.errw(), "ERROR: making %s executable: %v\n", bootstrapRel, err)
		return exitFailure
	}

	var stdout bytes.Buffer
	cmd := exec.Command(script, cfg.BootstrapArgs()...)
	cmd.Dir = workspace
	cmd.Stdout = &stdout
	cmd.Stderr = f.errw()

	runErr := cmd.Run()
	exitCode := exitOK
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			exitCode = ee.ExitCode()
		} else {
			// Failed to start (e.g. bash missing, not executable).
			fmt.Fprintf(f.errw(), "ERROR: running %s: %v\n", bootstrapRel, runErr)
			return exitFailure
		}
	}

	env, parseErr := ParseEnvelope(stdout.Bytes())
	if parseErr != nil {
		fmt.Fprintf(f.errw(), "ERROR: %v\n", parseErr)
		if exitCode == exitOK {
			// bootstrap claimed success but produced no envelope -- treat as a
			// failure rather than reporting a bogus success.
			return exitFailure
		}
		return exitCode
	}

	f.renderSummary(cfg, env)
	return exitCode
}

// renderSummary prints the human-facing outcome from the parsed envelope.
func (f *Flow) renderSummary(cfg Config, env Envelope) {
	w := f.out()
	if err := env.Err(); err != nil {
		fmt.Fprintf(w, "ERROR: %v\n", err)
		return
	}

	res, _ := env.DecodeResult()
	domain := firstNonEmpty(res.Domain, cfg.Domain, defaultDomain)
	engineVersion := firstNonEmpty(res.EngineVersion, cfg.EngineVersion, "(resolved by bootstrap)")

	if res.DryRun || cfg.DryRun {
		fmt.Fprintln(w, "DRY RUN -- no changes were made. Planned stamp:")
	} else {
		root := firstNonEmpty(res.WorkspaceRoot, "(workspace)")
		fmt.Fprintf(w, "SUCCESS: workspace stamped at %s\n", root)
	}

	fmt.Fprintf(w, "  product:        %s\n", firstNonEmpty(res.Product, cfg.Product))
	fmt.Fprintf(w, "  org:            %s\n", firstNonEmpty(res.ProductOrg, cfg.ProductOrg))
	fmt.Fprintf(w, "  engine version: %s\n", engineVersion)
	fmt.Fprintf(w, "  front door:     %s\n", strings.Join(FrontDoorURLs(domain), ", "))
	if len(res.StampedRepos) > 0 {
		fmt.Fprintf(w, "  stamped repos:  %s\n", strings.Join(res.StampedRepos, ", "))
	} else {
		fmt.Fprintln(w, "  stamped repos:  (none yet -- carrier/client payloads land with memql#2446/#2447)")
	}

	if res.DryRun || cfg.DryRun {
		return
	}
	fmt.Fprintln(w, "Next steps:")
	fmt.Fprintf(w, "  cd %s-carrier && make up\n", firstNonEmpty(res.Product, cfg.Product))
}

// git runs a git subcommand, sending its output to the flow's stderr. dir is
// the working directory ("" for the current one).
func (f *Flow) git(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = f.errw()
	cmd.Stderr = f.errw()
	return cmd.Run()
}

func refSuffix(ref string) string {
	if ref == "" {
		return ""
	}
	return " (ref: " + ref + ")"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
