package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostsStub writes a resolver-stub directory: one file per hostname holding the
// addresses that name resolves to. A hostname with no file does not resolve.
func hostsStub(t *testing.T, answers map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for host, addrs := range answers {
		if err := os.WriteFile(filepath.Join(dir, host), []byte(addrs), 0o600); err != nil {
			t.Fatalf("write stub %s: %v", host, err)
		}
	}
	return dir
}

func hostsRunProbe(t *testing.T, stub string, args ...string) (hostsEnvelope, int, string) {
	t.Helper()
	env := append(hostsSandbox(t, hostsNoSudo), "MEMQL_RESOLVE_STUB="+stub)
	return hostsRunEnv(t, env, args...)
}

// An operator who pointed a real wildcard A record at 127.0.0.1 has already
// done what the hosts block does. Writing it anyway costs them a sudo prompt
// for no effect, and the wizard's whole claim is that elevation appears only
// where it does something (memql#3593).
func TestHostsEntriesSkipsWhenAlreadyResolving(t *testing.T) {
	stub := hostsStub(t, map[string]string{
		"api.lab.example.com":      "127.0.0.1\n",
		"identity.lab.example.com": "127.0.0.1\n",
		"mcp.lab.example.com":      "127.0.0.1\n",
		"portal.lab.example.com":   "127.0.0.1\n",
		"lab.example.com":          "127.0.0.1\n",
	})
	hostsFile := hostsFixture(t, "127.0.0.1 localhost\n")

	env, code, out := hostsRunProbe(t, stub,
		"--action=add", "--domain=lab.example.com",
		"--hosts-file="+hostsFile, "--confirm=add-memql-hosts")

	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	if env.Result["skipped"] != true {
		t.Errorf("result.skipped = %v, want true\n%s", env.Result["skipped"], out)
	}
	if env.Result["probe"] != "satisfied" {
		t.Errorf("result.probe = %v, want satisfied", env.Result["probe"])
	}
	if env.Changed {
		t.Error("changed = true on a run that wrote nothing")
	}
	if body := hostsRead(t, hostsFile); strings.Contains(body, "BEGIN memql") {
		t.Errorf("hosts file was written despite the names already resolving:\n%s", body)
	}
}

// A hostname answering somewhere else is refused, not overwritten. Shadowing a
// record the operator may depend on is the wrong repair, and verify-frontdoor
// already states the principle: a hostname pointing at some other address is a
// worse failure than one that does not resolve.
func TestHostsEntriesRefusesConflictingResolution(t *testing.T) {
	stub := hostsStub(t, map[string]string{
		"api.lab.example.com": "203.0.113.7\n",
	})
	hostsFile := hostsFixture(t, "127.0.0.1 localhost\n")

	env, code, out := hostsRunProbe(t, stub,
		"--action=add", "--domain=lab.example.com",
		"--hosts-file="+hostsFile, "--confirm=add-memql-hosts")

	if code != 3 {
		t.Fatalf("exit %d, want 3 (refused)\n%s", code, out)
	}
	if env.Error == nil || !strings.Contains(env.Error.Message, "203.0.113.7") {
		t.Errorf("refusal does not name the offending address: %+v\n%s", env.Error, out)
	}
	if body := hostsRead(t, hostsFile); strings.Contains(body, "BEGIN memql") {
		t.Errorf("hosts file was written on a refused run:\n%s", body)
	}
}

// Partial resolution is its own refusal: writing the block would leave the
// cluster reachable at some names through DNS and others through the hosts
// file, which is two sources of truth for one front door.
func TestHostsEntriesRefusesPartialResolution(t *testing.T) {
	stub := hostsStub(t, map[string]string{
		"api.lab.example.com": "127.0.0.1\n",
	})
	hostsFile := hostsFixture(t, "127.0.0.1 localhost\n")

	_, code, out := hostsRunProbe(t, stub,
		"--action=add", "--domain=lab.example.com",
		"--hosts-file="+hostsFile, "--confirm=add-memql-hosts")

	if code != 3 {
		t.Fatalf("exit %d, want 3 (refused)\n%s", code, out)
	}
}

// Nothing resolves: the block is written, exactly as before this change.
func TestHostsEntriesWritesWhenNothingResolves(t *testing.T) {
	hostsFile := hostsFixture(t, "127.0.0.1 localhost\n")

	env, code, out := hostsRunProbe(t, t.TempDir(),
		"--action=add", "--domain=memql.localhost",
		"--hosts-file="+hostsFile, "--confirm=add-memql-hosts")

	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	if env.Result["skipped"] != false {
		t.Errorf("result.skipped = %v, want false", env.Result["skipped"])
	}
	body := hostsRead(t, hostsFile)
	for _, want := range []string{"api.memql.localhost", "identity.memql.localhost", "memql.localhost"} {
		if !strings.Contains(body, want) {
			t.Errorf("hosts file missing %q:\n%s", want, body)
		}
	}
}

// --domain and --hostnames are two spellings of one answer. Both is a
// contradiction the script must not silently resolve.
func TestHostsEntriesRefusesBothDomainAndHostnames(t *testing.T) {
	hostsFile := hostsFixture(t, "127.0.0.1 localhost\n")

	_, code, out := hostsRunProbe(t, t.TempDir(),
		"--action=add", "--domain=lab.example.com", "--hostnames=a.example.com",
		"--hosts-file="+hostsFile, "--confirm=add-memql-hosts")

	if code != 2 {
		t.Fatalf("exit %d, want 2 (bad param)\n%s", code, out)
	}
}

// Removal never probes. The block is ours and its removal is not conditional
// on what DNS currently says.
func TestHostsEntriesRemoveIgnoresResolution(t *testing.T) {
	stub := hostsStub(t, map[string]string{
		"api.memql.localhost": "203.0.113.7\n",
	})
	hostsFile := hostsFixture(t,
		"127.0.0.1 localhost\n# BEGIN memql\n127.0.0.1 api.memql.localhost\n# END memql\n")

	_, code, out := hostsRunProbe(t, stub,
		"--action=remove", "--hosts-file="+hostsFile, "--confirm=remove-memql-hosts")

	if code != 0 {
		t.Fatalf("exit %d, want 0\n%s", code, out)
	}
	if body := hostsRead(t, hostsFile); strings.Contains(body, "BEGIN memql") {
		t.Errorf("block was not removed:\n%s", body)
	}
}
