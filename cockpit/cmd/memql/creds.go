// memql creds -- inspect + migrate the credential store.
//
// Subcommands:
//
//	creds status              show which backend won resolution + count
//	creds migrate-to-keyring  move every ~/.memql/credentials/*.json into
//	                          the OS keyring; deletes the source files on
//	                          successful import. Idempotent.
//
// Lives in its own file so the main.go switch stays readable.

package main

import (
	"fmt"
	"log/slog"
	"os"
	"sort"

	"github.com/znasllc-io/memql/cockpit/internal/config"
)

func handleCredsCmd(args []string) {
	if len(args) == 0 {
		printCredsUsage()
		os.Exit(1)
	}
	switch args[0] {
	case "status":
		handleCredsStatus()
	case "migrate-to-keyring":
		handleCredsMigrateToKeyring()
	case "help", "-h", "--help":
		printCredsUsage()
	default:
		fmt.Fprintf(os.Stderr, "ERROR: unknown creds subcommand %q\n\n", args[0])
		printCredsUsage()
		os.Exit(1)
	}
}

func printCredsUsage() {
	fmt.Println("Usage: memql creds <subcommand>")
	fmt.Println()
	fmt.Println("Subcommands:")
	fmt.Println("  status              Show the active credential backend + cached cluster names.")
	fmt.Println("  migrate-to-keyring  Move ~/.memql/credentials/*.json into the OS keyring.")
	fmt.Println()
	fmt.Println("Environment:")
	fmt.Println("  MEMQL_COCKPIT_CRED_STORE   Force backend: 'keyring' or 'file'. Default: auto-probe.")
}

func handleCredsStatus() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	store, err := config.Resolve(config.ResolveOptions{Logger: logger})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Active backend: %s\n", store.Name())

	// Enumerate what's in the active store (best-effort -- both
	// stores expose a list method but the interface stays minimal).
	var stored []string
	switch s := store.(type) {
	case *config.FileStore:
		stored, _ = s.List()
	case *config.KeyringStore:
		stored, _ = s.Keys()
	}
	sort.Strings(stored)

	if len(stored) == 0 {
		fmt.Println("Cached clusters: (none)")
		return
	}
	fmt.Printf("Cached clusters: %d\n", len(stored))
	for _, name := range stored {
		fmt.Printf("  - %s\n", name)
	}
}

func handleCredsMigrateToKeyring() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	fileStore := config.NewFileStore()
	names, err := fileStore.List()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: enumerate ~/.memql/credentials: %v\n", err)
		os.Exit(1)
	}
	if len(names) == 0 {
		fmt.Println("Nothing to migrate: ~/.memql/credentials is empty.")
		return
	}

	kr, err := config.NewKeyringStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: open OS keyring: %v\n", err)
		fmt.Fprintln(os.Stderr, "Hint: on Linux, ensure gnome-keyring / KWallet / a Secret Service")
		fmt.Fprintln(os.Stderr, "      provider is running. On macOS this should always work.")
		os.Exit(1)
	}

	logger.Info("starting credential migration",
		"source", fileStore.Name(),
		"destination", kr.Name(),
		"count", len(names),
	)

	var (
		migrated []string
		skipped  []string
		failed   []string
	)
	for _, name := range names {
		tok, err := fileStore.Get(name)
		if err != nil {
			logger.Warn("skipping cluster (read error)", "cluster", name, "error", err)
			failed = append(failed, name)
			continue
		}
		if tok == nil {
			skipped = append(skipped, name)
			continue
		}
		// Idempotency: if the keyring already has an entry under
		// this cluster name, skip rather than overwrite. The
		// operator can run `creds migrate-to-keyring --force` if
		// we ever add that knob; today we err on the side of
		// preserving the keyring copy.
		if existing, err := kr.Get(name); err == nil && existing != nil {
			logger.Info("skipping cluster (already in keyring)", "cluster", name)
			skipped = append(skipped, name)
			continue
		}
		if err := kr.Put(name, tok); err != nil {
			logger.Warn("skipping cluster (keyring put error)", "cluster", name, "error", err)
			failed = append(failed, name)
			continue
		}
		// Source file goes away only after the destination
		// confirms the write.
		if err := fileStore.Delete(name); err != nil {
			logger.Warn("imported into keyring but failed to delete source file",
				"cluster", name, "error", err)
			// Counted as migrated anyway -- the keyring is the
			// authoritative copy now.
		}
		migrated = append(migrated, name)
	}

	fmt.Printf("Migrated %d, skipped %d, failed %d.\n",
		len(migrated), len(skipped), len(failed))
	if len(migrated) > 0 {
		fmt.Println("Migrated:")
		for _, n := range migrated {
			fmt.Printf("  - %s\n", n)
		}
	}
	if len(skipped) > 0 {
		fmt.Println("Skipped (already in keyring or empty file):")
		for _, n := range skipped {
			fmt.Printf("  - %s\n", n)
		}
	}
	if len(failed) > 0 {
		fmt.Println("Failed:")
		for _, n := range failed {
			fmt.Printf("  - %s\n", n)
		}
		os.Exit(2)
	}
}
