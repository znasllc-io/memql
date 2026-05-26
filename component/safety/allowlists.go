package safety

// WorkbenchExecAllowlist is the set of binaries the workbench's
// per-Plan sandbox permits in `exec` actions. Single source of truth
// for the data; both the rule-engine workbench-allowlist rule
// (`ruleShellWorkbenchAllowlist`) and the legacy
// `integrations/workbench/exec_allowlist.go` enforcement helper read
// from here.
//
// The set is intentionally narrow. Adding a binary is a deliberate
// security decision -- "do agents need it for the task?" not "would
// it be convenient." Anything that can spawn arbitrary subprocesses
// (`sudo`, `xargs sh`, `env COMMAND`) is excluded. Path-bearing
// invocations (`/usr/bin/python3`, `./script.sh`) are matched against
// their basename -- the boundary cares about which BINARY runs, not
// where on disk it lives.
//
// Pre-migration (issue #110), this lived in
// integrations/workbench/exec_allowlist.go. Issue #228 moved it
// here so the rule engine + the legacy helper read the same set.
var WorkbenchExecAllowlist = map[string]struct{}{
	// File + path inspection
	"ls": {}, "cat": {}, "head": {}, "tail": {}, "less": {}, "more": {},
	"find": {}, "stat": {}, "file": {}, "wc": {}, "pwd": {}, "readlink": {},
	"basename": {}, "dirname": {}, "tree": {}, "which": {},
	// File mutation (workspace is per-Plan, contained)
	"cp": {}, "mv": {}, "rm": {}, "mkdir": {}, "chmod": {}, "touch": {},
	"ln": {},
	// Text processing
	"grep": {}, "awk": {}, "sed": {}, "sort": {}, "uniq": {}, "diff": {},
	"cut": {}, "tr": {}, "tee": {}, "xargs": {}, "echo": {}, "printf": {},
	"yes": {}, "test": {}, "true": {}, "false": {}, "env": {}, "date": {},
	// Archives + compression
	"tar": {}, "gzip": {}, "gunzip": {}, "zip": {}, "unzip": {}, "bzip2": {},
	"bunzip2": {}, "xz": {}, "unxz": {},
	// Hashing
	"md5sum": {}, "sha256sum": {}, "sha1sum": {}, "shasum": {},
	// Networking (read-only fetch)
	"curl": {}, "wget": {},
	// Language toolchains
	"python": {}, "python3": {}, "pip": {}, "pip3": {},
	"node": {}, "npm": {}, "npx": {}, "yarn": {}, "pnpm": {},
	"go": {}, "gofmt": {},
	"ruby": {}, "gem": {}, "bundle": {},
	"rustc": {}, "cargo": {},
	"java": {}, "javac": {}, "mvn": {}, "gradle": {},
	"perl": {},
	// Source control
	"git": {},
	// Data tooling
	"jq": {}, "yq": {},
}

// safeReadOnlyBinaries is the fast-path allowlist used by
// ruleShellSafeBinary -- a deliberately tight subset of binaries
// whose normal use can't damage system state or exfiltrate data.
// Side-effect-free / read-only utilities only. Things like `curl`,
// `wget`, `git`, `chmod`, `rm`, `pip` are NOT here -- they're on the
// broader WorkbenchExecAllowlist but reach the LLM classifier
// because they CAN do harmful things (and a rule above catches the
// common abuse patterns).
//
// Used for the "common case" optimization: if a command's only
// segment binary is in this set and no dangerous-pattern rule fired,
// it's tier=low and the LLM never gets called.
var safeReadOnlyBinaries = map[string]struct{}{
	"ls": {}, "cat": {}, "head": {}, "tail": {}, "wc": {},
	"grep": {}, "find": {}, "stat": {}, "file": {}, "pwd": {},
	"which": {}, "readlink": {}, "basename": {}, "dirname": {},
	"tree": {}, "echo": {}, "printf": {}, "true": {}, "false": {},
	"test": {}, "date": {}, "yes": {},
}
