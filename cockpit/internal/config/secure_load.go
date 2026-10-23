package config

import (
	"fmt"
	"log/slog"
	"os"
)

// VerifyCredentialFileMode rejects credential files whose mode allows
// access beyond the owner. The write path for every cockpit
// credential file uses 0600 (correct); this check at the LOAD path
// catches the case where the file mode drifted out-of-band: a
// hand-edited file with a `chmod 644`, an `rsync` from another
// machine that preserved permissive modes, or a misconfigured umask
// when a third-party tool re-wrote the file.
//
// The check is conservative: any group- or other-readable mode bit
// (0077) is treated as a violation. Symlinks are followed (os.Stat
// rather than os.Lstat) so the target's mode is what matters.
//
// Returns nil on a clean (owner-only) file. Returns a typed error
// when the file mode permits broader access; loader callers should
// surface it to the user with the path and the offending mode so
// they can `chmod 0600` and retry.
func VerifyCredentialFileMode(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat credential file %s: %w", path, err)
	}
	mode := info.Mode().Perm()
	if mode&0o077 != 0 {
		return fmt.Errorf("credential file %s has mode %04o; must be 0600 (group/other access forbidden)", path, mode)
	}
	return nil
}

// WarnCredentialFileMode is the soft-fail variant: log a WARN with
// the path + offending mode but continue. Used at callsites where
// breaking the legacy code path on first encounter would be hostile
// to existing operators (e.g. a long-running daemon that's been
// reading a permissive file for months). The hard-fail
// VerifyCredentialFileMode is the goal; WarnCredentialFileMode is a
// transition stepping stone.
//
// Returns the same error the verifier would return, OR nil when the
// file is clean. The caller decides whether to propagate.
func WarnCredentialFileMode(path string, logger *slog.Logger) error {
	err := VerifyCredentialFileMode(path)
	if err != nil && logger != nil {
		logger.Warn("credential file has overly-permissive mode",
			"path", path,
			"error", err,
		)
	}
	return err
}
