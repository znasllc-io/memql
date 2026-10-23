package worker

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/backup"
	"github.com/znasllc-io/memql/cockpit/internal/worker/tools"
)

// `memql worker backup` -- what this machine is being asked to back up, and
// whether it will (memql#4841).
//
// IT RUNS THE SAME CODE THE WORKER RUNS, which is the discipline models_cmd.go
// states and the reason this exists at all: a second implementation could
// disagree with the first, and the operator would have no way to know which
// one lied. `--once` drives Manager.SweepOnce -- literally the function the
// loop calls -- so what an operator sees here is what the background worker
// does.
//
// The listing half is the one thing an operator actually needs when a backup
// is not working, and it is a question no other surface answers: the Files app
// can say "this machine said no", and only this command can say WHY, because
// the reason is in a file on this machine.

// backupSweepTimeout bounds `--once`.
//
// GENEROUS ON PURPOSE. A first sweep of the folder this feature exists for --
// client video -- is gigabytes over a domestic uplink, and a minute-long
// deadline cut it off mid-push: files were uploaded, the context tripped, and
// the whole sweep's ledger was discarded, so the next run re-pushed every one
// of them and versioned each again. (The ledger is saved on the way out now
// either way, which makes an interrupted sweep merely incomplete rather than
// wasted -- but the deadline should still be one a real folder can finish
// inside.)
const backupSweepTimeout = 6 * time.Hour

// backupListTimeout bounds the LISTING, which is two reads and should never
// take long. Separate so a slow cluster fails the listing fast instead of
// hanging for hours on a question with a quick answer.
const backupListTimeout = 30 * time.Second

func handleBackup(args []string) {
	fs := flag.NewFlagSet("worker backup", flag.ExitOnError)
	configPath := fs.String("config", DefaultConfigPath(), "path to legacy worker.yaml")
	workersPath := fs.String("workers", DefaultWorkersPath(), "path to workers.yaml")
	homeID := fs.String("home", "", "home id when multiple homes are enrolled (see memql worker config)")
	once := fs.Bool("once", false, "run one sweep now instead of only listing")
	_ = fs.Parse(args)

	w, err := LoadWorkers(*workersPath, *configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	_, cfg, err := selectBackupHome(w, *homeID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	policyPath := filepath.Join(filepath.Dir(*workersPath), "policy.yaml")
	if _, err := os.Stat(policyPath); err != nil {
		policyPath = filepath.Join(filepath.Dir(*configPath), "policy.yaml")
	}
	policy, err := tools.LoadPolicy(policyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)
	baseURL := backupBaseURL(cfg.ClusterURL)
	bearer := backupBearer(cfg.ClusterURL, logger)

	printBackupHeader(os.Stdout, baseURL, policy.BackupRoots(), policyPath, bearer != nil)
	if bearer == nil {
		os.Exit(0)
	}

	graph := backup.NewGraph(baseURL, &http.Client{}, bearer)

	// EVERY watch the caller has, not this machine's. The registration id
	// belongs to the running worker's stream, which this process is not, so the
	// listing asks the wider question and names the machine on each row -- and
	// `Watches` OMITS the argument for a blank id rather than rendering
	// `workerId: ""`, which would have matched nothing and reported that a
	// person with backups had none.
	listCtx, cancelList := context.WithTimeout(context.Background(), backupListTimeout)
	defer cancelList()
	fmt.Fprintln(os.Stdout, "")
	watches, err := graph.Watches(listCtx, "")
	if err != nil {
		fmt.Fprintf(os.Stdout, "Could not read your watched folders: %v\n", err)
		os.Exit(1)
	}

	// The id the running worker recorded the last time it registered here.
	// Without it this command cannot sweep AS this machine, and a sweep that
	// named no machine would be refused at the upload anyway.
	workerID := backup.LoadRegistrationID(cfg.StateDir)
	printBackupWatches(os.Stdout, watches, policy.CheckBackupPath, workerID)

	if !*once {
		return
	}
	fmt.Fprintln(os.Stdout, "")
	if workerID == "" {
		// REFUSED, LOUDLY. Sweeping with no id used to read every row as
		// "nothing to do" and print "Done" -- a command that did nothing and
		// said it had succeeded.
		fmt.Fprintln(os.Stderr, "Cannot sweep: this machine has no recorded registration id.")
		fmt.Fprintln(os.Stderr, "Start the worker here once (`memql worker run`, or the installed")
		fmt.Fprintln(os.Stderr, "service) so it registers, then run this again.")
		os.Exit(4)
	}
	manager := backup.New(backup.Options{
		Logger:            logger,
		StateDir:          cfg.StateDir,
		BaseURL:           baseURL,
		Bearer:            bearer,
		CheckPath:         policy.CheckBackupPath,
		CheckDownloadPath: policy.CheckBackupDownloadPath,
		HTTPClient:        &http.Client{},
	})
	sweepCtx, cancelSweep := context.WithTimeout(context.Background(), backupSweepTimeout)
	defer cancelSweep()
	fmt.Fprintf(os.Stdout, "Sweeping now as %s. This is the same pass the worker runs.\n", workerID)
	manager.SweepOnce(sweepCtx, workerID)
	fmt.Fprintln(os.Stdout, "Done. The Files app shows what each machine reported.")
}

// selectBackupHome picks the home whose namespaced state_dir / cluster
// the backup command should act as. One enabled home is unambiguous;
// multiple require --home so we never sweep or read registration for
// the wrong sibling.
func selectBackupHome(w WorkersFile, homeID string) (Home, Config, error) {
	homeID = strings.TrimSpace(homeID)
	if homeID != "" {
		for _, h := range w.Homes {
			if h.ID == homeID {
				return h, w.ConfigForHome(h), nil
			}
		}
		return Home{}, Config{}, fmt.Errorf("home %q not found in workers.yaml", homeID)
	}
	homes := w.EnabledHomes()
	switch len(homes) {
	case 0:
		return Home{}, Config{}, errors.New("no enabled homes in workers.yaml (pair a cluster or pass --home)")
	case 1:
		return homes[0], w.ConfigForHome(homes[0]), nil
	default:
		ids := make([]string, 0, len(homes))
		for _, h := range homes {
			ids = append(ids, h.ID)
		}
		return Home{}, Config{}, fmt.Errorf("multiple enabled homes (%s); pass --home <id>", strings.Join(ids, ", "))
	}
}

// printBackupHeader states what this machine will and will not do, before any
// list. Split out so its shape is assertable with no cluster and no sign-in.
func printBackupHeader(w io.Writer, baseURL string, roots []string, policyPath string, signedIn bool) {
	fmt.Fprintln(w, "Watched-folder backup on this machine")
	fmt.Fprintln(w, "")
	fmt.Fprintf(w, "Cluster:   %s\n", orNone(baseURL))
	if signedIn {
		fmt.Fprintln(w, "Sign-in:   this machine has a usable session")
	} else {
		// The exact repair, named. This is the single most common reason a
		// backup does nothing, and "not signed in" without the command is a
		// diagnosis with no treatment.
		fmt.Fprintln(w, "Sign-in:   NONE. Run `memql login` here -- the Library needs a user")
		fmt.Fprintln(w, "           sign-in, and the worker token this machine runs on cannot")
		fmt.Fprintln(w, "           reach it.")
	}
	fmt.Fprintln(w, "")
	if len(roots) == 0 {
		fmt.Fprintln(w, "Allowed folders: NONE.")
		fmt.Fprintf(w, "  This machine backs up nothing until %s lists one:\n", policyPath)
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "    backup:")
		fmt.Fprintln(w, "      roots:")
		fmt.Fprintln(w, "        - ~/Clients")
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "  Default-deny is deliberate: a watched folder is arranged in the")
		fmt.Fprintln(w, "  cluster, so this machine decides which paths it will honour.")
		return
	}
	fmt.Fprintln(w, "Allowed folders (backup.roots):")
	for _, root := range roots {
		fmt.Fprintf(w, "  %s\n", root)
	}
}

// printBackupWatches lists the arrangements and says, per row, whether this
// machine will honour the path.
func printBackupWatches(w io.Writer, watches []backup.Watch, check func(string) error, workerID string) {
	if len(watches) == 0 {
		fmt.Fprintln(w, "No watched folders are set up for you.")
		fmt.Fprintln(w, "Set one up in MemQL OS: Files -> Backups -> Back up a folder.")
		return
	}
	fmt.Fprintln(w, "Your watched folders:")
	for _, watch := range watches {
		mine := workerID != "" && watch.WorkerID == workerID
		verdict := "will back up"
		switch {
		case workerID != "" && !mine:
			// The policy check is about THIS machine, so running it against
			// another machine's path would report a refusal that machine has
			// not made. Say whose it is instead.
			verdict = "another machine's"
		case !watch.Active():
			verdict = "paused"
		case check == nil:
			verdict = "REFUSED (no policy configured)"
		default:
			if err := check(watch.LocalPath); err != nil {
				verdict = "REFUSED: " + err.Error()
			}
		}
		fmt.Fprintf(w, "  %-44s %s\n", watch.LocalPath, verdict)
		fmt.Fprintf(w, "  %-44s machine %s%s\n", "", watch.WorkerID, thisMachineMark(mine))
	}
	fmt.Fprintln(w, "")
	if workerID == "" {
		fmt.Fprintln(w, "This machine has not registered yet, so none of these is marked as its")
		fmt.Fprintln(w, "own. Start the worker here once and run this again.")
		return
	}
	fmt.Fprintln(w, "A folder listed for ANOTHER of your machines shows here too --")
	fmt.Fprintln(w, "only the machine it names sweeps it.")
}

func thisMachineMark(mine bool) string {
	if mine {
		return "  (this machine)"
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "not configured"
	}
	return s
}
