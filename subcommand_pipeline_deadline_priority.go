package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// `memql pipeline-deadline-priority` -- one pass that moves waiting pipeline step
// Jobs forward as their runs age. What a pass does and why it is a CronJob rather
// than a loop in the runner: integrations/pipelinesteps/deadline_priority.go. The
// CronJob, its one grant and its classes: deploy/k8s/components/pipelines-capacity.
//
// AVAILABLE ON EVERY NODE BINARY, with no build tag and no database, secret or
// mesh, for the reason error-pages and dsl-fetch are: it needs nothing a node
// boots, and integrations/pipelinesteps is linked into every binary already, so
// this adds nothing to any of them. It runs on the workbench image only because a
// cluster with a queue always runs that image.

const (
	pipelineDeadlinePriorityDefaultNamespace = "memql-pipelines"
	pipelineDeadlinePriorityDefaultQueue     = "memql-background"
	// pipelineDeadlinePriorityDerivedWindow is a window flag left unset: the
	// window follows each Job's own run ceiling. Not zero, because zero has always
	// meant the aged tier is off.
	pipelineDeadlinePriorityDerivedWindow = -1
	// pipelineDeadlinePriorityBudget ends a pass before the CronJob's
	// activeDeadlineSeconds (120) kills it, so a wedged API server still gets the
	// pass's one line out and the Job fails for a reason rather than by timeout.
	pipelineDeadlinePriorityBudget = 100 * time.Second
)

// boostRunFlag collects a repeatable --boost-run.
type boostRunFlag []string

func (f *boostRunFlag) String() string { return strings.Join(*f, ",") }
func (f *boostRunFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func printPipelineDeadlinePriorityUsage(w io.Writer) {
	fmt.Fprintf(w, `usage: memql pipeline-deadline-priority [flags]

One pass over the pipeline step Jobs waiting in a queue. Relabels each suspended
Job by how close its run is to its deadline, so old runs finish first and a step
near its deadline goes to the front. Prints one JSON line. Reads no database and
no secret; touches only Jobs still suspended, and never moves one down.

Flags:
  --namespace=<name>          namespace the step Jobs run in (default %s)
  --queue=<name>              LocalQueue to reorder (default %s)
  --window-minutes=<n>        run deadline within n minutes takes %s
                              (15..1440; default a quarter of the Job's run ceiling)
  --aged-window-minutes=<n>   run deadline within n minutes takes %s
                              (0 turns this tier off; default half of the Job's run ceiling)
  --boost-run=<id>            a pipeline run whose waiting steps take %s;
                              repeatable (32 hex characters, as the run page shows it)
  --dry-run                   report what would move and write nothing
  --api=<url>                 a local "kubectl proxy"; in-cluster when omitted

Run one pass by hand against a cluster:
  kubectl proxy --port=8001 &
  go run . pipeline-deadline-priority --api=http://127.0.0.1:8001 --dry-run

Exit codes:
  0  the pass finished; every due Job moved or had changed since the list
  1  a patch was refused (it is in the JSON line, and the others still moved), or
     the Jobs could not be listed (on stderr, with no JSON line)
  2  bad usage
`, pipelineDeadlinePriorityDefaultNamespace, pipelineDeadlinePriorityDefaultQueue,
		pipelinesteps.ClassExpiring, pipelinesteps.ClassAged, pipelinesteps.ClassBoosted)
}

func runPipelineDeadlinePrioritySubcommand(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runPipelineDeadlinePriority(ctx, args, os.Stdout, os.Stderr, time.Now())
}

// runPipelineDeadlinePriority is one pass with its streams and clock as parameters.
func runPipelineDeadlinePriority(ctx context.Context, args []string, stdout, stderr io.Writer, now time.Time) int {
	fs := flag.NewFlagSet("pipeline-deadline-priority", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { printPipelineDeadlinePriorityUsage(stderr) }
	namespace := fs.String("namespace", pipelineDeadlinePriorityDefaultNamespace, "")
	queue := fs.String("queue", pipelineDeadlinePriorityDefaultQueue, "")
	window := fs.Int("window-minutes", pipelineDeadlinePriorityDerivedWindow, "")
	aged := fs.Int("aged-window-minutes", pipelineDeadlinePriorityDerivedWindow, "")
	dryRun := fs.Bool("dry-run", false, "")
	api := fs.String("api", "", "")
	var boosts boostRunFlag
	fs.Var(&boosts, "boost-run", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	usage := func(message string) int {
		fmt.Fprintf(stderr, "pipeline-deadline-priority: %s\n", message)
		return 2
	}
	if fs.NArg() != 0 {
		return usage(fmt.Sprintf("takes no arguments, got %q", fs.Args()))
	}
	if *namespace == "" {
		return usage("--namespace must name the namespace the step Jobs run in")
	}
	// Compared in minutes, before any Duration is made: a value this large would
	// overflow one and could wrap into range.
	if *window > pipelinesteps.MaxDeadlineWindowMinutes || *aged > pipelinesteps.MaxDeadlineWindowMinutes {
		return usage(fmt.Sprintf("a window is at most %d minutes, the longest a run may last", pipelinesteps.MaxDeadlineWindowMinutes))
	}
	settings := pipelinesteps.DeadlinePriority{Queue: *queue, BoostRuns: boosts, DryRun: *dryRun}
	switch {
	case *window == pipelineDeadlinePriorityDerivedWindow:
	case *window <= 0:
		return usage("--window-minutes must be a number of minutes")
	default:
		settings.ExpiringWindow = time.Duration(*window) * time.Minute
	}
	switch {
	case *aged == pipelineDeadlinePriorityDerivedWindow:
	case *aged == 0:
		settings.AgedOff = true
	case *aged < 0:
		return usage("--aged-window-minutes must be a number of minutes, or 0 to turn the tier off")
	default:
		settings.AgedWindow = time.Duration(*aged) * time.Minute
	}
	if err := settings.Validate(); err != nil {
		return usage(err.Error())
	}

	var cluster *deploycontrol.ClusterAPI
	if *api != "" {
		// An operator's `kubectl proxy`: their own credentials, no token here.
		cluster = deploycontrol.NewClusterAPIWith(strings.TrimRight(*api, "/"), "", nil)
	} else {
		var err error
		if cluster, err = deploycontrol.NewClusterAPI(); err != nil {
			fmt.Fprintf(stderr, "pipeline-deadline-priority: %s\n", err)
			return 1
		}
	}
	ctx, cancel := context.WithTimeout(ctx, pipelineDeadlinePriorityBudget)
	defer cancel()
	pass, err := settings.Pass(ctx, pipelinesteps.NewKube(cluster, *namespace), now)
	if err != nil {
		fmt.Fprintf(stderr, "pipeline-deadline-priority: listing the step Jobs: %s\n", err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(pass); err != nil {
		fmt.Fprintf(stderr, "pipeline-deadline-priority: writing the pass: %s\n", err)
		return 1
	}
	if len(pass.Failed) != 0 {
		return 1
	}
	return 0
}
