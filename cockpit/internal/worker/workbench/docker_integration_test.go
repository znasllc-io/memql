//go:build darwin || linux

package workbench

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDockerWorkbenchIsolationAndBoundedOperations(t *testing.T) {
	if os.Getenv("MEMQL_FLEET_DOCKER_TEST") != "1" {
		t.Skip("requires explicit local Docker qualification")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	docker := Docker{}
	if err := docker.Pull(ctx); err != nil {
		t.Fatal(err)
	}
	m := New(docker, t.TempDir())
	t.Cleanup(func() {
		if err := m.CloseHome("home"); err != nil {
			t.Error(err)
		}
	})
	run := identity("isolation")
	call := func(id Identity, action string, args map[string]any) Result {
		t.Helper()
		r, err := m.Execute(ctx, id, Request{Action: action, Args: args})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	out := call(run, "exec", map[string]any{"cmd": "id -u; test ! -e /var/run/docker.sock; test ! -e /home/znas; test ! -w /; test ! -w /usr; printf isolated > kept.txt"})
	if !out.OK {
		t.Fatal(out)
	}
	payload := out.Payload.(map[string]any)
	if payload["exitCode"] != float64(0) || strings.TrimSpace(payload["stdout"].(string)) != "65532" {
		t.Fatalf("host isolation: %+v", out)
	}
	if r := call(run, "fs_read", map[string]any{"path": "kept.txt"}); !r.OK || r.Payload.(map[string]any)["content"] != "isolated" {
		t.Fatal("workspace lost content", r)
	}
	if r := call(run, "exec", map[string]any{"cmd": "python3 -c 'open(\"binary.zip\",\"wb\").write(bytes(range(256))*5000)'"}); !r.OK {
		t.Fatal(r)
	}
	var binary []byte
	version := ""
	for offset := 0; ; {
		args := map[string]any{"path": "binary.zip", "encoding": "base64", "offset": offset, "maxBytes": 256 << 10}
		if version != "" {
			args["version"] = version
		}
		r := call(run, "fs_read", args)
		if !r.OK {
			t.Fatal(r)
		}
		p := r.Payload.(map[string]any)
		chunk, err := base64.StdEncoding.DecodeString(p["content"].(string))
		if err != nil || len(chunk) > 256<<10 {
			t.Fatal("invalid binary chunk", err)
		}
		binary = append(binary, chunk...)
		offset += len(chunk)
		version = p["version"].(string)
		if p["eof"] == true {
			break
		}
	}
	if len(binary) != 256*5000 {
		t.Fatal("truncated binary", len(binary))
	}
	for index, value := range binary {
		if value != byte(index%256) {
			t.Fatal("altered binary", index)
		}
	}
	call(run, "fs_write", map[string]any{"path": "binary.zip", "content": "changed"})
	if r := call(run, "fs_read", map[string]any{"path": "binary.zip", "encoding": "base64", "version": version}); r.OK {
		t.Fatal("export joined changed bytes")
	}
	borrower := run
	borrower.Caller = "other-person"
	if r := call(borrower, "fs_stat", map[string]any{"path": "kept.txt"}); !r.OK || r.Payload.(map[string]any)["exists"] != false {
		t.Fatal("borrower read another workspace", r)
	}
	r := call(run, "exec", map[string]any{"cmd": "python3 -c 'print(\"x\" * 2000000)'"})
	data := r.Payload.(map[string]any)
	if len(data["stdout"].(string)) != MaxOutput || data["truncated"] != true {
		t.Fatal("output was not bounded")
	}
	r = call(run, "exec", map[string]any{"cmd": "dd if=/dev/zero of=full bs=1M count=280", "timeoutSec": 20})
	data = r.Payload.(map[string]any)
	if data["exitCode"] == float64(0) {
		t.Fatal("workspace exceeded tmpfs capacity")
	}
	call(run, "exec", map[string]any{"cmd": "rm -f full"})
	r = call(run, "exec", map[string]any{"cmd": "python3 -c 'import socket; socket.create_connection((\"1.1.1.1\",443),timeout=1)'"})
	if r.Payload.(map[string]any)["exitCode"] == float64(0) {
		t.Fatal("workbench had network access")
	}
	if _, err := m.Execute(ctx, run, Request{Action: "fs_read", Args: map[string]any{"path": "../../etc/passwd"}}); err == nil {
		t.Fatal("traversal accepted")
	}
	// Inspect actual runtime policy as well as exercising the boundaries.
	key, _ := run.Key()
	m.mu.Lock()
	name := m.spaces[key].name
	m.mu.Unlock()
	raw, err := docker.command(ctx, nil, "inspect", "--format", "{{json .HostConfig}}", name)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if json.Unmarshal(raw, &config) != nil {
		t.Fatal("inspect failed")
	}
	if config["NetworkMode"] != "none" || config["ReadonlyRootfs"] != true || config["Privileged"] != false || config["Memory"] != float64(MemoryMiB*1024*1024) {
		t.Fatalf("runtime limits: %s", raw)
	}
	bound := m.spaces[key].runtime
	wrong := bound
	wrong.ID = "another-daemon"
	if _, err := docker.Execute(ctx, wrong, name, Request{Action: "exec", Args: map[string]any{"cmd": "touch should-not-exist"}}); !errors.Is(err, ErrInterrupted) {
		t.Fatal("changed daemon admitted execution", err)
	}
	if err := docker.Remove(ctx, wrong, name); !errors.Is(err, ErrInterrupted) {
		t.Fatal("changed daemon admitted cleanup", err)
	}
	if r := call(run, "fs_read", map[string]any{"path": "kept.txt"}); !r.OK {
		t.Fatal("refused cleanup removed the actual workspace")
	}
	stopped, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stop()
	if _, err := m.Execute(stopped, run, Request{Action: "exec", Args: map[string]any{"cmd": "sleep 60 & wait"}}); err == nil {
		t.Fatal("cancellation ignored")
	}
	if _, err := m.Execute(ctx, run, request()); err == nil {
		t.Fatal("uncertain command automatically replayed")
	}
	if r := call(borrower, "exec", map[string]any{"cmd": "echo still-running"}); !r.OK {
		t.Fatal("cancellation crossed borrower scope")
	}
}
