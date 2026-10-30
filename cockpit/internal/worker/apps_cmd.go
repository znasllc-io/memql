package worker

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql/cockpit/internal/worker/appsession"
	"github.com/znasllc-io/memql/cockpit/internal/worker/harness"
	"github.com/znasllc-io/memql/cockpit/internal/worker/inference"
	"github.com/znasllc-io/memql/cockpit/internal/worker/tools"
)

// `memql worker apps` (memql-cockpit#438).
//
// It answers the question an owner has the moment they write apps.levels --
// *what will this machine actually run, at each level, for each app?* -- and
// the one the portal cannot answer for them: why an app on this machine is
// not being picked. "Not installed", "installed but not allowed for this
// cluster" and "allowed but not signed in" all render identically from the
// cluster, as an absence; only the machine can tell them apart.
//
// It runs the SAME detector and reads the SAME policy the worker does, so
// what it prints is what a session would get. A second implementation of
// "which knobs does strong mean here" would be a second answer, and the
// owner would have no way to know which one lied.
//
// With --allow / --deny it also CHANGES the consent, per cluster
// (apps_consent.go), and the report that follows is the state the edit
// left behind.

const appsProbeTimeout = 15 * time.Second

// appsCheckTimeout is the watchdog on one --check. A ping took under 3 seconds when
// measured, and a signed-out Codex's limits read answers at once.
const appsCheckTimeout = 90 * time.Second

func handleApps(args []string) {
	fs := flag.NewFlagSet("worker apps", flag.ExitOnError)
	configPath := fs.String("config", DefaultConfigPath(), "path to legacy worker.yaml")
	workersPath := fs.String("workers", DefaultWorkersPath(), "path to workers.yaml")
	var allow, deny repeatedFlag
	fs.Var(&allow, "allow", "allow this app for one cluster (policy.yaml apps.homes.<cluster>.allow); repeatable")
	fs.Var(&deny, "deny", "withdraw this app from one cluster; repeatable")
	home := fs.String("home", "", "the cluster --allow and --deny apply to: its home id or cluster URL (default: the only one enrolled)")
	check := fs.Bool("check", false, "measure now whether each app can take a call, signed in and with usage left; a Claude Code check spends about 600 tokens")
	_ = fs.Parse(args)

	// The policy.yaml the worker itself reads: beside workers.yaml for a
	// fleet, beside worker.yaml otherwise -- the order `memql worker
	// backup` already follows. A report read from the other file would be
	// a confident answer about a policy the worker never loads.
	policyPath := filepath.Join(filepath.Dir(*workersPath), "policy.yaml")
	if _, err := os.Stat(policyPath); err != nil {
		policyPath = filepath.Join(filepath.Dir(*configPath), "policy.yaml")
	}
	// The clusters this machine is enrolled with, which is what consent is
	// given to. workers.yaml, or the legacy worker.yaml it migrates from.
	workers, err := LoadWorkers(*workersPath, *configPath)
	if err != nil {
		if len(allow) > 0 || len(deny) > 0 {
			// Consent names a cluster, and there is no reading which
			// clusters there are.
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(1)
		}
		// The report still has everything else to say.
		fmt.Fprintf(os.Stderr, "WARNING: the clusters this machine is enrolled with could not be read, so no cluster's consent is shown: %v\n", err)
	}

	if len(allow) > 0 || len(deny) > 0 {
		err := runAppConsent(context.Background(), os.Stdout, appConsentRequest{
			policyPath: policyPath, homes: workers.Homes, home: *home, allow: allow, deny: deny,
		}, inference.Readvertise)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
			os.Exit(SetupExitCode(err))
		}
	}

	policy, err := tools.LoadPolicy(policyPath)
	if err != nil {
		// Unlike the worker, which runs on defaults when the file does not
		// parse, this command exists to say what the file means -- and a
		// table printed from defaults would be a confident answer about a
		// file nobody read.
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	// Search where the worker searches: this shell's PATH, plus the app
	// directories the worker adds to its own (servicepath.go).
	ensureServicePath(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), appsProbeTimeout)
	defer cancel()
	inventory := (&apps.Detector{}).Detect(ctx, nil)
	homeIDs := make([]string, 0, len(workers.Homes))
	for _, h := range workers.Homes {
		homeIDs = append(homeIDs, h.ID)
	}
	printAppInventory(os.Stdout, inventory, homeIDs, policy, policyPath)
	if *check {
		printAppChecks(context.Background(), os.Stdout, &apps.Detector{}, appsession.CheckLauncher())
	}
}

// printAppChecks runs each installed app's real check and prints what it found, for
// a person at a terminal asking whether this machine's apps work. The worker does
// the same on its own schedule, and this reads nothing it wrote.
func printAppChecks(ctx context.Context, w io.Writer, detector *apps.Detector, launch harness.LaunchFunc) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Checked now")
	for _, spec := range apps.Specs() {
		resolved, ok := detector.ResolveSpec(ctx, spec.ID)
		if !ok {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, appsCheckTimeout)
		signal := apps.MeasureNow(checkCtx, spec.ID, resolved, launch)
		cancel()
		fmt.Fprintf(w, "%-12s %s\n", spec.ID, appCheckLine(signal))
	}
}

// appCheckLine is one check's answer in a sentence.
func appCheckLine(signal harness.ReadinessSignal) string {
	switch signal.State {
	case harness.ReadinessReady:
		return "ready"
	case harness.ReadinessSignedOut:
		return "signed out: " + signal.Detail
	case harness.ReadinessOutOfUsage:
		if signal.ResetsAt.IsZero() {
			return "out of usage"
		}
		return "out of usage until " + signal.ResetsAt.Local().Format("Jan 2 3:04 PM")
	}
	return "could not tell: " + signal.Detail
}

// printAppInventory writes the report. Split out so the shape is assertable
// without a claude or a codex on the machine running the tests.
//
// inventory's own allowed flags are not read: consent is per cluster, so
// each app is shown against every cluster in homes.
func printAppInventory(w io.Writer, inventory []apps.Info, homes []string, policy *tools.Policy, policyPath string) {
	fmt.Fprintln(w, "Apps on this machine")
	fmt.Fprintln(w, "(found on this shell's PATH plus the app directories the worker adds to its own)")
	fmt.Fprintln(w)
	// Consent is given to these, by these names (--home).
	if len(homes) == 0 {
		fmt.Fprintln(w, "Clusters: none enrolled -- pair this machine with a cluster first (memql worker pair)")
	} else {
		fmt.Fprintln(w, "Clusters: "+strings.Join(homes, ", "))
	}
	fmt.Fprintln(w)

	found := make(map[string]apps.Info, len(inventory))
	for _, a := range inventory {
		found[a.Id] = a
	}
	for _, spec := range apps.Specs() {
		a, ok := found[spec.ID]
		if !ok {
			fmt.Fprintf(w, "%-12s not installed (no %s on PATH)\n\n", spec.ID, spec.Binary)
			continue
		}
		printAppRows(w, a, homes, policy)
	}

	printProblems(w, "app consent", policyPath, policy.AppConsentProblems())
	printProblems(w, "apps.levels", policyPath, policy.AppLevelProblems())

	fmt.Fprintln(w, "Consent is per cluster: `memql worker apps --allow <app> --home <cluster>` allows")
	fmt.Fprintln(w, "one, `--deny` withdraws it, and the running worker is signalled. Both edit")
	fmt.Fprintln(w, "  "+policyPath)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "A level is how much intelligence a step needs. This machine turns it into each")
	fmt.Fprintln(w, "app's own model and effort; the model and effort a session actually ran on come")
	fmt.Fprintln(w, "back from the app itself. To change a row, edit apps.levels in that file and send")
	fmt.Fprintln(w, "the worker a SIGHUP.")
}

// printProblems lists one block's problems, when it has any.
func printProblems(w io.Writer, block, policyPath string, problems []string) {
	if len(problems) == 0 {
		return
	}
	noun := "problems"
	if len(problems) == 1 {
		noun = "problem"
	}
	fmt.Fprintf(w, "%s in %s has %d %s:\n", block, policyPath, len(problems), noun)
	for _, p := range problems {
		fmt.Fprintf(w, "  - %s\n", p)
	}
	fmt.Fprintln(w)
}

// printAppRows writes one app: its sign-in, its consent for each cluster,
// its harness, and every level.
func printAppRows(w io.Writer, a apps.Info, homes []string, policy *tools.Policy) {
	version := strings.TrimSpace(a.Version)
	if version == "" {
		version = "version unknown"
	}
	fmt.Fprintf(w, "%-12s %s\n", a.Id, version)
	fmt.Fprintf(w, "  %-10s %s\n", "signed in", signedInLine(a))
	if len(homes) == 0 {
		fmt.Fprintf(w, "  %-10s %s\n", "clusters", "none enrolled -- pair this machine with a cluster first (memql worker pair)")
	}
	width := 0
	for _, home := range homes {
		width = max(width, len(home))
	}
	for i, home := range homes {
		label := ""
		if i == 0 {
			label = "clusters"
		}
		allowed := slices.Contains(policy.AppsAllowFor(home), a.Id)
		fmt.Fprintf(w, "  %-10s %-*s  %s\n", label, width, home, clusterVerdict(a, allowed, home))
	}
	fmt.Fprintf(w, "  %-10s %s\n", "harness", harnessLine(a))

	// The table a session would resolve through: the built-in one for the
	// harness this machine drives the app with, under the owner's entries.
	override, refused := policy.AppLevels(a.Id)
	table := harness.MergeLevels(harness.BuiltinLevels(a.Harness), override)

	levels := harness.AppLevels()
	cells := make([]string, len(levels))
	width = 0
	for i, level := range levels {
		if _, bad := refused[level]; bad {
			cells[i] = "REFUSED (see the problem below)"
		} else {
			cells[i] = harness.DescribeKnobs(a.Harness, table[level])
		}
		width = max(width, len(cells[i]))
	}
	for i, level := range levels {
		label := ""
		if i == 0 {
			label = "levels"
		}
		source := ""
		if _, bad := refused[level]; !bad {
			source = "built-in"
			if _, mine := override[level]; mine {
				source = "policy.yaml"
			}
		}
		row := fmt.Sprintf("  %-10s %-11s %-*s  %s", label, level, width, cells[i], source)
		fmt.Fprintln(w, strings.TrimRight(row, " "))
	}
	fmt.Fprintf(w, "  %-10s %-11s %s\n", "", "embeddings", "never through an app")
	fmt.Fprintln(w)
}

// signedInLine is the app's own half of the routing label. A cluster picks
// this machine only when the app is signed in AND allowed for that cluster.
func signedInLine(a apps.Info) string {
	if a.SignedIn {
		return "yes"
	}
	return "NO -- sign in to the app itself; until then no cluster will pick this machine"
}

// clusterVerdict is this machine's consent for one cluster, and when it is
// missing, the command that gives it.
func clusterVerdict(a apps.Info, allowed bool, home string) string {
	switch {
	case allowed && a.SignedIn:
		return "allowed -- this cluster can send it sessions"
	case allowed:
		return "allowed, once the app is signed in"
	}
	return "BLOCKED -- " + appsCommand([]string{a.Id}, nil, home)
}

// harnessLine names the protocol this machine drives the app through and
// what that protocol can do. A harness that cannot constrain an answer says
// so, because it is the reason the engine keeps structured calls away from
// this machine.
func harnessLine(a apps.Info) string {
	var can []string
	if a.StructuredResult {
		can = append(can, "structured answers")
	} else {
		can = append(can, "no structured answers")
	}
	if a.FollowUps {
		can = append(can, "follow-up turns")
	} else {
		can = append(can, "no follow-up turns")
	}
	word := a.Harness
	if strings.TrimSpace(word) == "" {
		word = "no harness"
	}
	return word + ": " + strings.Join(can, ", ")
}
