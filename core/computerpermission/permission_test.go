package computerpermission

import "testing"

func TestComputerPermissionExactInputAndDirectoryBoundary(t *testing.T) {
	args := map[string]any{"cmd": "cat report.txt", "cwd": "/work/docs", "env": map[string]any{"LANG": "C"}, "stdin": "input"}
	hash, _ := Hash("workerHost", "exec", args)
	s := Scope{Version: 1, Kind: "exact_command", RegistrationID: "worker", MachineID: "machine", MachineLabel: "Laptop", Tool: "workerHost", Action: "exec", Command: "cat report.txt", CWD: "/work/docs", ArgsHash: hash, AllowedModes: []string{"once", "always"}}
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
