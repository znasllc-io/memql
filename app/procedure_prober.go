package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
	procedure "github.com/znasllc-io/memql/integrations/procedure"
)

// procedure_prober.go -- the environment a replay is about to run in,
// measured against what the procedure learned (epic memql#5408, D16).
//
// IT REPORTS ONLY WHAT IT MEASURED. A predicate it could not measure -- a tool
// the host would not run, a workspace it could not list -- is ABSENT from the
// answer, and component/procedure.CheckPreconditions reads absent as
// "unmeasured, does not hold". A prober that filled a gap with a guess would
// be the one place a replay could start on a precondition nobody checked.
//
// IT MEASURES ONLY WHAT WAS LEARNED. Every probe is a dispatch on the target,
// through the same gates a step goes through; probing a toolchain the
// procedure never runs would spend a call on an answer nothing compares.
//
// THE WORKBENCH IS NOT ASKED FOR ITS PLATFORM. CheckPreconditions does not
// compare the platform on the workbench -- a portable footprint is
// platform-independent (D4) -- and `uname` is not on the workbench's exec
// allowlist, so asking would spend a refused call to measure nothing that is
// read. The machine is asked, because there it is compared.
//
// Tool versions are reported as the tool's own first line: normalizing them
// is CheckPreconditions' job, on both sides, and a second normalizer here is
// a second opinion about what a version is.

// procedureHostBuilder builds the host a probe runs on, for one owner's
// replay run.
type procedureHostBuilder func(ctx context.Context, owner, runId string) (procedureHost, error)

// procedureProber is the procedure.Prober. It holds one host per target this
// node can reach; a target with none is refused by name.
type procedureProber struct {
	hosts map[work.ReplayTarget]procedureHostBuilder
}

var _ procedure.Prober = (*procedureProber)(nil)

// procedureProbeTimeoutSec bounds one probe command. A version flag answers
// at once; one that has not answered in this long is not going to be the
// reason a replay is worth waiting for.
const procedureProbeTimeoutSec = 30

// setHost installs the host for one target.
func (p *procedureProber) setHost(target work.ReplayTarget, build procedureHostBuilder) {
	if build == nil {
		return
	}
	if p.hosts == nil {
		p.hosts = map[work.ReplayTarget]procedureHostBuilder{}
	}
	p.hosts[target] = build
}

// measures reports whether any target has a host.
func (p *procedureProber) measures() bool { return p != nil && len(p.hosts) > 0 }

// Probe measures, on target, the predicates learned carries.
func (p *procedureProber) Probe(ctx context.Context, target work.ReplayTarget, ownerUserId, runId string, learned proc.Preconditions) (proc.Preconditions, error) {
	build := p.hosts[target]
	if build == nil {
		return proc.Preconditions{}, fmt.Errorf("procedure probe: this node cannot measure the %s", target)
	}
	owner := strings.TrimSpace(ownerUserId)
	if owner == "" || strings.TrimSpace(runId) == "" {
		return proc.Preconditions{}, fmt.Errorf("procedure probe: an owner and the replay run are required, and a probe with neither measures nobody's environment")
	}
	ctx = auth.ContextWithUserActor(ctx, owner)
	host, err := build(ctx, owner, strings.TrimSpace(runId))
	if err != nil {
		return proc.Preconditions{}, fmt.Errorf("procedure probe: %w", err)
	}

	var observed proc.Preconditions
	if target != work.TargetWorkbench && len(learned.Platform) > 0 {
		if payload, ok := procedureProbeExec(ctx, host, "uname -s -m"); ok {
			if goos, goarch, ok := procedurePlatformOf(procedureVersionLine(payload)); ok {
				observed.Platform = map[string]string{"os": goos, "arch": goarch}
			}
		}
	}

	names := make([]string, 0, len(learned.Tools))
	for name := range learned.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !procedureToolWord.MatchString(name) {
			continue
		}
		payload, ok := procedureProbeExec(ctx, host, procedureVersionCommand(name))
		if !ok {
			continue
		}
		if line := procedureVersionLine(payload); line != "" {
			if observed.Tools == nil {
				observed.Tools = map[string]string{}
			}
			observed.Tools[name] = line
		}
	}

	if learned.EmptyWorkspace != nil {
		if empty, ok := procedureProbeEmptyWorkspace(ctx, host); ok {
			observed.EmptyWorkspace = &empty
		}
	}
	return observed, nil
}

// procedureProbeExec runs one probe command with no working directory -- a
// version flag does not depend on one -- and answers its output when it exited
// cleanly.
func procedureProbeExec(ctx context.Context, host procedureHost, cmd string) (map[string]any, bool) {
	reply, err := host.call(ctx, "exec", map[string]any{"cmd": cmd, "timeoutSec": procedureProbeTimeoutSec})
	if err != nil || reply.ErrorCode != "" {
		return nil, false
	}
	if code, ok := procedurePayloadInt(reply.Payload["exitCode"]); !ok || code != 0 {
		return nil, false
	}
	return reply.Payload, true
}

// procedureProbeEmptyWorkspace answers whether the replay's workspace has no
// entries. A workspace that does not exist yet is empty: the replay creates
// it. A listing that was cut short is not a count.
func procedureProbeEmptyWorkspace(ctx context.Context, host procedureHost) (bool, bool) {
	dir, _, err := host.resolvePath(ctx, ".")
	if err != nil {
		return false, false
	}
	reply, err := host.call(ctx, "fs_list", map[string]any{"path": dir})
	if err != nil {
		return false, false
	}
	if reply.OK {
		if truncated, _ := reply.Payload["truncated"].(bool); truncated {
			return false, true
		}
		if n, ok := procedurePayloadInt(reply.Payload["count"]); ok {
			return n == 0, true
		}
		if entries, ok := reply.Payload["entries"].([]any); ok {
			return len(entries) == 0, true
		}
		return false, false
	}
	stat, err := host.call(ctx, "fs_stat", map[string]any{"path": dir})
	if err != nil || !stat.OK {
		return false, false
	}
	if exists, known := stat.Payload["exists"].(bool); known && !exists {
		return true, true
	}
	return false, false
}
