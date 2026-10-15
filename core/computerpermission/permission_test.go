package computerpermission

import (
	"reflect"
	"testing"
)

func TestComputerPermissionCommandInputPreservesOriginalDefaultArguments(t *testing.T) {
	args := map[string]any{"cmd": "cat report.txt", "cwd": "/work/docs"}
	original := map[string]any{"cmd": "cat report.txt", "cwd": "/work/docs"}
	hash, _ := Hash("workerHost", "exec", args)
	for range 2 {
		cmd, cwd, timeout, err := CommandInput(args)
		if err != nil || cmd != args["cmd"] || cwd != args["cwd"] || timeout != 60 || !reflect.DeepEqual(args, original) {
			t.Fatalf("approval or retry mutated original input: %v, timeout %d, error %v", args, timeout, err)
		}
		retryHash, _ := Hash("workerHost", "exec", args)
		if retryHash != hash {
			t.Fatal("retry changed original argument hash")
		}
	}
	for _, input := range []map[string]any{
		{"cmd": "echo ok", "cwd": "/work/docs", "env": map[string]any{"TOKEN": "private"}},
		{"cmd": "echo ok", "cwd": "/work/docs", "stdin": "private"},
		{"cmd": "echo ok", "cwd": "/work/docs", "shell": "other"},
		{"cmd": "echo ok", "cwd": "/work/docs", "timeoutSec": 0},
		{"cmd": "echo ok", "cwd": "/work/docs", "timeoutSec": 601},
		{"cmd": "echo ok", "cwd": "/work/docs", "timeoutSec": 1.5},
		{"cmd": "echo ok", "cwd": "/work/docs", "timeoutSec": "60"},
	} {
		if _, _, _, err := CommandInput(input); err == nil {
			t.Fatal("unreviewed or invalid invocation accepted")
		}
	}
}

func TestComputerPermissionExactInputAndDirectoryBoundary(t *testing.T) {
	args := map[string]any{"cmd": "cat report.txt", "cwd": "/work/docs"}
	hash, _ := Hash("workerHost", "exec", args)
	s := Scope{Version: 1, Kind: "exact_command", RegistrationID: "worker", MachineID: "machine", MachineLabel: "Laptop", Tool: "workerHost", Action: "exec", Command: "cat report.txt", CWD: "/work/docs", TimeoutSeconds: 60, ArgsHash: hash, AllowedModes: []string{"once", "always"}}
	req := Request{Tool: "workerHost", Action: "exec", Args: args}
	if !s.Matches(req, "always") {
		t.Fatal("exact command refused")
	}
	for _, key := range []string{"cmd", "cwd", "env", "stdin", "timeoutSec"} {
		copy := map[string]any{}
		for k, v := range args {
			copy[k] = v
		}
		copy[key] = "changed"
		if s.Matches(Request{Tool: req.Tool, Action: req.Action, Args: copy}, "always") {
			t.Fatalf("changed %s admitted", key)
		}
	}
	s.Kind = "directory_read"
	s.Action = "fs_read"
	s.Directory = "/work/docs"
	s.Command = ""
	s.CWD = ""
	s.TimeoutSeconds = 0
	args = map[string]any{"path": "/work/docs/report.txt"}
	s.ArgsHash, _ = Hash("workerHost", "fs_read", args)
	for _, action := range []string{"fs_read", "fs_list", "fs_stat"} {
		if !s.Matches(Request{Tool: "workerHost", Action: action, Args: args}, "always") {
			t.Fatal(action)
		}
	}
	for _, p := range []string{"/work/docs-other/file", "/work/docs/../secret", "~/docs/file", "/work/docs//file", "/work/docs\\file"} {
		if s.Matches(Request{Tool: "workerHost", Action: "fs_read", Args: map[string]any{"path": p}}, "always") {
			t.Fatal(p)
		}
	}
	if s.Matches(Request{Tool: "workerHost", Action: "fs_list", Args: args}, "once") {
		t.Fatal("once widened action")
	}
	if s.Matches(Request{Tool: "workerHost", Action: "fs_write", Args: args}, "always") {
		t.Fatal("directory widened to write")
	}
}

func TestExactCommandDefaultTimeoutHasOneApprovalMeaning(t *testing.T) {
	for _, approvedExplicit := range []bool{false, true} {
		approved := map[string]any{"cmd": "cat report.txt", "cwd": "/work/docs"}
		requested := map[string]any{"cmd": "cat report.txt", "cwd": "/work/docs"}
		if approvedExplicit {
			approved["timeoutSec"] = 60
		} else {
			requested["timeoutSec"] = 60
		}
		hash, _ := Hash("workerHost", "exec", approved)
		scope := Scope{Version: 1, Kind: "exact_command", RegistrationID: "worker", MachineID: "machine", MachineLabel: "Laptop", Tool: "workerHost", Action: "exec", Command: "cat report.txt", CWD: "/work/docs", TimeoutSeconds: 60, ArgsHash: hash, AllowedModes: []string{"once", "always"}}
		for _, mode := range []string{"once", "always"} {
			if !scope.Matches(Request{Tool: "workerHost", Action: "exec", Args: requested}, mode) {
				t.Errorf("equivalent default refused: approvedExplicit=%v mode=%s", approvedExplicit, mode)
			}
			for name, args := range map[string]map[string]any{
				"missing cwd":        {"cmd": "cat report.txt"},
				"changed cwd":        {"cmd": "cat report.txt", "cwd": "/work/elsewhere"},
				"changed command":    {"cmd": "cat other.txt", "cwd": "/work/docs"},
				"changed whitespace": {"cmd": "cat report.txt ", "cwd": "/work/docs"},
				"different timeout":  {"cmd": "cat report.txt", "cwd": "/work/docs", "timeoutSec": 59},
				"extra input":        {"cmd": "cat report.txt", "cwd": "/work/docs", "stdin": "data"},
				"unknown input":      {"cmd": "cat report.txt", "cwd": "/work/docs", "unknown": true},
				"null timeout":       {"cmd": "cat report.txt", "cwd": "/work/docs", "timeoutSec": nil},
				"zero timeout":       {"cmd": "cat report.txt", "cwd": "/work/docs", "timeoutSec": 0},
				"text timeout":       {"cmd": "cat report.txt", "cwd": "/work/docs", "timeoutSec": "60"},
				"fractional timeout": {"cmd": "cat report.txt", "cwd": "/work/docs", "timeoutSec": 60.5},
			} {
				if scope.Matches(Request{Tool: "workerHost", Action: "exec", Args: args}, mode) {
					t.Errorf("%s widened approval", name)
				}
			}
		}
		// Receipt equivalence must not change the raw hash used by the wire
		// constraint, or mutate either caller's args to supply missing values.
		requestedHash, _ := Hash("workerHost", "exec", requested)
		if hash == requestedHash {
			t.Fatal("raw wire hashes were normalized")
		}
		if _, explicit := approved["timeoutSec"]; explicit != approvedExplicit {
			t.Fatal("approved arguments mutated")
		}
		if _, explicit := requested["timeoutSec"]; explicit == approvedExplicit {
			t.Fatal("requested arguments mutated")
		}
		changed := scope
		changed.ArgsHash, _ = Hash("workerHost", "exec", map[string]any{"cmd": "cat report.txt", "cwd": "/work/docs", "env": map[string]any{}})
		if changed.Matches(Request{Tool: "workerHost", Action: "exec", Args: requested}, "once") {
			t.Fatal("unreviewed stored inputs accepted")
		}
	}
}
