package tools

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/znasllc-io/memql-cockpit/internal/worker/apps"
	"github.com/znasllc-io/memql-cockpit/internal/worker/harness"
	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// Policy controls allow/deny decisions for the cockpit's headless
// tools. Loaded from ~/.memql/policy.yaml at startup; reload via
// SIGHUP.
//
// Defaults (DefaultPolicy) ship a curated allow list for common
// development commands and a sticky deny list for destructive ops.
// User policy can extend `Allow` and `Deny`; `Deny` always beats
// `Allow`.
type Policy struct {
	mu          sync.RWMutex
	shell       ShellPolicy
	fs          FSPolicy
	http        HTTPPolicy
	apps        AppsPolicy
	models      ModelsPolicy
	backup      BackupPolicy
	pipelines   PipelinesPolicy
	workbenches WorkbenchesPolicy
	configPath  string
}

// ShellPolicy controls workerHost.exec.
//
// RunAsUser is the optional system username to setuid to before
// running the child process. Empty (the default) inherits the
// cockpit-worker process's uid. Setting this to a dedicated user
// like "memql-worker-exec" gives shell exec a clean blast radius
// even if the worker itself runs as the user. NOTE: setuid
// requires the cockpit-worker process to be running as root (or
// have the appropriate capability); silently ignored otherwise.
//
// The MaxCPUSeconds / MaxMemoryMB / MaxOpenFiles fields apply
// rlimits in the child shell before the command starts on Linux/macOS; the
// worker's own limits are unchanged. macOS does not apply max_memory_mb.
// Zero or negative values leave the corresponding
// rlimit at the inherited default.
type ShellPolicy struct {
	Allow         []string `yaml:"allow"`
	Deny          []string `yaml:"deny"`
	RunAsUser     string   `yaml:"run_as_user"`
	MaxCPUSeconds int      `yaml:"max_cpu_seconds"`
	MaxMemoryMB   int      `yaml:"max_memory_mb"`
	MaxOpenFiles  int      `yaml:"max_open_files"`
}

// FSPolicy controls workerHost.fs_*.
type FSPolicy struct {
	WorkspaceRoot string   `yaml:"workspace_root"`
	Allow         []string `yaml:"allow"`
	Deny          []string `yaml:"deny"`
}

// AppsPolicy controls which local apps this machine will let each cluster
// drive (memql-cockpit#346), PER CLUSTER:
//
//	apps:
//	  homes:
//	    api.memql.localhost:     # a home id from workers.yaml
//	      allow:
//	        - claude-code
//	        - codex
//
// DEFAULT-DENY, and deliberately so. An app session does exactly what
// workerHost.exec does -- edits files and runs commands on somebody's own
// computer -- so it gets the same posture the rest of this file has:
// nothing runs until the machine's owner says which app may. A cluster the
// block does not name is allowed NOTHING, which is the state of every
// machine that has not been configured and of every cluster paired after
// the owner wrote this block.
//
// PER CLUSTER, because consent here is the owner's word about one cluster.
// A single machine-wide list meant that allowing Claude Code to test it
// against a local cluster offered it to production too -- the crossing the
// per-home tool consent windows already rule out (consent/homes.go,
// memql-cockpit#433). The machine-wide `apps.allow` this replaced is not
// read at all; a file that still has it is TOLD so (AppConsentProblems),
// with the command that writes the per-cluster entry, rather than being
// either honoured or silently ignored.
//
// An app that is present but not listed is still REPORTED, with
// allowed=false. That is what makes the portal able to say
// "present, blocked" instead of rendering it identically to "not
// installed" -- one of those an operator can fix, the other sends them
// looking for the wrong problem.
//
// LEVELS ARE THE OPPOSITE POSTURE, and on purpose (memql-cockpit#438,
// design D8). allow decides WHETHER an app may run here, which only the
// owner can grant; levels decide HOW it runs once it may -- which model and
// effort a LEVEL the engine names becomes -- and the cockpit ships an
// answer for that (internal/worker/harness BuiltinLevels). So an absent
// block is the built-in table, not "nothing":
//
//	apps:
//	  levels:
//	    claude-code:
//	      reasoning:
//	        model: fable
//	        effort: max
//	    codex:
//	      strong:
//	        model: gpt-5.5
//	        effort: xhigh
//
// An entry replaces its level's built-in entry WHOLE, model and effort
// together; a level with no entry keeps the built-in one; an entry with
// neither knob runs that level at the app's own defaults. An entry the app
// would misread REFUSES its level rather than falling back -- see
// AppLevels.
type AppsPolicy struct {
	// Homes is apps.homes, kept as the YAML node it was written as and read
	// by readAppsHomes, for the reason Levels below is: a typed decode
	// fails the WHOLE file on a shorthand (`api.memql.localhost:
	// [claude-code]`), and a worker that cannot parse policy.yaml runs on
	// defaults that lose every other line the owner wrote. Walking the node
	// makes that one entry's problem, and allows nothing for it.
	Homes yaml.Node `yaml:"homes"`
	// RetiredAllow is the machine-wide `apps.allow` this file no longer
	// honours. It is decoded only so a file that still carries it can be
	// told so; nothing reads it as consent.
	RetiredAllow yaml.Node `yaml:"allow"`
	// Levels is kept as the YAML node it was written as, and read by
	// readAppLevels, rather than decoded into Go types here. A typed decode
	// has two failure modes and both are wrong for this block. A key it
	// does not know (`efort: max`) is DROPPED without a word -- and since an
	// entry replaces its row whole, the owner's effort silently becomes the
	// app's default, the very failure CheckKnobs exists to prevent. A shape
	// it does not expect (`reasoning: opus`) fails the WHOLE file, and a
	// worker that cannot parse policy.yaml runs on the defaults, which allow
	// no app at all. Walking the node turns both into a sentence about the
	// one entry that is wrong.
	Levels yaml.Node `yaml:"levels"`
}

// ModelsPolicy controls which local models this machine will serve, and
// which OpenAI-compatible runtimes it knows about (memql-cockpit#359).
//
//	models:
//	  allow:
//	    - qwen3.5:9b
//	    - qwen3-embedding:0.6b
//	  runtimes:
//	    - name: lmstudio
//	      base_url: http://127.0.0.1:1234/v1
//	      models:
//	        - id: qwen2.5-7b-instruct
//	          context_window: 32768
//	          structured_output: true
//	    - name: local-speech
//	      base_url: http://127.0.0.1:8880/v1
//	      models:
//	        - id: kokoro-82m
//	          audio_out: true
//
// DEFAULT-DENY, for the reason app consent is. Serving a model call spends
// this machine's own GPU on somebody else's prompt, so nothing is offered
// until the machine's owner says which model may be. An empty allow list
// is the state of every machine upgrading into this feature, and it must
// not mean "all".
//
// A model that is present but unlisted is still REPORTED, blocked --
// which is what lets the portal say "present, blocked" rather than
// rendering it identically to "not installed".
//
// The runtime declaration types live in the models package rather than
// here: they are the shape discovery consumes, and the yaml tags belong
// with the struct that carries the fields. models imports nothing from
// this repo, so the direction stays acyclic.
//
// models.pull is the OPPOSITE posture -- DEFAULT TRUE -- and the
// inconsistency is the point rather than an oversight. Allowing a model
// spends this machine's GPU on somebody ELSE's prompt, which is a grant
// only the machine's owner can make; PULLING one is that owner acting on
// their own machine, and the engine gates the act on being that owner
// before it ever reaches this process. A default-deny pull switch would
// mean every machine in the fleet answered "Pull" with a refusal until
// somebody edited a file on it by hand, on the machine -- which is the
// exact errand this feature exists to remove. An operator who wants the
// machine to refuse says so:
//
//	models:
//	  pull: false
type ModelsPolicy struct {
	Allow    []string                 `yaml:"allow"`
	Runtimes []models.DeclaredRuntime `yaml:"runtimes"`
	// Pull is a POINTER, and that is the whole of how the default-true
	// posture survives contact with a yaml file. An absent key
	// unmarshals into the zero value, so a plain bool would read every
	// policy.yaml written before this key existed -- which is all of
	// them -- as `pull: false`, and every machine already in the fleet
	// would start refusing pulls with nothing on it saying why. nil
	// means "the file has no opinion", and the accessor answers that
	// with the default. Do not "simplify" this to a bool.
	Pull *bool `yaml:"pull"`
	// Parallel is the per-model concurrency this machine advertises for its
	// native Ollama (the `max=` on each model label), for a runtime whose
	// OLLAMA_NUM_PARALLEL the cockpit cannot read from a unit it wrote. Absent
	// is "not said". It is kept as the YAML node it was written as, and read by
	// readModelsTuning rather than decoded here: a typed decode fails the WHOLE
	// file on `parallel: "2"`, and reads `parallel: 2.5` as 2 without a word.
	Parallel yaml.Node `yaml:"parallel"`
	// Resident lists the models this machine keeps loaded and how
	// (docs/superpowers/specs/2026-10-18-fleet-tuning-design.md). Absent
	// changes nothing: no keep_alive is sent and no window is pinned. It is
	// kept as a node for the reason Parallel is: `working_context: 16k` would
	// fail the whole file, and `working_ctx: 8192` would be dropped silently.
	Resident yaml.Node `yaml:"resident"`
	// Unknown is every key directly under models: that none of the fields above
	// takes. A typed decode drops such a key without a word, so `residnet:` read
	// exactly like an owner who never asked for anything; readModelsTuning names
	// each one. The values are nodes only so that any shape decodes: nothing
	// reads them.
	Unknown map[string]yaml.Node `yaml:",inline"`
}

// BackupPolicy controls which folders this machine will back up into the
// Library (memql#4841).
//
//	backup:
//	  roots:
//	    - ~/Clients
//	    - /Volumes/Work
//
// DEFAULT-DENY, the same posture as app consent and for a stronger reason.
// A watched folder is arranged in the GRAPH -- somebody sets it up in a
// browser, on a different machine -- so the path in it is one the CLUSTER is
// naming on somebody else's computer. That is exactly the situation
// appsession's CheckWorkspace exists for, and it gets the same answer: the
// engine may ASK, and this machine decides. Without it, anyone who could
// write a watch row could point this cockpit at ~/.ssh and have it uploaded.
//
// An empty roots list is the state of every machine that has not been
// configured, including every machine upgrading into this feature, and it
// must not mean "all".
//
// A refusal is REPORTED rather than silent: the sweep answers
// originState=refused_by_policy, which the Files app renders as "this machine
// said no" with the repair -- add the path here -- named on screen. A machine
// that quietly ignored a watch would be indistinguishable from one that was
// offline.
type BackupPolicy struct {
	DownloadRoots []string `yaml:"download_roots"`
	Roots         []string `yaml:"roots"`
}

// PipelinesPolicy controls workerHost.pipeline_step: whether the cluster may
// run one of its CI pipeline steps on this machine (memql#5494).
//
//	pipelines:
//	  allow: true
//	  repos:
//	    - acme/widgets
//	  workspace_root: ~/ci
//	  max_timeout_sec: 3600
//
// DEFAULT-DENY, and this is the owner's STANDING CONSENT: a step is admitted
// by this block and never by a consent window, because a CI run arrives when a
// push lands, not while somebody is at the machine to grant one. Nothing about
// a laptop is a default place for CI. The cluster's router only picks a
// machine that advertises pipelines=allowed, and the worker advertises it
// exactly when allow is true -- the refusal here is the second consent, not
// the only one.
//
// THE TRUST, PLAINLY: allow lets the cluster's pipeline runner run commands
// it chooses, as the user the worker runs as, with that user's environment
// and files, for the repositories it names. This machine cannot see the
// pipeline the command came from. repos, when it lists any, narrows which
// repositories (owner/name, compared without regard to case or a .git
// suffix) -- and the clone URL must name the same repository, so the list
// filters what is cloned rather than a name the request states. A step an
// agent dispatched is refused whatever this block says: the runner
// dispatches with no agent.
//
// workspace_root is where each step's fresh checkout is made and removed
// again; absent, it is fs.workspace_root/pipelines, else ~/.memql/pipelines.
// max_timeout_sec caps the timeout a step asks for (default 3600).
// container_memory_mib sizes the command container only (default 2048).
// Services keep their separate 2 GiB caps. The request cannot override this.
//
// The whole block REPLACES on reload, unlike the allow lists above: it is a
// consent, and one a SIGHUP could not take back would be a grant the file no
// longer states.
type PipelinesPolicy struct {
	Allow              bool     `yaml:"allow"`
	Repos              []string `yaml:"repos"`
	WorkspaceRoot      string   `yaml:"workspace_root"`
	MaxTimeoutSec      int      `yaml:"max_timeout_sec"`
	ContainerMemoryMiB int      `yaml:"container_memory_mib"`
}

// DefaultPipelineMaxTimeoutSec is the longest a pipeline step may run when
// policy.yaml names no max_timeout_sec.
const DefaultPipelineMaxTimeoutSec = 3600

const DefaultPipelineContainerMemoryMiB = 2048

// Check decides whether this machine runs a pipeline step of repository. The
// refusal is a sentence naming the setting that changes it: it reaches the
// run's log, and it is the only account of the refusal anybody will read.
func (pp PipelinesPolicy) Check(repository string) error {
	if !pp.Allow {
		return errors.New("this machine runs no pipeline steps: its owner has not set pipelines.allow: true in policy.yaml")
	}
	if pp.ContainerMemoryMiB != 0 && (pp.ContainerMemoryMiB < 256 || pp.ContainerMemoryMiB > 32768) {
		return errors.New("pipelines.container_memory_mib must be between 256 and 32768 MiB, or omitted for the 2048 MiB default")
	}
	if len(pp.Repos) == 0 {
		return nil
	}
	want := normalRepository(repository)
	if want == "" {
		return errors.New("this machine runs pipeline steps only for the repositories in pipelines.repos, and the step names no repository")
	}
	for _, listed := range pp.Repos {
		if normalRepository(listed) == want {
			return nil
		}
	}
	return fmt.Errorf("this machine's policy does not list %s: add it to pipelines.repos in policy.yaml to run its steps here", strings.TrimSpace(repository))
}

// normalRepository reads owner/name the way GitHub does: without regard to
// case, surrounding space or a trailing .git.
func normalRepository(r string) string {
	r = strings.ToLower(strings.TrimSpace(r))
	return strings.TrimSuffix(r, ".git")
}

// HTTPPolicy controls workerHost.http_fetch.
type HTTPPolicy struct {
	AllowURLs       []string `yaml:"allow_urls"`
	DenyURLs        []string `yaml:"deny_urls"`
	MaxBodyBytes    int      `yaml:"max_body_bytes"`
	MaxRedirects    int      `yaml:"max_redirects"`
	BlockPrivateNet bool     `yaml:"block_private_net"`
}

// rawPolicy is the YAML-unmarshal target.
type rawPolicy struct {
	Shell       ShellPolicy       `yaml:"shell"`
	FS          FSPolicy          `yaml:"fs"`
	HTTP        HTTPPolicy        `yaml:"http"`
	Apps        AppsPolicy        `yaml:"apps"`
	Backup      BackupPolicy      `yaml:"backup"`
	Models      ModelsPolicy      `yaml:"models"`
	Pipelines   PipelinesPolicy   `yaml:"pipelines"`
	Workbenches WorkbenchesPolicy `yaml:"workbenches"`
}

// DefaultPolicy returns the baseline allow/deny lists shipped with
// the cockpit binary.
func DefaultPolicy() *Policy {
	return &Policy{
		shell: ShellPolicy{
			Allow: []string{
				"git", "npm", "yarn", "pnpm", "pip", "pip3", "python", "python3",
				"node", "go", "cargo", "make", "ls", "cat", "grep", "find",
				"mkdir", "touch", "mv", "cp", "echo", "pwd", "head", "tail",
				"wc", "sort", "uniq", "diff", "tar", "unzip", "zip", "ssh-keygen",
				"docker", "kubectl",
				// macOS-specific launchers. `open` is the canonical
				// "launch this app / URL / document" command on macOS;
				// without it the agent can't fulfil "open Chrome" or
				// "open this folder in Finder" via shell, which is the
				// cleaner path than scripting cmd+space + type + return.
				// `osascript` is the AppleScript runner that bridges to
				// any Mac app's scripting dictionary -- "tell Mail to
				// send this", "tell Calendar to add an event", etc.
				// Both are user-level commands; neither escalates
				// privileges and the FS deny list still gates writes
				// to sensitive paths (~/.ssh, /etc/shadow, etc.).
				"open", "osascript",
			},
			Deny: []string{
				"rm", "dd", "mkfs", "sudo", "su", "chown", "chmod",
				"kill", "killall", "reboot", "shutdown", "halt", "poweroff",
				"curl", "wget", "fdisk", "format", "mount", "umount",
			},
			// Per-call rlimits. Earlier defaults left these as zero
			// (inherit parent), which gave a runaway shell exec the
			// full ulimit ceiling of the user session -- a
			// fork-bomb-shaped agent could exhaust CPU + memory of
			// the operator's laptop before the kill switch fired.
			// 5 minutes / 1 GiB / 1024 fds is generous for normal
			// dev work but caps the blast radius. Operators with
			// genuine heavier workloads can override via worker
			// policy.yaml.
			//
			// Note: RLIMIT_AS (memory) is a no-op on macOS today --
			// macOS lacks a portable equivalent; see
			// exec_unix_darwin.go. The cap still applies on Linux.
			MaxCPUSeconds: 300,
			MaxMemoryMB:   1024,
			MaxOpenFiles:  1024,
		},
		fs: FSPolicy{
			WorkspaceRoot: "",
			Deny: []string{
				"~/.ssh", "~/.aws", "~/.config", "~/.kube",
				"~/Library/Cookies", "~/Library/Application Support/Google/Chrome",
				"/etc/shadow", "/etc/passwd", "/private/etc/master.passwd",
			},
		},
		http: HTTPPolicy{
			MaxBodyBytes:    50 * 1024 * 1024,
			MaxRedirects:    5,
			BlockPrivateNet: true,
		},
	}
}

// LoadPolicy reads the YAML file at path and merges it on top of
// DefaultPolicy. Missing file returns DefaultPolicy.
func LoadPolicy(path string) (*Policy, error) {
	p := DefaultPolicy()
	p.configPath = path
	if err := p.reload(); err != nil {
		return p, err
	}
	return p, nil
}

// Reload re-reads the policy file. Called on SIGHUP from the worker
// runner.
func (p *Policy) Reload() error {
	if p == nil {
		return nil
	}
	return p.reload()
}

func (p *Policy) reload() error {
	if p == nil || p.configPath == "" {
		return nil
	}
	data, err := readFile(p.configPath)
	if err != nil {
		if errors.Is(err, errNotExist) {
			return nil
		}
		return fmt.Errorf("policy: read %s: %w", p.configPath, err)
	}
	var raw rawPolicy
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("policy: parse %s: %w", p.configPath, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.shell.Allow = mergeUnique(p.shell.Allow, raw.Shell.Allow)
	p.shell.Deny = mergeUnique(p.shell.Deny, raw.Shell.Deny)
	if raw.Shell.RunAsUser != "" {
		p.shell.RunAsUser = raw.Shell.RunAsUser
	}
	if raw.Shell.MaxCPUSeconds > 0 {
		p.shell.MaxCPUSeconds = raw.Shell.MaxCPUSeconds
	}
	if raw.Shell.MaxMemoryMB > 0 {
		p.shell.MaxMemoryMB = raw.Shell.MaxMemoryMB
	}
	if raw.Shell.MaxOpenFiles > 0 {
		p.shell.MaxOpenFiles = raw.Shell.MaxOpenFiles
	}
	if raw.FS.WorkspaceRoot != "" {
		p.fs.WorkspaceRoot = raw.FS.WorkspaceRoot
	}
	p.fs.Allow = mergeUnique(p.fs.Allow, raw.FS.Allow)
	p.fs.Deny = mergeUnique(p.fs.Deny, raw.FS.Deny)
	p.backup.Roots = mergeUnique(p.backup.Roots, raw.Backup.Roots)
	p.backup.DownloadRoots = mergeUnique(p.backup.DownloadRoots, raw.Backup.DownloadRoots)
	p.http.AllowURLs = mergeUnique(p.http.AllowURLs, raw.HTTP.AllowURLs)
	p.http.DenyURLs = mergeUnique(p.http.DenyURLs, raw.HTTP.DenyURLs)
	if raw.HTTP.MaxBodyBytes > 0 {
		p.http.MaxBodyBytes = raw.HTTP.MaxBodyBytes
	}
	if raw.HTTP.MaxRedirects > 0 {
		p.http.MaxRedirects = raw.HTTP.MaxRedirects
	}
	if raw.HTTP.BlockPrivateNet {
		p.http.BlockPrivateNet = true
	}
	// apps.homes REPLACES: it is a
	// consent, and merging it would make withdrawing one -- `memql worker
	// apps --deny`, or deleting the line -- take a restart. There is no
	// baseline either way: DefaultPolicy names no cluster, which is the
	// default-deny above.
	p.apps.Homes = raw.Apps.Homes
	p.apps.RetiredAllow = raw.Apps.RetiredAllow
	// apps.levels REPLACES, for the reason models.runtimes does below: an
	// entry is a record, and merging two generations of one would run a
	// model from one file at an effort from another. A SIGHUP that removed
	// the block returns this machine to the built-in table.
	p.apps.Levels = raw.Apps.Levels
	// models.allow merges the way the shell and fs lists do, so SIGHUP
	// makes a newly pulled model offerable without a worker restart.
	p.models.Allow = mergeUnique(p.models.Allow, raw.Models.Allow)
	// models.pull REPLACES rather than merges, for the reason the
	// runtimes below do and one of its own: the field is tri-state, so
	// "absent" is a meaningful value (the default, true). Merging would
	// make `pull: false` unremovable without a worker restart -- an
	// operator who deleted the line would keep the refusal and have
	// nothing left in the file that explained it.
	p.models.Pull = raw.Models.Pull
	// Runtimes REPLACE rather than merge, and the asymmetry is
	// deliberate: an allow entry is a bare name, where a runtime is a
	// record with a base URL, a key variable and a model list. Merging
	// two records that share a name produces a hybrid neither the
	// operator nor this code intended -- an endpoint moved to a new port
	// would keep answering on the old one until a restart.
	p.models.Runtimes = raw.Models.Runtimes
	// parallel and resident REPLACE, for models.pull's reason and one of
	// their own: each is a setting whose absence is the default, so a merge
	// would keep a model loaded for hours on the strength of a line the owner
	// already deleted, and a SIGHUP is how they take the deletion back.
	p.models.Parallel = raw.Models.Parallel
	p.models.Resident = raw.Models.Resident
	p.models.Unknown = raw.Models.Unknown
	// pipelines REPLACES, whole, for the same reason: allow is a consent, and
	// a narrowed repos list that kept the repository it dropped would admit a
	// step the file no longer names.
	p.pipelines = raw.Pipelines
	p.workbenches = raw.Workbenches
	return nil
}

// PipelinesAllowed reports pipelines.allow: whether this machine runs CI
// pipeline steps at all. The worker advertises pipelines=allowed on Register
// exactly when it is true.
func (p *Policy) PipelinesAllowed() bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.pipelines.Allow
}

// Pipelines returns the pipelines block as it stands, with its defaults
// applied: WorkspaceRoot resolved to an absolute directory and MaxTimeoutSec
// defaulted. Read once per step, under one lock, so a SIGHUP mid-step cannot
// hand it half of one file and half of another. Repos is the caller's own
// copy, for AppsAllow's reason.
func (p *Policy) Pipelines() PipelinesPolicy {
	if p == nil {
		return PipelinesPolicy{
			WorkspaceRoot:      pipelinesWorkspaceRoot("", ""),
			MaxTimeoutSec:      DefaultPipelineMaxTimeoutSec,
			ContainerMemoryMiB: DefaultPipelineContainerMemoryMiB,
		}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := p.pipelines
	if len(out.Repos) > 0 {
		out.Repos = append([]string(nil), out.Repos...)
	}
	out.WorkspaceRoot = pipelinesWorkspaceRoot(p.pipelines.WorkspaceRoot, p.fs.WorkspaceRoot)
	if out.MaxTimeoutSec <= 0 {
		out.MaxTimeoutSec = DefaultPipelineMaxTimeoutSec
	}
	if out.ContainerMemoryMiB == 0 {
		out.ContainerMemoryMiB = DefaultPipelineContainerMemoryMiB
	}
	return out
}

// pipelinesWorkspaceRoot is where pipeline steps make their checkouts:
// pipelines.workspace_root when set, else the shell's workspace root plus
// /pipelines, else ~/.memql/pipelines -- under the Linux user unit, ~/.memql
// is the one directory in the home the worker may write.
func pipelinesWorkspaceRoot(pipelinesRoot, shellRoot string) string {
	if root, ok := absoluteRoot(pipelinesRoot); ok {
		return root
	}
	if root, ok := absoluteRoot(shellRoot); ok {
		return filepath.Join(root, "pipelines")
	}
	if home := homeDir(); home != "" {
		return filepath.Join(home, ".memql", "pipelines")
	}
	return filepath.Join(os.TempDir(), "memql-pipelines")
}

// absoluteRoot resolves a configured root (~ expanded, made absolute), or
// reports that none is configured.
func absoluteRoot(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", false
	}
	root, err := canonicalPath(p)
	if err != nil {
		return "", false
	}
	return root, true
}

// CheckShell returns nil if the supplied command is allowed.
func (p *Policy) CheckShell(cmd string) error {
	if p == nil {
		return errors.New("policy: not configured")
	}
	binary := firstToken(cmd)
	if binary == "" {
		return errors.New("policy: empty command")
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, deny := range p.shell.Deny {
		if strings.EqualFold(binary, deny) {
			return fmt.Errorf("shell deny list: %q", binary)
		}
	}
	for _, allow := range p.shell.Allow {
		if strings.EqualFold(binary, allow) {
			return nil
		}
	}
	return fmt.Errorf("shell allow list: %q not allowed", binary)
}

// CheckPath rejects paths that escape the workspace root or land
// inside a sensitive directory.
func (p *Policy) CheckPath(path string) error {
	if p == nil {
		return errors.New("policy: not configured")
	}
	cleaned, err := canonicalPath(path)
	if err != nil {
		return err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, deny := range p.fs.Deny {
		if matchesPath(cleaned, expandHome(deny)) {
			return fmt.Errorf("fs deny list: %q under %q", cleaned, deny)
		}
	}
	if p.fs.WorkspaceRoot != "" {
		root := expandHome(p.fs.WorkspaceRoot)
		if !strings.HasPrefix(cleaned, root) {
			return fmt.Errorf("fs: path %q outside workspace root %q", cleaned, root)
		}
	}
	return nil
}

// CheckBackupPath decides whether this machine will back up a folder the
// cluster named (memql#4841).
//
// SEPARATE FROM CheckPath, and deliberately not layered on it. CheckPath
// answers "may a tool touch this", and its workspace root is a SINGLE
// directory an agent works inside; a backup is a standing arrangement over
// somebody's own documents, which live in several places and never inside a
// workspace root. Reusing it would have made the feature unusable and then
// tempted somebody to widen fs.workspace_root, which would widen every tool
// call on the machine at the same time.
//
// The fs DENY list still applies, because a directory somebody marked as
// never-touch is not a directory to upload either.
func (p *Policy) CheckBackupPath(path string) error {
	if p == nil {
		return errors.New("policy: not configured")
	}
	cleaned, err := canonicalPath(path)
	if err != nil {
		return err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, deny := range p.fs.Deny {
		if matchesPath(cleaned, expandHome(deny)) {
			return fmt.Errorf("this machine's policy denies %q (fs.deny lists %q)", cleaned, deny)
		}
	}
	if len(p.backup.Roots) == 0 {
		return fmt.Errorf("this machine backs up nothing yet: add %q (or a folder above it) to backup.roots in policy.yaml", cleaned)
	}
	resolved, e := resolveBackupPath(cleaned)
	if e != nil {
		return e
	}
	for _, deny := range p.fs.Deny {
		if matchesPath(resolved, expandHome(deny)) {
			return errors.New("the source resolves to a denied path")
		}
	}
	for _, root := range p.backup.Roots {
		allowed, e := resolveBackupPath(expandHome(root))
		if e == nil && matchesPath(resolved, allowed) {
			return nil
		}
	}
	return fmt.Errorf("this machine's policy does not list %q: add it (or a folder above it) to backup.roots in policy.yaml", cleaned)
}

// BackupRoots returns a copy of the folders this machine will back up.
// The copy matters for AppsAllowFor's reason: a SIGHUP can reload underneath a
// sweep that is already walking.
func (p *Policy) BackupRoots() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.backup.Roots) == 0 {
		return nil
	}
	out := make([]string, len(p.backup.Roots))
	copy(out, p.backup.Roots)
	return out
}

// CheckURL applies the http policy: allow/deny lists, SSRF
// protection (private network detection).
func (p *Policy) CheckURL(rawURL string) error {
	if p == nil {
		return errors.New("policy: not configured")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("http: invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("http: unsupported scheme %q", u.Scheme)
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, deny := range p.http.DenyURLs {
		if strings.HasPrefix(rawURL, deny) {
			return fmt.Errorf("http deny list: %q", rawURL)
		}
	}
	if len(p.http.AllowURLs) > 0 {
		matched := false
		for _, allow := range p.http.AllowURLs {
			if strings.HasPrefix(rawURL, allow) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("http allow list: %q", rawURL)
		}
	}
	if p.http.BlockPrivateNet {
		host := u.Hostname()
		if isPrivateAddress(host) {
			return fmt.Errorf("http SSRF block: private/loopback address %q", host)
		}
	}
	return nil
}

// MaxBodyBytes returns the configured response body cap.
func (p *Policy) MaxBodyBytes() int {
	if p == nil {
		return 50 * 1024 * 1024
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.http.MaxBodyBytes <= 0 {
		return 50 * 1024 * 1024
	}
	return p.http.MaxBodyBytes
}

// MaxRedirects returns the configured redirect cap.
func (p *Policy) MaxRedirects() int {
	if p == nil {
		return 5
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.http.MaxRedirects <= 0 {
		return 5
	}
	return p.http.MaxRedirects
}

// AppsAllowFor returns the app ids policy.yaml allows for ONE cluster:
// apps.homes.<home>.allow, with <home> the id workers.yaml gives it. A
// cluster the block does not name gets nil -- default-deny per cluster,
// never an inheritance from a machine-wide list or from another cluster.
//
// The home and the ids are read without regard to case or surrounding
// space: home ids are hostnames and registry names, and app ids are read
// the way apps.levels reads them, so the keys an owner writes side by side
// agree about what they name.
//
// The slice is the caller's own, built fresh from the block on every call:
// the worker asks on every heartbeat and at every session start, and a
// shared slice would race a SIGHUP reload. There is no second, cached copy
// of the answer for a reload to leave stale.
func (p *Policy) AppsAllowFor(home string) []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return readAppsHomes(p.apps.Homes).allow[normalHomeID(home)]
}

// AppConsentProblems is every problem with this machine's app consent, as
// sentences an owner can act on: an entry in apps.homes this cockpit
// cannot read (it allows nothing), and the retired machine-wide apps.allow
// (which allows nothing anywhere). The worker logs them when the file is
// read and `memql worker apps` prints them, because a consent that
// silently did nothing is indistinguishable from a machine that is broken.
func (p *Policy) AppConsentProblems() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	problems := readAppsHomes(p.apps.Homes).problems
	if retired := retiredAllowProblem(p.apps.RetiredAllow); retired != "" {
		problems = append([]string{retired}, problems...)
	}
	return problems
}

// AppLevels returns the owner's apps.levels entries for one app that
// stand, and -- per level -- the sentence refusing any entry the app would
// misread. No entries at all is the built-in table (the session lays these
// over harness.BuiltinLevels), which is what an absent block means.
//
// A REFUSED ENTRY REFUSES ITS LEVEL; it does not fall back to the built-in
// entry. An owner who pinned a cheaper model for a level did not agree to
// the expensive default because of a typo, and Claude Code in particular
// IGNORES an effort word it does not know rather than refusing it -- so the
// refusal here is the only one anybody would see. It is REPORTED on the
// session's End, naming the line to fix, where a quiet fallback would be
// indistinguishable from the owner's entry working.
//
// The maps are the caller's own, built fresh on every call, for
// AppsAllowFor's reason: a SIGHUP can reload underneath a session that is
// still deciding.
func (p *Policy) AppLevels(appID string) (harness.Table, map[string]string) {
	if p == nil {
		return nil, nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	read := readAppLevels(p.apps.Levels)
	id := normalAppID(appID)
	return read.table[id], read.refused[id]
}

// AppLevelProblems is every problem apps.levels has, as sentences an owner
// can act on: the refusals AppLevels reports, and the entries no session can
// reach -- an app this cockpit does not drive, a word that is not a level,
// the embeddings level -- which are ignored. The worker logs them when the
// file is read and `memql worker apps` prints them, because an entry that
// silently did nothing is indistinguishable from one that worked.
func (p *Policy) AppLevelProblems() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return readAppLevels(p.apps.Levels).problems
}

// appsHomesRead is apps.homes read against what this cockpit can drive.
type appsHomesRead struct {
	// allow holds the app ids each cluster is allowed, by normalHomeID.
	allow map[string][]string
	// problems is every sentence, in a stable order.
	problems []string
}

// readAppsHomes validates apps.homes. Like readAppLevels it is a pure
// function of the block as written, run on every read, and it never fails:
// a shape it cannot read becomes a sentence about that entry, and that
// entry allows NOTHING -- the default-deny direction, never a guess at
// what the owner meant. Keys are walked in sorted order so the problem
// list does not shuffle between two reads of one file.
func readAppsHomes(block yaml.Node) appsHomesRead {
	read := appsHomesRead{allow: map[string][]string{}}
	root := resolveYAML(&block)
	if yamlAbsent(root) {
		return read
	}
	if root.Kind != yaml.MappingNode {
		read.problems = append(read.problems,
			"apps.homes: a mapping of clusters belongs here (api.memql.example.com:), not "+describeYAML(root)+
				", so no app is allowed for any cluster")
		return read
	}
	byHome := yamlMapping(root)
	for _, key := range sortedKeys(byHome) {
		where := "apps.homes." + key
		entry := resolveYAML(byHome[key])
		if yamlAbsent(entry) {
			// `local:` with nothing under it allows nothing, which is what
			// it reads as; not a problem.
			continue
		}
		if entry.Kind != yaml.MappingNode {
			read.problems = append(read.problems, fmt.Sprintf(
				"%s: a mapping with allow: belongs here (for example {allow: [claude-code]}), not %s, so no app is allowed for it",
				where, describeYAML(entry)))
			continue
		}
		fields := yamlMapping(entry)
		for _, field := range sortedKeys(fields) {
			if field != "allow" {
				read.problems = append(read.problems, fmt.Sprintf(
					"%s: takes allow, and %q is not it, so that key is ignored", where, field))
			}
		}
		list := resolveYAML(fields["allow"])
		if yamlAbsent(list) {
			continue
		}
		if list.Kind != yaml.SequenceNode {
			read.problems = append(read.problems, fmt.Sprintf(
				"%s.allow: a list of apps belongs here (allow: [claude-code]), not %s, so no app is allowed for it",
				where, describeYAML(list)))
			continue
		}
		home := normalHomeID(key)
		for _, item := range list.Content {
			item = resolveYAML(item)
			if item == nil || item.Kind != yaml.ScalarNode || yamlAbsent(item) {
				read.problems = append(read.problems, fmt.Sprintf(
					"%s.allow: each entry is one app id, and one is not, so that entry allows nothing", where))
				continue
			}
			id := normalAppID(item.Value)
			if _, ok := apps.SpecFor(id); !ok {
				read.problems = append(read.problems, fmt.Sprintf(
					"%s.allow: this cockpit drives no app called %q (it drives %s), so that entry allows nothing",
					where, item.Value, knownAppIDs()))
				continue
			}
			if !slices.Contains(read.allow[home], id) {
				read.allow[home] = append(read.allow[home], id)
			}
		}
	}
	return read
}

// retiredAllowProblem is the sentence for a file that still carries the
// machine-wide apps.allow, or "" when it does not. It names the command
// that writes the per-cluster entry -- and removes this list in the same
// edit -- with the ids the list held, so the owner decides which cluster
// they meant rather than having every cluster decided for them.
func retiredAllowProblem(n yaml.Node) string {
	root := resolveYAML(&n)
	if yamlAbsent(root) {
		return ""
	}
	var flags []string
	if root.Kind == yaml.SequenceNode {
		for _, item := range root.Content {
			if item = resolveYAML(item); item != nil && item.Kind == yaml.ScalarNode && !yamlAbsent(item) {
				flags = append(flags, "--allow "+strings.TrimSpace(item.Value))
			}
		}
	}
	if len(flags) == 0 {
		flags = []string{"--allow <app>"}
	}
	return "apps.allow is no longer read: app consent is per cluster now, so this machine-wide list allows nothing anywhere. " +
		"Allow an app for the cluster you mean with `memql worker apps " + strings.Join(flags, " ") + " --home <cluster>` " +
		"(`memql worker apps` lists the clusters), which writes apps.homes.<cluster>.allow and removes apps.allow"
}

// normalHomeID reads a home id the way apps.homes keys are matched: without
// regard to case or surrounding space.
func normalHomeID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// appLevelsRead is apps.levels read against what this cockpit can drive.
type appLevelsRead struct {
	// table holds the entries that stand, per app id.
	table map[string]harness.Table
	// refused holds, per app id and level, the sentence refusing sessions
	// at that level.
	refused map[string]map[string]string
	// problems is every sentence, in a stable order.
	problems []string
}

// readAppLevels validates apps.levels. It is a pure function of the block
// as written, run on every read rather than cached, so there is no second
// copy of the answer for a reload to leave stale -- and it never fails: a
// shape it cannot read becomes a problem about that entry, never an error
// that would cost the rest of policy.yaml.
//
// Every judgement is borrowed rather than restated: the app ids are
// apps.Specs', the level words are the engine's (harness.CheckAppLevel), and
// what an app would misread is the harness's own check (harness.CheckKnobs,
// the same one every harness runs in Start). The keys are walked in sorted
// order so a problem list does not shuffle between two reads of one file,
// which would read as the file changing.
func readAppLevels(block yaml.Node) appLevelsRead {
	read := appLevelsRead{
		table:   map[string]harness.Table{},
		refused: map[string]map[string]string{},
	}
	root := resolveYAML(&block)
	if yamlAbsent(root) {
		return read
	}
	if root.Kind != yaml.MappingNode {
		read.problems = append(read.problems,
			"apps.levels: a mapping of apps belongs here (claude-code:, codex:), not "+describeYAML(root)+", so the block is ignored")
		return read
	}
	byApp := yamlMapping(root)
	for _, key := range sortedKeys(byApp) {
		appID := normalAppID(key)
		spec, ok := apps.SpecFor(appID)
		if !ok {
			read.problems = append(read.problems, fmt.Sprintf(
				"apps.levels.%s: this cockpit drives no app called %q (it drives %s), so the entry is ignored",
				key, key, knownAppIDs()))
			continue
		}
		levelsNode := byApp[key]
		if yamlAbsent(levelsNode) {
			continue
		}
		if levelsNode.Kind != yaml.MappingNode {
			read.problems = append(read.problems, fmt.Sprintf(
				"apps.levels.%s: a mapping of levels belongs here (fast:, strong:, reasoning:), not %s, so the entry is ignored",
				key, describeYAML(levelsNode)))
			continue
		}
		levels := yamlMapping(levelsNode)
		for _, level := range sortedKeys(levels) {
			where := "apps.levels." + key + "." + level
			if err := harness.CheckAppLevel(level); err != nil {
				read.problems = append(read.problems, fmt.Sprintf("%s: %v, so the entry is ignored", where, err))
				continue
			}
			knobs, err := readLevelEntry(levels[level])
			if err == nil {
				err = harness.CheckKnobs(spec.Harness, knobs)
			}
			if err != nil {
				sentence := fmt.Sprintf("%s: %v -- this machine refuses %s sessions for %s until the entry is fixed",
					where, err, level, appID)
				if read.refused[appID] == nil {
					read.refused[appID] = map[string]string{}
				}
				read.refused[appID][level] = sentence
				read.problems = append(read.problems, sentence)
				continue
			}
			if read.table[appID] == nil {
				read.table[appID] = harness.Table{}
			}
			read.table[appID][level] = knobs
		}
	}
	return read
}

// readLevelEntry reads one apps.levels entry: a mapping of `model` and
// `effort`, each a single word, either one optional. Null -- `fast:` with
// nothing after it -- reads as `fast: {}`, an entry with no knobs.
//
// Anything else is an error NAMING what was wrong: a scalar or a list where
// the mapping belongs (`reasoning: opus`), a key that is neither knob
// (`efort`), a knob that is not a single word. The caller refuses the level
// with it, because dropping the key and running the rest of the entry would
// replace the whole row with half of what the owner wrote.
func readLevelEntry(n *yaml.Node) (harness.Knobs, error) {
	n = resolveYAML(n)
	if yamlAbsent(n) {
		return harness.Knobs{}, nil
	}
	if n.Kind != yaml.MappingNode {
		return harness.Knobs{}, fmt.Errorf("an entry is a mapping of model and effort (for example {model: opus, effort: high}), not %s",
			describeYAML(n))
	}
	var k harness.Knobs
	entry := yamlMapping(n)
	for _, key := range sortedKeys(entry) {
		v := resolveYAML(entry[key])
		if key != "model" && key != "effort" {
			return harness.Knobs{}, fmt.Errorf("an entry takes model and effort, and %q is neither", key)
		}
		if v.Kind != yaml.ScalarNode {
			return harness.Knobs{}, fmt.Errorf("%s is %s, where a single word belongs", key, describeYAML(v))
		}
		word := strings.TrimSpace(v.Value)
		if yamlAbsent(v) {
			word = ""
		}
		if key == "model" {
			k.Model = word
		} else {
			k.Effort = word
		}
	}
	return k, nil
}

// yamlMapping returns a mapping node's pairs by key. yaml.v3 has already
// refused a duplicate key when it parsed the file, so a key appears once.
func yamlMapping(n *yaml.Node) map[string]*yaml.Node {
	out := make(map[string]*yaml.Node, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		out[n.Content[i].Value] = n.Content[i+1]
	}
	return out
}

// resolveYAML follows an alias (`*name`) to the node it names.
func resolveYAML(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// yamlAbsent is a key that is not there, or one written with nothing after
// it -- which YAML reads as null.
func yamlAbsent(n *yaml.Node) bool {
	return n == nil || n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.Tag == "!!null")
}

// describeYAML names a node's shape for a sentence, quoting a scalar so the
// owner sees the very word they wrote.
func describeYAML(n *yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return fmt.Sprintf("the single word %q", n.Value)
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	}
	return "something else"
}

// normalAppID reads an app id the way the detector reads app consent:
// without regard to case or surrounding space, so the two keys an owner
// writes side by side agree about what they name.
func normalAppID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

// knownAppIDs names the apps this cockpit drives, for a sentence.
func knownAppIDs() string {
	var ids []string
	for _, s := range apps.Specs() {
		ids = append(ids, s.ID)
	}
	return strings.Join(ids, ", ")
}

// ModelsAllow returns a copy of the allowed model ids. Empty is
// default-deny, not "all".
func (p *Policy) ModelsAllow() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.models.Allow) == 0 {
		return nil
	}
	out := make([]string, len(p.models.Allow))
	copy(out, p.models.Allow)
	return out
}

// ModelsPullAllowed reports whether this machine will pull a model when
// it is asked to -- by its own CLI, and by the engine over ModelPullStart
// (the arm in internal/worker/modelpull.go reads it before running).
//
// TRUE when the key is absent, which is the opposite of every other
// answer this file gives. The reasoning is on ModelsPolicy: a pull is the
// machine's own owner acting on their own machine, and a fleet-wide
// silent refusal is a worse failure than a pull somebody did not want,
// which the engine's owner-only gate already prevents.
//
// It returns the VALUE, never the pointer, for the reason ModelsAllow
// returns a copy: a SIGHUP reload replaces this field underneath a caller
// that is still deciding, and a caller holding the pointer would be
// reading a switch that flipped between its own two lines.
func (p *Policy) ModelsPullAllowed() bool {
	if p == nil {
		// A build that loaded no policy at all has refused nothing.
		// Answering false here would refuse a pull the owner asked for
		// with no line in any file to point at -- the same direction
		// MaxBodyBytes takes for a nil policy.
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.models.Pull == nil {
		return true
	}
	return *p.models.Pull
}

// ModelRuntimes returns a copy of the declared OpenAI-compatible
// runtimes. The nested model slices are copied too: a caller that
// appended to one would be editing the live policy under the lock this
// method just released.
func (p *Policy) ModelRuntimes() []models.DeclaredRuntime {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.models.Runtimes) == 0 {
		return nil
	}
	out := make([]models.DeclaredRuntime, len(p.models.Runtimes))
	for i, rt := range p.models.Runtimes {
		out[i] = rt
		if len(rt.Models) > 0 {
			out[i].Models = make([]models.DeclaredModel, len(rt.Models))
			copy(out[i].Models, rt.Models)
		}
	}
	return out
}

// ShellLimits exposes the rlimit + privilege-drop knobs to the
// exec runner. Zero values mean "inherit from parent process".
type ShellLimits struct {
	RunAsUser     string
	MaxCPUSeconds int
	MaxMemoryMB   int
	MaxOpenFiles  int
}

// ShellLimits returns a copy of the active shell-policy limits.
func (p *Policy) ShellLimits() ShellLimits {
	if p == nil {
		return ShellLimits{}
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return ShellLimits{
		RunAsUser:     p.shell.RunAsUser,
		MaxCPUSeconds: p.shell.MaxCPUSeconds,
		MaxMemoryMB:   p.shell.MaxMemoryMB,
		MaxOpenFiles:  p.shell.MaxOpenFiles,
	}
}

// WorkspaceRoot returns the configured fs workspace root (already
// expanded).
func (p *Policy) WorkspaceRoot() string {
	if p == nil {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.fs.WorkspaceRoot == "" {
		return ""
	}
	return expandHome(p.fs.WorkspaceRoot)
}

// DenyPaths returns fs.deny with every "~" expanded: the paths CheckPath
// refuses a tool, which an app session's app may neither read nor write
// either (appsession/protected.go). A copy, for AppsAllow's reason.
func (p *Policy) DenyPaths() []string {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]string, 0, len(p.fs.Deny))
	for _, deny := range p.fs.Deny {
		out = append(out, expandHome(deny))
	}
	return out
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func firstToken(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	for i, r := range cmd {
		if r == ' ' || r == '\t' {
			return cmd[:i]
		}
	}
	return cmd
}

// sortedKeys returns a map's keys in order, for the walks whose output a
// person reads.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func mergeUnique(a, b []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(a)+len(b))
	add := func(s string) {
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	for _, v := range a {
		add(v)
	}
	for _, v := range b {
		add(v)
	}
	return out
}

func canonicalPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("path: empty")
	}
	expanded := expandHome(p)
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("path: abs %q: %w", p, err)
	}
	return filepath.Clean(abs), nil
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home := homeDir()
	if home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

func matchesPath(target, prefix string) bool {
	if prefix == "" {
		return false
	}
	cleanedTarget := filepath.Clean(target)
	cleanedPrefix := filepath.Clean(prefix)
	return cleanedTarget == cleanedPrefix || strings.HasPrefix(cleanedTarget, cleanedPrefix+string(filepath.Separator))
}

func isPrivateAddress(host string) bool {
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isPrivateIP(ip)
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		// Conservative default: treat unresolvable hosts as
		// non-private (the actual fetch will fail anyway).
		return false
	}
	for _, ip := range addrs {
		if isPrivateIP(ip) {
			return true
		}
	}
	return false
}

func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsPrivate() ||
		ip.IsUnspecified() {
		return true
	}
	// Cloud metadata endpoints.
	metadataAddrs := []string{
		"169.254.169.254", // AWS / GCP / Azure IMDS
		"fd00:ec2::254",   // AWS IPv6 IMDS
	}
	str := ip.String()
	for _, m := range metadataAddrs {
		if str == m {
			return true
		}
	}
	return false
}

// Download permission is separate from read permission. Merely consenting to
// back up a directory never authorizes the cluster to write into it.
func (p *Policy) CheckBackupDownloadPath(path string) error {
	if p == nil {
		return errors.New("backup downloads are not allowed on this machine")
	}
	cleaned, e := canonicalPath(path)
	if e != nil {
		return e
	}
	resolved, e := resolveBackupPath(cleaned)
	if e != nil {
		return e
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, deny := range p.fs.Deny {
		if matchesPath(cleaned, expandHome(deny)) || matchesPath(resolved, expandHome(deny)) {
			return errors.New("the download destination is denied by this machine")
		}
	}
	for _, root := range p.backup.DownloadRoots {
		allowed, e := resolveBackupPath(expandHome(root))
		if e == nil && matchesPath(resolved, allowed) && resolved != allowed {
			return nil
		}
	}
	return errors.New("allow this destination in backup.download_roots on this machine before downloading")
}
func resolveBackupPath(p string) (string, error) {
	absolute, e := filepath.Abs(p)
	if e != nil {
		return "", e
	}
	tail := []string{}
	current := absolute
	for {
		resolved, e := filepath.EvalSymlinks(current)
		if e == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return resolved, nil
		}
		if !errors.Is(e, os.ErrNotExist) {
			return "", e
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", e
		}
		tail = append(tail, filepath.Base(current))
		current = parent
	}
}

// Enabling the isolated runtime grants no host commands, apps or desktop use.
type WorkbenchesPolicy struct {
	Allow bool `yaml:"allow"`
}

func (p *Policy) WorkbenchesAllowed() bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.workbenches.Allow
}
