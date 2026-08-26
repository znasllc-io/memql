// Tests for scripts/install/detect-ollama.sh (capability
// install.detectOllama, epic memql#4676 / task memql#4685).
//
// THE ASSERTION THAT MATTERS is not "it finds Ollama". It is that NOT finding
// Ollama is a SUCCESS. Install, uninstall, repair and update never require
// inference (design D8), so a probe that exited non-zero on a machine with no
// model runtime would make an inference-free install look broken to every
// caller branching on status -- and the installer branches on status.
//
// The second assertion is that the probe can actually find something. A test
// suite that only ever proves the absence case proves nothing about the
// instrument: it would pass identically against a script that always answered
// "no". So the positive case runs against a real HTTP server speaking Ollama's
// tag list.
//
// Hermetic: every case points --endpoint at an httptest server or at a port
// with nothing on it. No call leaves the machine.
package install

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type ollamaEnvelope struct {
	OK         bool   `json:"ok"`
	Capability string `json:"capability"`
	Changed    bool   `json:"changed"`
	Result     struct {
		Found      bool     `json:"found"`
		Endpoint   string   `json:"endpoint"`
		Runtime    string   `json:"runtime"`
		Models     []string `json:"models"`
		ModelCount int      `json:"modelCount"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func detectOllamaScript(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	return filepath.Join(filepath.Dir(thisFile), "detect-ollama.sh")
}

// runDetectOllama runs the probe and returns the parsed envelope plus the
// process exit code.
func runDetectOllama(t *testing.T, args ...string) (ollamaEnvelope, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{detectOllamaScript(t)}, args...)...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); ok {
			code = exitErr.ExitCode()
		} else {
			t.Fatalf("running the probe: %v (stderr: %s)", err, stderr.String())
		}
	}
	var env ollamaEnvelope
	if uerr := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &env); uerr != nil {
		t.Fatalf("stdout is not one JSON envelope: %v\nstdout: %q\nstderr: %s",
			uerr, stdout.String(), stderr.String())
	}
	return env, code
}

func asExitError(err error, out **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*out = e
		return true
	}
	return false
}

// THE D8 ASSERTION at the probe level: no runtime is a SUCCESSFUL probe.
func TestNoLocalRuntimeIsASuccessfulProbeNotAFailure(t *testing.T) {
	env, code := runDetectOllama(t, "--endpoint=http://127.0.0.1:1", "--timeout=1")
	if code != 0 {
		t.Fatalf("exit = %d, want 0. A machine with no model runtime is a perfectly good answer; "+
			"a non-zero exit here makes an inference-free install look broken to every caller "+
			"that branches on status", code)
	}
	if !env.OK {
		t.Fatalf("ok = false, want true: %+v", env.Error)
	}
	if env.Result.Found {
		t.Fatal("found must be false when nothing is listening")
	}
	if env.Result.ModelCount != 0 || len(env.Result.Models) != 0 {
		t.Fatalf("models = %+v, want empty", env.Result)
	}
	if env.Changed {
		t.Fatal("a probe must never report changed=true -- it changes nothing")
	}
}

// THE REACHABLE POSITIVE. Without this the absence case above proves nothing
// about the instrument: it would pass identically against a script that always
// answered "no".
func TestTheProbeFindsAndParsesAModelList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[
			{"name":"llama3.1:8b","size":4700000000},
			{"name":"nomic-embed-text","size":270000000},
			{"name":"qwen2.5:7b","size":4400000000}]}`))
	}))
	defer srv.Close()

	env, code := runDetectOllama(t, "--endpoint="+srv.URL, "--timeout=5")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !env.Result.Found {
		t.Fatal("found must be true when a runtime answers")
	}
	if env.Result.Runtime != "ollama" {
		t.Fatalf("runtime = %q", env.Result.Runtime)
	}
	if env.Result.ModelCount != 3 {
		t.Fatalf("modelCount = %d, want 3 -- the probe found the endpoint but did not read it",
			env.Result.ModelCount)
	}
	got := strings.Join(env.Result.Models, ",")
	if got != "llama3.1:8b,nomic-embed-text,qwen2.5:7b" {
		t.Fatalf("models = %q; the names must survive the parse INCLUDING the colon in a tag, "+
			"which is the character a naive split would eat", got)
	}
}

// A model name is a string somebody else chose. It is escaped on the way into
// the envelope, or one odd name makes the whole result unparseable.
func TestModelNamesAreEscapedIntoTheEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"weird\\model"},{"name":"ok:1b"}]}`))
	}))
	defer srv.Close()

	env, code := runDetectOllama(t, "--endpoint="+srv.URL, "--timeout=5")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(env.Result.Models) != 2 {
		t.Fatalf("models = %+v, want both to survive escaping", env.Result.Models)
	}
}

// Something ANSWERING at the endpoint with a non-Ollama 200 is a real
// condition an operator can act on ("the port is taken"), and reporting it as
// absence would hide it.
func TestANonOllamaServiceOnThePortIsReportedNotHidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"service":"something else entirely"}`))
	}))
	defer srv.Close()

	env, code := runDetectOllama(t, "--endpoint="+srv.URL, "--timeout=5")
	if code != 5 {
		t.Fatalf("exit = %d, want 5 (operation failed). Reporting a taken port as 'no runtime' "+
			"hides a condition the operator can fix", code)
	}
	if env.OK {
		t.Fatal("ok must be false")
	}
	if env.Error == nil || !strings.Contains(env.Error.Message, "did not answer with an Ollama tag list") {
		t.Fatalf("the error must say what was wrong: %+v", env.Error)
	}
}

// The capability contract: honest exit codes, and an unknown flag is a bad
// param rather than something the script quietly ignores.
func TestTheProbeKeepsTheCapabilityContract(t *testing.T) {
	if _, code := runDetectOllama(t, "--timeout=zero"); code != 2 {
		t.Fatalf("a non-numeric timeout must be exit 2 (bad param), got %d", code)
	}
	if _, code := runDetectOllama(t, "--timeout=0"); code != 2 {
		t.Fatalf("a zero timeout must be exit 2 (bad param), got %d", code)
	}
	if _, code := runDetectOllama(t, "--not-a-flag=1"); code != 2 {
		t.Fatalf("an unknown flag must be exit 2 (bad param), got %d", code)
	}
}

// --print-spec must describe the capability without probing anything.
func TestTheProbePrintsItsSpec(t *testing.T) {
	cmd := exec.Command("bash", detectOllamaScript(t), "--print-spec")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("--print-spec: %v", err)
	}
	var spec struct {
		Capability string `json:"capability"`
		Summary    string `json:"summary"`
		Params     []struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(out, &spec); err != nil {
		t.Fatalf("--print-spec is not JSON: %v (%s)", err, out)
	}
	if spec.Capability != "install.detectOllama" {
		t.Fatalf("capability = %q", spec.Capability)
	}
	names := map[string]bool{}
	for _, p := range spec.Params {
		names[p.Name] = true
	}
	for _, want := range []string{"endpoint", "timeout"} {
		if !names[want] {
			t.Fatalf("--print-spec omits the %q param", want)
		}
	}
}
