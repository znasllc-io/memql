package main

// `memql recovery-key claim` -- the operator path to the cluster's owner
// recovery key (memql#3969).
//
// # THE PLAINTEXT IS NEVER BROADCAST
//
// The key is MINTED by an invariant on the identity node (memql#3965), which
// discards the plaintext on the floor. That is deliberate: a break-glass
// credential printed at boot would land in the pod log, and from there in
// whatever ships those logs off the cluster. So the mint produces a row and
// nothing readable, and this subcommand is how a human obtains the value --
// once, on demand, from inside the pod.
//
// # WHY A SUBCOMMAND AND NOT A CALL
//
// The same authority `memql pat mint` and `memql enrolment-token mint` rely
// on: somebody who can exec inside the identity pod already holds the
// cluster's secrets, so access to the process IS the authorization. And here
// it is not merely convenient but necessary -- the operator claiming a
// recovery key is, by construction, the operator who may not be able to
// authenticate.
//
// UNTAGGED, like `pat` and `enrolment-token`: a recovery key is a random
// bearer rather than a signed JWT, so claiming needs the engine + database and
// nothing from the identity issuer.
//
// The key is written to the REAL stdout and never persisted or logged -- only
// its SHA-256 hash was ever in the row. Everything else, component startup
// logs included, goes to stderr so a caller's `$(kubectl exec ...)` capture
// holds the key and only the key.
//
// # CLAIMING IS NOT SPENDING
//
// It stamps claimedAt and reveals the value; the key stays usable. What the
// stamp changes is that the key stops being freely RE-MINTABLE: while nobody
// holds it, replacing it costs nothing, and once somebody does, silently
// replacing it would strand whatever they wrote down.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/identity/recoverykey"
)

func runRecoveryKeySubcommand(args []string) int {
	if len(args) == 0 {
		printRecoveryKeyUsage()
		return 2
	}
	switch args[0] {
	case "claim":
		return runRecoveryKeyClaim(args[1:])
	case "-h", "--help", "help":
		printRecoveryKeyUsage()
		return 0
	}
	fmt.Fprintf(os.Stderr, "recovery-key: unknown subcommand %q\n", args[0])
	printRecoveryKeyUsage()
	return 2
}

func printRecoveryKeyUsage() {
	fmt.Fprintln(os.Stderr, `usage: memql recovery-key <subcommand> [flags]

Subcommands:
  claim   Reveal the cluster's owner recovery key ONCE and stamp it claimed.

A recovery key authorizes exactly one action -- register a passkey as the
cluster owner -- and only while that owner has NO working way to sign in. It is
the break-glass credential: keep it somewhere the cluster is not.

It is minted automatically on the identity node and its plaintext is never
logged, so this command is the only way to obtain the value. Claiming reveals
it once and stamps claimedAt; it does not spend the key.

Requires the database environment (MEMQL_DATABASE_DSN). Typically run as
  kubectl exec -n memql deploy/identity -- /app/memql recovery-key claim

Run "memql recovery-key claim --help" for flags.`)
}

func runRecoveryKeyClaim(args []string) int {
	fs := flag.NewFlagSet("recovery-key claim", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	userId := fs.String("user-id", "",
		"Owner whose key to claim. Defaults to the cluster's owner when there is exactly one.")
	reclaim := fs.Bool("reclaim", false,
		"Re-reveal a key that has ALREADY been claimed. Refused without this flag: a second reveal "+
			"of a credential somebody already holds is a copy, and the audit trail should say it was asked for.")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Redirect stdout to stderr BEFORE bootstrap, for the reason `pat mint`
	// documents: the component loggers are created during app.Build and would
	// otherwise pollute a caller's command-substitution capture. The key is
	// written through the saved real-stdout fd.
	realStdout := redirectStdoutToStderr()
	defer restoreStdout(realStdout)

	deps, engine, logger, code := bootstrapPATEngine("recovery-key claim")
	if code != 0 {
		return code
	}
	defer stopPATDependencies(deps)

	// The CLI path is unauthenticated by design (operator exec), so stamp the
	// system CREDENTIAL actor: the engine's per-row authz gate and the
	// memql#2513 credential-actor guard both require one for a credential row.
	//
	// It must be ContextWithSystemCredentialActor specifically, and this line
	// said so while calling ContextWithSystemActor -- which does not satisfy
	// the guard it names. That actor stamps role="owner" and an email, and
	// ActorFromToken PREFERS email over subject, so the actor resolves to
	// "system@identity.memql.local": not role=="system", and not prefixed
	// "system:". Both arms of isSystemActor fail and every write here is
	// refused. The credential actor sets role="system" with no email, which is
	// exactly the shape the guard admits.
	ctx, cancel := context.WithTimeout(identity.ContextWithSystemCredentialActor(context.Background()), 30*time.Second)
	defer cancel()

	store := &identity.Store{Engine: engine, Logger: logger}
	owner := strings.TrimSpace(*userId)
	if owner == "" {
		owners, err := store.OwnerUserIds(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "recovery-key claim: resolve cluster owner: %v\n", err)
			return 1
		}
		switch len(owners) {
		case 0:
			fmt.Fprintln(os.Stderr, "recovery-key claim: this cluster has no owner yet, so there is no "+
				"recovery key to claim. A cluster is claimed by its first sign-in; the key is minted "+
				"once an owner exists.")
			return 1
		case 1:
			owner = owners[0]
		default:
			// Refused rather than guessed. With several owners the key the
			// operator wants is a question only they can answer, and picking
			// one would hand them a credential for an account they did not
			// name.
			fmt.Fprintf(os.Stderr, "recovery-key claim: this cluster has %d owners; pass --user-id to "+
				"say whose key to claim: %s\n", len(owners), strings.Join(owners, ", "))
			return 1
		}
	}

	recStore := &recoverykey.Store{Engine: engine, Logger: logger}
	live, err := recStore.ActiveForUser(ctx, owner)
	if err != nil {
		fmt.Fprintf(os.Stderr, "recovery-key claim: read active keys: %v\n", err)
		return 1
	}
	if len(live) == 0 {
		fmt.Fprintf(os.Stderr, "recovery-key claim: no active recovery key for %s. One is minted "+
			"automatically when the identity node starts; restart it, or check its logs for a mint "+
			"failure.\n", owner)
		return 1
	}
	row := live[0]

	if row.IsClaimed() && !*reclaim {
		// THE PLAINTEXT IS UNRECOVERABLE, and saying so plainly is the useful
		// part: only the hash was ever stored, so even --reclaim cannot show
		// the ORIGINAL value. What --reclaim does is mint a replacement and
		// reveal that -- which is a rotation, and is spelled as one.
		fmt.Fprintf(os.Stderr, "recovery-key claim: the key for %s was already claimed at %s.\n"+
			"Only its SHA-256 hash was ever stored, so the original value cannot be shown again.\n"+
			"Pass --reclaim to RETIRE that key and mint a replacement, which is revealed here once.\n",
			owner, row.ClaimedAt.UTC().Format(time.RFC3339))
		return 1
	}

	// claimId is stamped claimed only AFTER the plaintext has been successfully
	// revealed -- see the reveal-before-commit block below (#4628).
	plain := ""
	claimId := ""
	if row.IsClaimed() {
		// --reclaim: rotate. The predecessor is deactivated and a fresh key
		// takes its place, with rotatedFrom recording the lineage. This is the
		// same operation memql#3970 exposes to a signed-in owner; here it is
		// available to somebody who cannot sign in, which is the whole point of
		// the CLI path existing.
		newPlain, hash, mintErr := recoverykey.Mint()
		if mintErr != nil {
			fmt.Fprintf(os.Stderr, "recovery-key claim: generate key: %v\n", mintErr)
			return 1
		}
		newId, idErr := recoverykey.NewId()
		if idErr != nil {
			fmt.Fprintf(os.Stderr, "recovery-key claim: generate id: %v\n", idErr)
			return 1
		}
		if err := recStore.Create(ctx, newId, owner, hash, "operator:cli",
			recoverykey.CanonicalId(row.ID), recoverykey.DefaultLabel); err != nil {
			fmt.Fprintf(os.Stderr, "recovery-key claim: persist replacement: %v\n", err)
			return 1
		}
		// Deactivate the predecessor AFTER the replacement exists. The other
		// order would leave a window with no live key at all, which is the one
		// state this credential must never be in.
		if err := recStore.Deactivate(ctx, row.ID); err != nil {
			fmt.Fprintf(os.Stderr, "recovery-key claim: retire previous key: %v\n", err)
			return 1
		}
		plain = newPlain
		claimId = newId
		fmt.Fprintf(os.Stderr, "recovery-key claim: rotated -- retired %s, minted %s for %s\n",
			recoverykey.CanonicalId(row.ID), recoverykey.CanonicalId(newId), owner)
	} else {
		// THE ORDINARY PATH, AND IT CANNOT WORK. The plaintext of an
		// already-minted key is not recoverable from its hash, so "claiming"
		// an unclaimed key has to mean rotating it too -- retire the row whose
		// value nobody has, mint one whose value we are about to print.
		//
		// Stated rather than hidden, because "claim" reads like it reveals
		// something that already existed, and it does not. What makes this
		// safe is precisely that the predecessor was never claimed: nobody
		// holds it, so nothing is stranded.
		newPlain, hash, mintErr := recoverykey.Mint()
		if mintErr != nil {
			fmt.Fprintf(os.Stderr, "recovery-key claim: generate key: %v\n", mintErr)
			return 1
		}
		newId, idErr := recoverykey.NewId()
		if idErr != nil {
			fmt.Fprintf(os.Stderr, "recovery-key claim: generate id: %v\n", idErr)
			return 1
		}
		if err := recStore.Create(ctx, newId, owner, hash, "operator:cli",
			recoverykey.CanonicalId(row.ID), recoverykey.DefaultLabel); err != nil {
			fmt.Fprintf(os.Stderr, "recovery-key claim: persist key: %v\n", err)
			return 1
		}
		if err := recStore.Deactivate(ctx, row.ID); err != nil {
			fmt.Fprintf(os.Stderr, "recovery-key claim: retire the unclaimed placeholder: %v\n", err)
			return 1
		}
		plain = newPlain
		claimId = newId
		fmt.Fprintf(os.Stderr, "recovery-key claim: minted %s for %s\n",
			recoverykey.CanonicalId(newId), owner)
	}

	// REVEAL BEFORE COMMIT (memql#4628). The rotation used to be stamped
	// claimed BEFORE the plaintext left the process, so between the stamp and
	// the write the credential was irreversibly spent while its only copy was
	// a local variable. Anything that lost that write -- a broken pipe, a
	// closed capture, a transport hiccup mid-stream -- left the cluster
	// asserting an operator held a break-glass key that had been shown to
	// nobody, and every later run then said "the key claimed earlier is still
	// the live one".
	//
	// Writing first inverts which failure is possible. What this order can
	// leave behind is a key that was REVEALED but not stamped -- and that is a
	// state the invariant already tolerates, because an unclaimed live key is
	// exactly what recoverykey.EnsureForAllOwners leaves on every boot. The
	// operator has a working credential either way.
	//
	// The ONE line on real stdout. Nothing else is written here, so
	// `KEY=$(kubectl exec ... -- /app/memql recovery-key claim)` holds the key
	// and only the key.
	if _, err := fmt.Fprintln(realStdout, plain); err != nil {
		// NOT stamped, so nothing was spent. Say that plainly: the remedy is
		// to run the same command again, not --reclaim.
		fmt.Fprintf(os.Stderr, "recovery-key claim: the key could not be written to stdout: %v\n", err)
		fmt.Fprintln(os.Stderr, "recovery-key claim: it was NOT stamped claimed, so it is still the "+
			"live unclaimed key. Run this again to reveal it; --reclaim is not needed.")
		return 1
	}
	// Confirm the write actually landed before spending the key. A pipe or a
	// tty answers Sync with EINVAL / ENOTTY / ENODEV -- there is nothing to
	// flush and that is not a failure -- so only a real error counts. This is
	// the file-redirect case; for the `kubectl exec` pipe the write error above
	// is the signal.
	if err := realStdout.Sync(); err != nil &&
		!errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTTY) && !errors.Is(err, syscall.ENODEV) {
		fmt.Fprintf(os.Stderr, "recovery-key claim: the key could not be flushed to stdout: %v\n", err)
		fmt.Fprintln(os.Stderr, "recovery-key claim: it was NOT stamped claimed, so it is still the "+
			"live unclaimed key. Run this again to reveal it; --reclaim is not needed.")
		return 1
	}

	if err := recStore.Claim(ctx, claimId, "", time.Now().UTC()); err != nil {
		// The key above IS the live key and the operator now holds it; only
		// the claimed stamp is missing. Exiting non-zero here would discard a
		// credential that works, so this reports and succeeds -- and names the
		// one consequence, which is that a later run treats the key as
		// unclaimed and rotates it.
		fmt.Fprintf(os.Stderr, "recovery-key claim: the key above is live and yours, but stamping it "+
			"claimed failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "recovery-key claim: store it now. Because the stamp is missing, a "+
			"later run of this command will mint a REPLACEMENT and this one will stop working.")
		return 0
	}

	fmt.Fprintf(os.Stderr, "recovery-key claim: claimed %s for %s\n",
		recoverykey.CanonicalId(claimId), owner)
	fmt.Fprintln(os.Stderr, "recovery-key claim: store this somewhere the cluster is not. It is shown "+
		"once, it is refused while the owner can still sign in normally, and it works exactly once.")
	return 0
}
