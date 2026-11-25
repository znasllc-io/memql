// Command memql-calls-tester-password makes a new tester password for mock
// calls commerce: the password an operator gives invited testers, and the hash
// line the bff reads from MEMQL_CALLS_TESTER_PASSWORD_HASH.
//
// It writes each to its own new file, mode 0600, and prints neither: a password
// on a terminal ends up in scrollback, logs and transcripts. Move the hash into
// the installation's Secret, keep the password where the operator looks it up
// (for example Key Vault), then delete both files.
//
//	go run ./cmd/memql-calls-tester-password --password-file=PATH --hash-file=PATH
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/znasllc-io/memql/integrations/calls"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("memql-calls-tester-password", flag.ContinueOnError)
	flags.SetOutput(stderr)
	passwordFile := flags.String("password-file", "", "new file for the tester password (created 0600; must not exist)")
	hashFile := flags.String("hash-file", "", "new file for the hash line (created 0600; must not exist)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *passwordFile == "" || *hashFile == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: memql-calls-tester-password --password-file=PATH --hash-file=PATH")
		return 2
	}
	if filepath.Clean(*passwordFile) == filepath.Clean(*hashFile) {
		fmt.Fprintln(stderr, "the password and the hash need two different files")
		return 2
	}
	// Refuse before writing anything, so an existing file is never replaced and
	// a half-made pair is never left behind.
	for _, p := range []string{*passwordFile, *hashFile} {
		if _, err := os.Lstat(p); err == nil || !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "refusing to use %s: it exists or cannot be checked\n", p)
			return 1
		}
	}
	password, hashLine, err := calls.GenerateTesterPassword()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err = writeNew(*passwordFile, password); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err = writeNew(*hashFile, hashLine); err != nil {
		_ = os.Remove(*passwordFile)
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote the tester password to %s\n", *passwordFile)
	fmt.Fprintf(stdout, "Wrote its hash line to %s (MEMQL_CALLS_TESTER_PASSWORD_HASH)\n", *hashFile)
	fmt.Fprintln(stdout, "Neither was printed. Install the hash, store the password, then delete both files.")
	return 0
}

func writeNew(path, value string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err = io.WriteString(f, value+"\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}
