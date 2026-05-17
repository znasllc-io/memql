package genesis

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/znasllc-io/memql/component/secret"
)

// RcAction describes what happened to a shell rc file after a sync.
type RcAction int

const (
	RcNoop     RcAction = iota // file already carried the desired key
	RcUpdated                  // an existing assignment was replaced
	RcAppended                 // a new export line was appended
)

// rcKeyLine matches an uncommented MEMQL_MASTER_KEY assignment with
// or without an `export ` prefix, anchored at column 0. Commented
// (`# MEMQL_MASTER_KEY=...`) lines don't match because `#` precedes
// the anchor.
var rcKeyLine = regexp.MustCompile(`(?m)^(?:export\s+)?` + secret.EnvMasterKey + `=.*$`)

// RcCandidatePaths returns the shell rc files under $HOME that
// exist and should be kept in sync with the operator key. We touch
// ~/.bashrc and ~/.zshrc only -- bash_profile / profile aren't
// sourced for the typical interactive shell where the operator runs
// memql commands.
func RcCandidatePaths() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range []string{".bashrc", ".zshrc"} {
		p := filepath.Join(home, name)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

// ReconcileShellRC makes path's MEMQL_MASTER_KEY line agree with
// masterKeyHex. Replaces all existing assignments (covers the case
// where a stale entry was duplicated) and otherwise appends a fresh
// `export MEMQL_MASTER_KEY=...` line. The file's permission bits
// are preserved on rewrite.
func ReconcileShellRC(path, masterKeyHex string) (RcAction, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return RcNoop, fmt.Errorf("read %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return RcNoop, fmt.Errorf("stat %s: %w", path, err)
	}
	desired := "export " + secret.EnvMasterKey + "=" + masterKeyHex

	if rcKeyLine.Match(raw) {
		replaced := rcKeyLine.ReplaceAllLiteralString(string(raw), desired)
		if replaced == string(raw) {
			return RcNoop, nil
		}
		return RcUpdated, writeRCAtomic(path, []byte(replaced), info.Mode().Perm())
	}

	var sb strings.Builder
	sb.Write(raw)
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		sb.WriteByte('\n')
	}
	sb.WriteString(desired)
	sb.WriteByte('\n')
	return RcAppended, writeRCAtomic(path, []byte(sb.String()), info.Mode().Perm())
}

// writeRCAtomic rewrites path with content atomically, preserving
// the supplied mode bits. Same pattern as writeGenesisAtomic but
// kept separate because shell rc files use 0644-ish modes (not
// 0600) and we don't want to clobber that.
func writeRCAtomic(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmpName, path, err)
	}
	cleanup = false
	return nil
}
