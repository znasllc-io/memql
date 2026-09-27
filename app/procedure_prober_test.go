package app

import (
	"context"
	"strings"
	"testing"

	proc "github.com/znasllc-io/memql/component/procedure"
	"github.com/znasllc-io/memql/component/work"
)

// procedure_prober_test.go -- the prober reports ONLY what it measured, and
// measures only what was learned. Its answers are checked through
// component/procedure.CheckPreconditions, because that is the only reader
// whose verdict matters: a prober is right when the check it feeds is.

// scriptedExec answers an exec by command.
func scriptedExec(answers map[string]map[string]any) func(map[string]any) map[string]any {
	return func(args map[string]any) map[string]any {
		cmd, _ := args["cmd"].(string)
		if a, ok := answers[cmd]; ok {
			return a
		}
		return map[string]any{"exitCode": 127, "stderr": cmd + ": not found"}
	}
}

func workbenchProber(wb *fakeHostWorkspace) *procedureProber {
	p := &procedureProber{}
	p.setHost(work.TargetWorkbench, func(_ context.Context, _ string, runId string) (procedureHost, error) {
		return &workbenchProcedureHost{handler: wb.handler(), runId: runId}, nil
	})
	return p
}

func execCommands(f *fakeHostWorkspace) []string {
	var out []string
	for _, c := range f.calls {
		if c["action"] == "exec" {
			args, _ := c["args"].(map[string]any)
			cmd, _ := args["cmd"].(string)
			out = append(out, cmd)
		}
	}
	return out
}

func TestTheProberMeasuresEachLearnedToolWithItsVersionCommand(t *testing.T) {
	wb := newFakeWorkbench()
	wb.exec = scriptedExec(map[string]map[string]any{
		"go version":     {"exitCode": 0, "stdout": "go version go1.22.1 linux/amd64\n"},
		"node --version": {"exitCode": 0, "stdout": "v22.1.0\n"},
	})
	learned := proc.Preconditions{Tools: map[string]string{"go": "1.22.1", "node": "22.1.0"}}
	observed, err := workbenchProber(wb).Probe(context.Background(), work.TargetWorkbench, "v1:identity:user:owner", "v1:work:run:replay1", learned)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(execCommands(wb), "|"); got != "go version|node --version" {
		t.Fatalf("commands = %s", got)
	}
	// Reported as the tool's own line; the check normalizes both sides.
	if observed.Tools["go"] != "go version go1.22.1 linux/amd64" || observed.Tools["node"] != "v22.1.0" {
		t.Fatalf("tools = %+v", observed.Tools)
	}
	if r := proc.CheckPreconditions(learned, observed, proc.TargetWorkbench); !r.Held {
		t.Fatalf("the check did not hold: %+v", r)
	}
	// And a different version is a mismatch, which is the whole point.
	wb.exec = scriptedExec(map[string]map[string]any{
		"go version":     {"exitCode": 0, "stdout": "go version go1.21.0 linux/amd64\n"},
		"node --version": {"exitCode": 0, "stdout": "v22.1.0\n"},
	})
	observed, _ = workbenchProber(wb).Probe(context.Background(), work.TargetWorkbench, "v1:identity:user:owner", "v1:work:run:replay1", learned)
	if r := proc.CheckPreconditions(learned, observed, proc.TargetWorkbench); r.Held || len(r.Mismatches) != 1 {
		t.Fatalf("an older go was not a mismatch: %+v", r)
	}
}

// The workbench does not compare the platform, and `uname` is not on its
// exec allowlist: asking would spend a refused call on nothing that is read.
func TestTheWorkbenchIsNeverAskedForItsPlatform(t *testing.T) {
	wb := newFakeWorkbench()
	learned := proc.Preconditions{Platform: map[string]string{"os": "darwin", "arch": "arm64"}}
	observed, err := workbenchProber(wb).Probe(context.Background(), work.TargetWorkbench, "v1:identity:user:owner", "v1:work:run:replay1", learned)
	if err != nil {
		t.Fatal(err)
	}
	if len(wb.calls) != 0 || observed.Platform != nil {
		t.Fatalf("the workbench was asked %v and answered %+v", wb.actions(), observed.Platform)
	}
	if r := proc.CheckPreconditions(learned, observed, proc.TargetWorkbench); !r.Held {
		t.Fatalf("a Mac recording on the workbench did not hold: %+v", r)
	}
}

func TestTheProberMeasuresAnEmptyWorkspaceByListingIt(t *testing.T) {
	learned := proc.Preconditions{EmptyWorkspace: boolPtr(true)}
	wb := newFakeWorkbench()
	observed, err := workbenchProber(wb).Probe(context.Background(), work.TargetWorkbench, "v1:identity:user:owner", "v1:work:run:replay1", learned)
	if err != nil {
		t.Fatal(err)
	}
	if observed.EmptyWorkspace == nil || !*observed.EmptyWorkspace {
		t.Fatalf("an empty workspace = %+v", observed.EmptyWorkspace)
	}
	if args := wb.lastArgs(); wb.actions()[0] != "fs_list" || args["path"] != "." {
		t.Fatalf("listed %v %+v, want the workspace root", wb.actions(), args)
	}
	wb.files["leftover.txt"] = "x"
	observed, _ = workbenchProber(wb).Probe(context.Background(), work.TargetWorkbench, "v1:identity:user:owner", "v1:work:run:replay1", learned)
	if observed.EmptyWorkspace == nil || *observed.EmptyWorkspace {
		t.Fatalf("a workspace with a file = %+v", observed.EmptyWorkspace)
	}
	if r := proc.CheckPreconditions(learned, observed, proc.TargetWorkbench); r.Held {
		t.Fatalf("a non-empty workspace held: %+v", r)
	}
}

// Unmeasured is ABSENT, and absent does not hold: a tool the host refused to
// run, or one whose command failed, is not reported at all.
func TestAToolTheHostCannotRunIsLeftUnmeasured(t *testing.T) {
	wb := newFakeWorkbench()
	wb.refuse["exec"] = "command_not_allowed"
	learned := proc.Preconditions{Tools: map[string]string{"make": "4.3"}}
	observed, err := workbenchProber(wb).Probe(context.Background(), work.TargetWorkbench, "v1:identity:user:owner", "v1:work:run:replay1", learned)
	if err != nil {
		t.Fatal(err)
	}
	if _, present := observed.Tools["make"]; present {
		t.Fatalf("a refused probe was reported: %+v", observed.Tools)
	}
	if r := proc.CheckPreconditions(learned, observed, proc.TargetWorkbench); r.Held || len(r.Unmeasured) != 1 {
		t.Fatalf("an unmeasured tool held: %+v", r)
	}
	wb = newFakeWorkbench()
	wb.exec = scriptedExec(nil) // every command exits 127
	observed, _ = workbenchProber(wb).Probe(context.Background(), work.TargetWorkbench, "v1:identity:user:owner", "v1:work:run:replay1", learned)
	if len(observed.Tools) != 0 {
		t.Fatalf("a failing version command was reported: %+v", observed.Tools)
	}
}

func TestAToolNameThatIsNotAWordIsNeverPutOnACommandLine(t *testing.T) {
	wb := newFakeWorkbench()
	learned := proc.Preconditions{Tools: map[string]string{"rm -rf /": "1", "$(curl x)": "1"}}
	observed, err := workbenchProber(wb).Probe(context.Background(), work.TargetWorkbench, "v1:identity:user:owner", "v1:work:run:replay1", learned)
	if err != nil {
		t.Fatal(err)
	}
	if len(wb.calls) != 0 || len(observed.Tools) != 0 {
		t.Fatalf("ran %v, reported %+v", execCommands(wb), observed.Tools)
	}
}

func TestTheProberMeasuresNothingThatWasNotLearned(t *testing.T) {
	wb := newFakeWorkbench()
	observed, err := workbenchProber(wb).Probe(context.Background(), work.TargetWorkbench, "v1:identity:user:owner", "v1:work:run:replay1", proc.Preconditions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(wb.calls) != 0 || observed.Platform != nil || observed.Tools != nil || observed.EmptyWorkspace != nil {
		t.Fatalf("an empty learned set cost %v and answered %+v", wb.actions(), observed)
	}
}

func TestAProberWithNoHostForTheTargetRefusesByName(t *testing.T) {
	p := workbenchProber(newFakeWorkbench())
	if _, err := p.Probe(context.Background(), work.TargetMachine, "v1:identity:user:owner", "v1:work:run:replay1", proc.Preconditions{}); err == nil || !strings.Contains(err.Error(), "machine") {
		t.Fatalf("a machine probe on a node without one = %v", err)
	}
	if _, err := p.Probe(context.Background(), work.TargetWorkbench, "", "v1:work:run:replay1", proc.Preconditions{}); err == nil {
		t.Fatal("a probe with no owner measured somebody")
	}
}

// fakeProbeHost is a host a test scripts directly: the machine, as the
// prober sees it.
type fakeProbeHost struct {
	ws *fakeHostWorkspace
}

func (h *fakeProbeHost) label() string { return "the machine" }
func (h *fakeProbeHost) call(ctx context.Context, action string, args map[string]any) (procedureHostReply, error) {
	return (&workbenchProcedureHost{handler: h.ws.handler(), runId: "r"}).call(ctx, action, args)
}
func (h *fakeProbeHost) resolvePath(_ context.Context, p string) (string, string, error) {
	return "/Users/someone/work/r/" + p, p, nil
}
func (h *fakeProbeHost) prepareCommand(context.Context, procedureCommandLine) (map[string]any, error) {
	return nil, nil
}
func (h *fakeProbeHost) delivers(string) bool { return true }

// The MACHINE compares the platform, so it is asked -- in Go's names, which
// is how the cockpit's fingerprint records it.
func TestTheMachineIsAskedForItsPlatformInGosNames(t *testing.T) {
	ws := newFakeWorkbench()
	ws.exec = scriptedExec(map[string]map[string]any{"uname -s -m": {"exitCode": 0, "stdout": "Darwin arm64\n"}})
	p := &procedureProber{}
	p.setHost(work.TargetMachine, func(context.Context, string, string) (procedureHost, error) { return &fakeProbeHost{ws: ws}, nil })
	learned := proc.Preconditions{Platform: map[string]string{"os": "darwin", "arch": "arm64"}}
	observed, err := p.Probe(context.Background(), work.TargetMachine, "v1:identity:user:owner", "v1:work:run:replay1", learned)
	if err != nil {
		t.Fatal(err)
	}
	if observed.Platform["os"] != "darwin" || observed.Platform["arch"] != "arm64" {
		t.Fatalf("platform = %+v", observed.Platform)
	}
	if r := proc.CheckPreconditions(learned, observed, proc.TargetMachine); !r.Held {
		t.Fatalf("the check did not hold: %+v", r)
	}
	ws.exec = scriptedExec(map[string]map[string]any{"uname -s -m": {"exitCode": 0, "stdout": "Linux x86_64\n"}})
	observed, _ = p.Probe(context.Background(), work.TargetMachine, "v1:identity:user:owner", "v1:work:run:replay1", learned)
	if r := proc.CheckPreconditions(learned, observed, proc.TargetMachine); r.Held {
		t.Fatalf("a Linux machine held a Mac precondition: %+v", r)
	}
}

// A workspace that does not exist yet is empty -- the replay makes it -- and
// is established by fs_stat, never assumed from a failed listing.
func TestAWorkspaceThatDoesNotExistYetIsEmpty(t *testing.T) {
	ws := newFakeWorkbench()
	ws.refuse["fs_list"] = "fs_list_failed"
	p := &procedureProber{}
	p.setHost(work.TargetMachine, func(context.Context, string, string) (procedureHost, error) { return &fakeProbeHost{ws: ws}, nil })
	observed, err := p.Probe(context.Background(), work.TargetMachine, "v1:identity:user:owner", "v1:work:run:replay1", proc.Preconditions{EmptyWorkspace: boolPtr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if observed.EmptyWorkspace == nil || !*observed.EmptyWorkspace {
		t.Fatalf("a missing workspace = %+v", observed.EmptyWorkspace)
	}
	if got := strings.Join(ws.actions(), ","); got != "fs_list,fs_stat" {
		t.Fatalf("actions = %s", got)
	}
}

func boolPtr(b bool) *bool { return &b }

// A TARGET THAT IS NOT THERE IS NOT A PRECONDITION THAT FAILED. A probe the
// target could not answer -- no workbench peer, a dropped stream -- says
// nothing about the environment, so the prober answers an ERROR, which the
// runner does not count against the procedure, rather than an empty
// measurement, which the check would read as every learned tool missing and
// the ladder would count as a refused start. The control: a tool the host
// answered it cannot run is still left unmeasured, with no error.
func TestAnUnavailableTargetIsAnErrorNotAnEmptyMeasurement(t *testing.T) {
	learned := proc.Preconditions{Tools: map[string]string{"go": "1.22.1"}}
	for _, code := range []string{"no_workbench_peer", "worker_disconnected", "worker_unreachable", "forward_failed"} {
		wb := newFakeWorkbench()
		wb.refuse["exec"] = code
		_, err := workbenchProber(wb).Probe(context.Background(), work.TargetWorkbench, "v1:identity:user:owner", "v1:work:run:replay1", learned)
		if err == nil || !strings.Contains(err.Error(), code) {
			t.Fatalf("%s: a probe the target could not answer = %v, want an error naming it", code, err)
		}
	}

	wb := newFakeWorkbench()
	wb.refuse["exec"] = "command_not_allowed"
	observed, err := workbenchProber(wb).Probe(context.Background(), work.TargetWorkbench, "v1:identity:user:owner", "v1:work:run:replay1", learned)
	if err != nil {
		t.Fatalf("a tool the host refused to run is unmeasured, not an unavailable target: %v", err)
	}
	if _, measured := observed.Tools["go"]; measured {
		t.Fatalf("a refused probe measured something: %+v", observed.Tools)
	}
}
