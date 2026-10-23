package hardware

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The runtime probes.
//
// Six names, fixed by the design record (D1): ollama, mlx, whispercpp,
// kokoro, mflux, docker. Each answers present-plus-version, each is
// bounded, and ABSENCE IS SILENT -- a machine without MLX is the
// ordinary case across most of a fleet, not a fault, and a log line per
// missing runtime on every tenth heartbeat is noise that trains an
// operator to stop reading the log.
const (
	RuntimeOllama     = "ollama"
	RuntimeMLX        = "mlx"
	RuntimeWhisperCPP = "whispercpp"
	RuntimeKokoro     = "kokoro"
	RuntimeMFlux      = "mflux"
	RuntimeDocker     = "docker"
)

// probeTimeout bounds one runtime probe. The same five seconds the
// models and inference packages use, for the same reason: this runs in
// front of a worker's registration, and a wedged daemon must not hold
// it there.
const probeTimeout = 5 * time.Second

// DefaultKokoroBaseURL is where a Kokoro speech runtime is expected.
// 8880 is the port the reference deployment publishes; MEMQL_KOKORO_HOST
// overrides it for anybody who moved it.
const DefaultKokoroBaseURL = "http://127.0.0.1:8880"

// baseURLFrom normalises an operator's host setting into a URL.
//
// IT ACCEPTS THE THREE SHAPES PEOPLE ACTUALLY WRITE -- a full URL, a
// bare host, and a host:port -- because all three appear in the wild
// and two of them are not URLs. Accepting only the first is not a
// stricter parser, it is a probe that reports a runtime ABSENT while it
// is serving: url.Parse reads "127.0.0.1:11434/api/version" as a scheme
// named "127.0.0.1", the request fails, and the machine says it has no
// Ollama while `memql worker models` lists five.
//
// The default port is applied to a bare host for the same reason: a
// person who wrote OLLAMA_HOST=box.local means the Ollama on box.local,
// and http://box.local is port 80.
//
// This mirrors the models package's Discoverer.ollamaBaseURL exactly.
// It is repeated rather than imported because that one is a method on a
// Discoverer this package has no reason to construct -- and the two
// MUST agree: they answer the same question, and a machine whose
// hardware inventory and model inventory disagree about where Ollama is
// reports a runtime absent while advertising its models.
func baseURLFrom(value, defaultPort, fallback string) string {
	host := strings.TrimSpace(value)
	if host == "" {
		return fallback
	}
	if !strings.Contains(host, "://") {
		if !strings.Contains(host, ":") {
			host += ":" + defaultPort
		}
		host = "http://" + host
	}
	return strings.TrimRight(host, "/")
}

// KokoroBaseURL resolves where to look for the speech runtime.
func KokoroBaseURL() string {
	return baseURLFrom(os.Getenv("MEMQL_KOKORO_HOST"), "8880", DefaultKokoroBaseURL)
}

// ollamaBaseURL resolves where to look for the model runtime.
func ollamaBaseURL() string {
	return baseURLFrom(os.Getenv("OLLAMA_HOST"), "11434", "http://127.0.0.1:11434")
}

func now() time.Time { return time.Now() }

// scanBudget is the ceiling on the WHOLE runtime scan.
//
// THIS RUNS ON THE REGISTER PATH, which is the reason it exists. The
// models package's floor is explicit about never forking a subprocess
// there -- a multi-second stall in front of a LaunchAgent's first
// connection, on every start -- and six probes at five seconds each,
// run one after another, is exactly that stall by a different route. So
// the probes run CONCURRENTLY under one deadline: the typical machine
// answers in about a tenth of a second, and the pathological one (a
// wedged Docker daemon, a firewalled Ollama) costs this budget once
// rather than probeTimeout six times.
//
// A probe that does not finish inside it reports the runtime ABSENT,
// which is the same fail-closed answer every other probe here gives to
// a question it could not settle.
const scanBudget = 6 * time.Second

// detectRuntimes reports what is installed, sorted by name so an
// unchanged machine produces a byte-identical payload -- the same
// stability rule the model labels have, and for the same reason: an
// unstable rendering rewrites a registration row on every refresh for
// no change.
func detectRuntimes(ctx context.Context) []Runtime {
	ctx, cancel := context.WithTimeout(ctx, scanBudget)
	defer cancel()

	probes := map[string]func(context.Context) (string, bool){
		RuntimeOllama:     ollamaVersion,
		RuntimeKokoro:     kokoroVersion,
		RuntimeDocker:     dockerVersion,
		RuntimeMLX:        mlxVersion,
		RuntimeWhisperCPP: whisperVersion,
		RuntimeMFlux:      mfluxVersion,
	}

	var mu sync.Mutex
	found := make(map[string]string, len(probes))

	var wg sync.WaitGroup
	for name, probe := range probes {
		wg.Add(1)
		go func(name string, probe func(context.Context) (string, bool)) {
			defer wg.Done()
			v, ok := probe(ctx)
			if !ok {
				return
			}
			mu.Lock()
			found[name] = v
			mu.Unlock()
		}(name, probe)
	}
	wg.Wait()

	out := make([]Runtime, 0, len(found))
	for name, version := range found {
		out = append(out, Runtime{Name: name, Version: version})
	}
	sortRuntimes(out)
	return out
}

// sortRuntimes orders by name. It is the single thing standing between
// map iteration order and a registration row rewritten on every
// refresh, which is why it is a named function with a test rather than
// a sort.Slice inline.
func sortRuntimes(rts []Runtime) {
	sort.Slice(rts, func(i, j int) bool { return rts[i].Name < rts[j].Name })
}

// ollamaVersion asks over HTTP, not the CLI.
//
// A Linux machine set up by this cockpit runs Ollama in a CONTAINER and
// has no `ollama` on PATH at all -- the same trap inference.Pull
// documents about `ollama pull`. `exec.LookPath("ollama")` on such a
// machine reports the runtime absent while it is serving models.
func ollamaVersion(ctx context.Context) (string, bool) {
	var body struct {
		Version string `json:"version"`
	}
	if !getJSON(ctx, ollamaBaseURL()+"/api/version", &body) {
		return "", false
	}
	return strings.TrimSpace(body.Version), true
}

// kokoroVersion probes the speech runtime the same way, and reports it
// present with an EMPTY version when the endpoint answers but names no
// version. Present-without-a-version and absent are different facts,
// and only one of them means "install this".
func kokoroVersion(ctx context.Context) (string, bool) {
	var body struct {
		Version string `json:"version"`
	}
	if !getJSON(ctx, KokoroBaseURL()+"/health", &body) {
		return "", false
	}
	return strings.TrimSpace(body.Version), true
}

// dockerVersion asks the DAEMON, not the CLI: `docker version` with a
// server template fails when nothing is listening, which is the
// distinction inference.DockerFacts already draws between "docker is
// not installed" and "the daemon did not answer". A machine where the
// user is simply not in the docker group reports no docker runtime,
// which is correct -- this process cannot use it.
func dockerVersion(ctx context.Context) (string, bool) {
	out, ok := runVersion(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if !ok {
		return "", false
	}
	return firstLine(out), true
}

func mlxVersion(ctx context.Context) (string, bool) {
	out, ok := runVersion(ctx, "python3", "-c", "import mlx.core,sys; sys.stdout.write(getattr(mlx.core,'__version__',''))")
	if !ok {
		return "", false
	}
	return firstLine(out), true
}

// whisperVersion tries whisper.cpp's two current binary names.
//
// It deliberately does NOT try `main`, which is what whisper.cpp's
// build produced before it renamed its tools. The name is far too
// generic to probe for: `main` on somebody's PATH is overwhelmingly
// likely to be their own build output, and running an arbitrary
// binary because it shares a name with a runtime we are looking for is
// not a probe, it is executing a stranger's program to see what it
// says. A machine with only the old binary reports whispercpp absent,
// which costs an inventory line and nothing else.
func whisperVersion(ctx context.Context) (string, bool) {
	for _, bin := range []string{"whisper-cli", "whisper-cpp"} {
		if out, ok := runVersion(ctx, bin, "--version"); ok {
			return parseRuntimeVersion(out), true
		}
	}
	return "", false
}

func mfluxVersion(ctx context.Context) (string, bool) {
	out, ok := runVersion(ctx, "mflux-generate", "--version")
	if !ok {
		return "", false
	}
	return parseRuntimeVersion(out), true
}

// runVersion shells out for a version string, bounded, and treats every
// failure as absence: no binary on PATH, a non-zero exit, a timeout.
// None of them establishes that the runtime is there.
func runVersion(ctx context.Context, bin string, args ...string) (string, bool) {
	if _, err := exec.LookPath(bin); err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	raw, err := exec.CommandContext(ctx, bin, args...).Output()
	if err != nil {
		return "", false
	}
	return string(raw), true
}

func getJSON(ctx context.Context, url string, out any) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false
	}
	// A body that will not decode still proves something ANSWERED at
	// that address, which is the question being asked. The version is
	// then absent, which the Runtime doc says is a reportable state.
	_ = json.NewDecoder(resp.Body).Decode(out)
	return true
}

// versionPattern finds a dotted version anywhere in a tool's greeting.
// Version output is not a format anybody promised: whisper.cpp prints a
// banner, mflux prints "mflux-generate, version 0.9.1". Pulling the
// first dotted number is what survives both without a per-tool parser
// that goes stale on the next release.
var versionPattern = regexp.MustCompile(`\d+\.\d+(\.\d+)*`)

// parseRuntimeVersion extracts the version, or returns empty -- which
// is "present, version unknown", not absent.
func parseRuntimeVersion(out string) string {
	return versionPattern.FindString(out)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}
