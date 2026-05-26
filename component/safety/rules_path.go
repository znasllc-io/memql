package safety

import (
	"fmt"
	"strings"
)

// Path rules cover the ActionFS{Read,Write,List,Stat} surfaces. The
// classifier looks at Payload.Paths -- one or many strings, each a
// path the action would touch. A single sensitive path is enough
// to fire the rule (any-match), since the action would still
// perform the dangerous read/write.

// sensitiveReadPaths are paths whose READ is a near-certain
// credential_access signal. Matching uses suffix / substring rules
// rather than glob so an absolute / relative / home-relative form
// of the same path all hit.
//
// Conservative on `.env` (only fires on a path whose basename is
// exactly `.env` or starts with `.env.`) since random files named
// `.environment` etc. would false-positive otherwise.
var sensitiveReadPatterns = []string{
	"/.ssh/id_",            // ~/.ssh/id_rsa, ~/.ssh/id_ed25519, ...
	"/.ssh/identity",       // legacy SSH identity
	"/.aws/credentials",    // AWS keypair
	"/.gnupg/",             // GPG keyring directory
	"/etc/shadow",          // hashed passwords
	"/etc/gshadow",         // hashed group passwords
	"/etc/sudoers",         // sudo policy (often reveals priv-esc surface)
	"/.docker/config.json", // docker registry creds
	"/.npmrc",              // npm auth tokens
	"/.pypirc",             // PyPI tokens
	"/.netrc",              // legacy network creds
	"/.config/gh/hosts",    // gh CLI tokens
}

func ruleFSReadSensitive(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionFSRead {
		return Classification{}, ErrNoClassifierOpinion
	}
	for _, p := range desc.Payload.Paths {
		if hitSensitive, why := matchSensitiveRead(p); hitSensitive {
			return cls(TierHigh,
				fmt.Sprintf("fs_read of credential-bearing path %q (%s)", p, why),
				CategoryCredentialAccess), nil
		}
	}
	return Classification{}, ErrNoClassifierOpinion
}

func matchSensitiveRead(path string) (bool, string) {
	low := strings.ToLower(path)
	for _, pat := range sensitiveReadPatterns {
		if strings.Contains(low, pat) {
			return true, "matches " + pat
		}
	}
	// `.env` and `.env.*` (production / local / test) are common
	// credential-laden files.
	base := basename(path)
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return true, "dotenv-style file"
	}
	return false, ""
}

// sensitiveWritePatterns are paths whose WRITE is a near-certain
// persistence / privilege-escalation signal. All entries are
// lower-case so the case-insensitive match against ToLower(path)
// works regardless of how the caller cased the input.
var sensitiveWritePatterns = []string{
	"/.ssh/authorized_keys", // remote-access persistence
	"/.ssh/config",          // SSH client persistence
	"/.bashrc", "/.bash_profile", "/.bash_login", "/.bash_logout",
	"/.zshrc", "/.zprofile", "/.zlogin",
	"/.profile", "/.kshrc", "/.cshrc", "/.tcshrc", "/.fishrc",
	"/etc/cron.", "/var/spool/cron/", "/etc/sudoers", "/etc/passwd", "/etc/shadow",
	"/library/launchagents/", "/library/launchdaemons/",
	"/.config/systemd/user/",
}

func ruleFSWriteSystem(desc ActionDescriptor) (Classification, error) {
	if desc.Action != ActionFSWrite {
		return Classification{}, ErrNoClassifierOpinion
	}
	for _, p := range desc.Payload.Paths {
		if hit, why, cat := matchSensitiveWrite(p); hit {
			return cls(TierHigh,
				fmt.Sprintf("fs_write to %q (%s)", p, why),
				cat...), nil
		}
	}
	return Classification{}, ErrNoClassifierOpinion
}

func matchSensitiveWrite(path string) (bool, string, []Category) {
	low := strings.ToLower(path)
	for _, pat := range sensitiveWritePatterns {
		if strings.Contains(low, pat) {
			// Categorize: shell-rc / cron / launchd = persistence;
			// /etc system files = priv-esc (often persistence too).
			cats := []Category{CategoryPersistence}
			if strings.HasPrefix(low, "/etc/") || strings.Contains(low, "/.ssh/authorized_keys") {
				cats = []Category{CategoryPersistence, CategoryPrivilegeEscalation}
			}
			return true, "matches " + pat, cats
		}
	}
	// Generic /usr/bin/ / /usr/sbin/ / /System/ writes are
	// system-modify priv-esc.
	if strings.HasPrefix(low, "/usr/bin/") ||
		strings.HasPrefix(low, "/usr/sbin/") ||
		strings.HasPrefix(low, "/usr/local/bin/") ||
		strings.HasPrefix(low, "/usr/local/sbin/") ||
		strings.HasPrefix(low, "/system/") {
		return true, "system binary path", []Category{CategoryPrivilegeEscalation}
	}
	return false, "", nil
}

// basename returns path's last component without importing path or
// filepath -- the rule code is hot-path-y and these helpers are
// shared across rules.
func basename(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}
