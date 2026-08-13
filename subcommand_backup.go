package main

// `memql backup export` / `memql backup restore` -- the operator path to a
// portable copy of a cluster's data (memql#3604).
//
// WHY A SUBCOMMAND, like `pat` and `enrolment-token`. A backup is every row in
// the cluster, including rows no signed-in user is allowed to read. There is no
// caller it could be safely exposed to over a wire, so it is not exposed over
// one: the authorization is being able to exec the binary inside the pod, which
// already means holding the cluster's secrets.
//
// The stream goes to the REAL stdout when --out is "-", so a caller can pipe
// `kubectl exec ... memql backup export --out=-` straight into a file. Every
// human log goes to stderr, exactly as `pat mint` does with a token, so that
// capture holds the backup and only the backup.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/znasllc-io/memql/app"
	"github.com/znasllc-io/memql/component/backup"
	"github.com/znasllc-io/memql/core/common"
)

func runBackupSubcommand(args []string) int {
	if len(args) == 0 {
		printBackupUsage()
		return 2
	}
	switch args[0] {
	case "export":
		return runBackupExport(args[1:])
	case "restore":
		return runBackupRestore(args[1:])
	case "-h", "--help", "help":
		printBackupUsage()
		return 0
	}
	fmt.Fprintf(os.Stderr, "backup: unknown subcommand %q\n", args[0])
	printBackupUsage()
	return 2
}

func printBackupUsage() {
	fmt.Fprint(os.Stderr, `memql backup -- portable export and restore of a cluster's data

  memql backup export  --out=<file|->   [--no-secrets]
  memql backup restore --in=<file|->    [--allow-nonempty]

A backup is newline-delimited JSON: a manifest, then every graph row. It is
NOT a pg_dump -- it carries rows as the engine understands them, so a LATER
engine can still read it. A newer engine reads every older backup; the
reverse is refused rather than half-applied.

Secret rows travel still-encrypted, under the source cluster's master key.
The manifest fingerprints that key so a restore can say plainly whether they
will be readable here.
`)
}

func runBackupExport(args []string) int {
	fs := flag.NewFlagSet("backup export", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	out := fs.String("out", "", `Write the backup here. "-" streams it to stdout (required).`)
	noSecrets := fs.String("no-secrets", "", "Set to 1 to omit SecretMemoryNodes rows. See the warning this prints.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "backup export: --out is required (use --out=- for stdout)")
		return 2
	}

	realStdout := redirectStdoutToStderr()
	defer restoreStdout(realStdout)

	deps, application, logger, code := bootstrapBackupApp("backup export")
	if code != 0 {
		return code
	}
	defer stopBackupDependencies(deps)
	_ = logger

	db := application.BunDB()
	if db == nil {
		fmt.Fprintln(os.Stderr, "backup export: no database on this binary")
		return 1
	}

	includeSecrets := *noSecrets != "1"
	if !includeSecrets {
		// Said loudly, because the consequence surfaces long after the choice:
		// a restore from this file produces a cluster with no credentials, and
		// the first symptom is an account list that is empty for no visible
		// reason.
		fmt.Fprintln(os.Stderr,
			"backup export: WARNING: omitting secret rows. A restore from this backup "+
				"will have NO identities -- every user must enrol again.")
	}

	w := os.Stdout
	if *out != "-" {
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "backup export: open --out: %v\n", err)
			return 1
		}
		defer f.Close()
		w = f
	} else {
		w = realStdout
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	manifest, err := backup.Export(ctx, db, w, backup.Options{
		EngineVersion:  resolveVersionFn(),
		Domain:         os.Getenv("MEMQL_DOMAIN"),
		MasterKey:      os.Getenv("MEMQL_MASTER_KEY"),
		IncludeSecrets: includeSecrets,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup export: %v\n", err)
		return 5
	}

	total := 0
	for _, n := range manifest.Counts {
		total += n
	}
	fmt.Fprintf(os.Stderr, "backup export: wrote %d rows (format v%d, engine %s)",
		total, manifest.FormatVersion, manifest.EngineVersion)
	for table, n := range manifest.Counts {
		fmt.Fprintf(os.Stderr, " %s=%d", table, n)
	}
	fmt.Fprintln(os.Stderr)
	return 0
}

func runBackupRestore(args []string) int {
	fs := flag.NewFlagSet("backup restore", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	in := fs.String("in", "", `Read the backup from here. "-" reads stdin (required).`)
	allowNonEmpty := fs.String("allow-nonempty", "", "Set to 1 to restore into a cluster that already has rows.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *in == "" {
		fmt.Fprintln(os.Stderr, "backup restore: --in is required (use --in=- for stdin)")
		return 2
	}

	realStdout := redirectStdoutToStderr()
	defer restoreStdout(realStdout)

	deps, application, _, code := bootstrapBackupApp("backup restore")
	if code != 0 {
		return code
	}
	defer stopBackupDependencies(deps)

	db := application.BunDB()
	if db == nil {
		fmt.Fprintln(os.Stderr, "backup restore: no database on this binary")
		return 1
	}

	r := os.Stdin
	if *in != "-" {
		f, err := os.Open(*in)
		if err != nil {
			fmt.Fprintf(os.Stderr, "backup restore: open --in: %v\n", err)
			return 1
		}
		defer f.Close()
		r = f
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// REFUSE A NON-EMPTY TARGET BY DEFAULT.
	//
	// A restore INSERTS row versions; it does not merge or reconcile. Into a
	// cluster that already has data that means two histories interleaved by
	// timestamp, which is not something anybody asked for and not something
	// that can be undone. The empty-target check is cheap and the mistake is
	// not, so the default is to stop.
	if *allowNonEmpty != "1" {
		n, err := backup.CountRows(ctx, db)
		if err != nil {
			fmt.Fprintf(os.Stderr, "backup restore: check target is empty: %v\n", err)
			return 5
		}
		if n > 0 {
			fmt.Fprintf(os.Stderr,
				"backup restore: refusing -- this cluster already holds %d rows.\n"+
					"A restore inserts row versions rather than merging, so restoring over "+
					"existing data interleaves two histories and cannot be undone.\n"+
					"Restore into a fresh cluster, or pass --allow-nonempty=1 if that is "+
					"genuinely what you want.\n", n)
			return 3
		}
	}

	report, err := backup.Restore(ctx, db, r, os.Getenv("MEMQL_MASTER_KEY"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup restore: %v\n", err)
		return 5
	}

	total := 0
	for _, n := range report.Inserted {
		total += n
	}
	fmt.Fprintf(os.Stderr, "backup restore: inserted %d rows from a format-v%d backup written by engine %s\n",
		total, report.Manifest.FormatVersion, report.Manifest.EngineVersion)
	if report.SecretsUnreadable {
		// Not a failure -- the rows are restored either way, because dropping
		// somebody's data over a fingerprint mismatch would be worse. But it is
		// the difference between a cluster that can be signed into and one that
		// cannot, so it is said in full.
		fmt.Fprintln(os.Stderr,
			"backup restore: WARNING: the secret rows were encrypted under a DIFFERENT "+
				"master key than this cluster holds. They are restored, but nothing here "+
				"can decrypt them -- identities from the source cluster will not work. "+
				"Set MEMQL_MASTER_KEY to the source cluster's key and restore again into "+
				"a fresh cluster, or re-enrol.")
	}
	return 0
}

// bootstrapBackupApp starts the dependencies up to and including the engine --
// the same stop-after-engine shape `pat` uses, and for the same reason: a
// backup needs the database and nothing that comes after it, and starting the
// identity service would fatal-validate on a binary that is not identity.
func bootstrapBackupApp(prefix string) ([]common.Dependency, *app.App, any, int) {
	if err := applySubcommandEnv(prefix); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil, nil, nil, 1
	}
	logger := mustCreateCLILogger()
	application := app.Build(logger, resolveVersionFn(), app.Overrides{})

	selected, ok := depsUpToEngine(application.Dependencies)
	if !ok {
		fmt.Fprintf(os.Stderr, "%s: engine dependency not present in this build\n", prefix)
		return nil, nil, logger, 1
	}
	deps := make([]common.Dependency, 0, len(selected))
	for _, d := range selected {
		d.Start(context.Background())
		deps = append(deps, d)
	}
	return deps, application, logger, 0
}

func stopBackupDependencies(deps []common.Dependency) {
	for i := len(deps) - 1; i >= 0; i-- {
		deps[i].Stop(context.Background())
	}
}
