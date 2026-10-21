package k3d

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func upDomainScript(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("up.sh")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The param exists and is documented, which is what the install graph and the
// runbook both read.
func TestUpDeclaresDomainParam(t *testing.T) {
	out, err := exec.Command("bash", upDomainScript(t), "--print-spec").CombinedOutput()
	if err != nil {
		t.Fatalf("--print-spec failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "domain") {
		t.Errorf("--print-spec does not declare --domain\n%s", out)
	}
}

// upEmit sources up.sh far enough to call one of its emitters without running
// main. The script guards nothing, so it is read and truncated at `function
// main`, which is the only shape that lets these pure functions be tested at
// all without a cluster.
func upEmit(t *testing.T, domain, registry, tag, call string) string {
	t.Helper()
	body, err := os.ReadFile(upDomainScript(t))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	cut := strings.Index(src, "\nfunction main()")
	if cut < 0 {
		t.Fatal("up.sh has no `function main()` -- this harness cannot truncate it")
	}
	// REPO_ROOT and OVERLAY_PATH point at the REAL overlay: the image emitter
	// reads the node image names out of its kustomization.yaml rather than
	// carrying a list of its own, so a harness that pointed elsewhere would be
	// testing nothing.
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	harness := src[:cut] +
		"\nDOMAIN='" + domain + "'" +
		"\nIMAGE_REGISTRY='" + registry + "'" +
		"\nIMAGE_TAG='" + tag + "'" +
		"\nREPO_ROOT='" + repoRoot + "'" +
		"\nOVERLAY_PATH='deploy/k8s/overlays/local'\n" + call + "\n"

	// up.sh derives SCRIPT_DIR from its own location and sources ../lib from
	// there, so the harness needs the same directory SHAPE: a sibling of a
	// `lib` directory. Symlinked rather than copied so the harness always runs
	// against the real capability runtime.
	root := t.TempDir()
	dir := filepath.Join(root, "k3d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	realLib, err := filepath.Abs(filepath.Join("..", "lib"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realLib, filepath.Join(root, "lib")); err != nil {
		t.Fatal(err)
	}

	script := filepath.Join(dir, "harness.sh")
	if err := os.WriteFile(script, []byte(harness), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", script).CombinedOutput()
	if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, out)
	}
	return string(out)
}

// The overlay commits its own Ingress hostname, so an install that accepts the
// default needs no patch at all. Emitting one always would mean every install
// carries generated YAML restating what the manifests already say.
func TestKustomizeHostOverridesSilentOnTheDefault(t *testing.T) {
	got := upEmit(t, "memql.localhost", "", "", "kustomize_source_block")
	if strings.TrimSpace(got) != "" {
		t.Errorf("default domain emitted a kustomize block:\n%s", got)
	}
}

// A custom domain repoints both Ingresses -- rule host AND tls host, because a
// certificate that does not cover the name is the same TLS failure by another
// route.
func TestKustomizeHostOverridesRepointBothIngresses(t *testing.T) {
	got := upEmit(t, "lab.example.com", "", "", "kustomize_source_block")

	for _, want := range []string{
		"kustomize:",
		"patches:",
		"name: api-front-door",
		"name: identity-front-door",
		"value: api.lab.example.com",
		"value: identity.lab.example.com",
		"/spec/rules/0/host",
		"/spec/tls/0/hosts/0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("emitted block missing %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "value: api.lab.example.com"); n != 2 {
		t.Errorf("api host replaced %d times, want 2 (rule + tls)", n)
	}
}

// ONE `kustomize:` key. Both emitters produce sub-blocks of the same mapping,
// and a second `kustomize:` line would be a duplicate mapping key -- ArgoCD
// would silently keep one of them, dropping either the image overrides or the
// hostnames with no error anywhere.
func TestKustomizeSourceBlockHasASingleKustomizeKey(t *testing.T) {
	got := upEmit(t, "lab.example.com", "ghcr.io/example", "1.2.3", "kustomize_source_block")

	if n := strings.Count(got, "kustomize:"); n != 1 {
		t.Errorf("kustomize: appears %d times, want exactly 1:\n%s", n, got)
	}
	if !strings.Contains(got, "images:") {
		t.Errorf("image overrides missing when both are requested:\n%s", got)
	}
	if !strings.Contains(got, "patches:") {
		t.Errorf("host patches missing when both are requested:\n%s", got)
	}

	// `patches:` must start its own line. It did not: command substitution
	// strips trailing newlines, so the last image override and the patches key
	// ran together on one line and the Application's YAML lost every host
	// patch -- valid YAML, silently missing half of what was asked for.
	for _, line := range strings.Split(got, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, "patches:") && trimmed != "patches:" {
			t.Errorf("`patches:` is not on its own line: %q", line)
		}
	}
}

// Images alone still work -- this is the memql#3572 path, unchanged.
func TestKustomizeSourceBlockImagesOnly(t *testing.T) {
	got := upEmit(t, "memql.localhost", "ghcr.io/example", "1.2.3", "kustomize_source_block")

	if !strings.Contains(got, "images:") {
		t.Errorf("image overrides missing:\n%s", got)
	}
	if strings.Contains(got, "patches:") {
		t.Errorf("host patches emitted for the default domain:\n%s", got)
	}
}

// Every front door the local overlay commits at the default domain follows a
// custom --domain, and the emitter repoints nothing else. The emitter is a
// hand-kept list and it drifted (memql#6205): os, mcp, vscode and the edge
// sites front door joined the overlay and never the list, so a custom-domain
// cluster derived its issuer and CORS origins from its own domain while
// routing MemQL OS and every hosted site at memql.localhost only -- and the
// by-name check above passed throughout. The second direction matters as much:
// a replace at a path the overlay does not have fails the whole kustomize
// render, so ArgoCD would sync nothing at all.
func TestKustomizeHostOverridesCoverEveryLocalFrontDoor(t *testing.T) {
	// A nested domain too: the shape a downstream stack runs at locally, and
	// one that ends in "localhost" like the default it replaces.
	for _, domain := range []string{"lab.example.com", "memql.znas.localhost"} {
		t.Run(domain, func(t *testing.T) {
			want := localFrontDoorHosts(t, domain)
			if len(want) == 0 {
				t.Fatal("the local overlay commits no host at memql.localhost -- the gate would pass vacuously")
			}
			got := emittedHostReplacements(t, upEmit(t, domain, "", "", "kustomize_source_block"))
			for loc, value := range want {
				switch emitted, ok := got[loc]; {
				case !ok:
					t.Errorf("%s/%s %s is committed at the default domain and a --domain=%s cluster leaves it there: add it to kustomize_host_overrides", loc.kind, loc.name, loc.path, domain)
				case emitted != value:
					t.Errorf("%s/%s %s repointed to %q, want %q", loc.kind, loc.name, loc.path, emitted, value)
				}
			}
			for loc, value := range got {
				if _, ok := want[loc]; !ok {
					t.Errorf("kustomize_host_overrides replaces %s/%s %s with %q, but the overlay commits no default-domain host there -- kustomize refuses the whole render on a missing path", loc.kind, loc.name, loc.path, value)
				}
			}
		})
	}
}

// frontDoorHost is one place a hostname lives: the object and the JSON pointer
// of the field inside it, which is exactly what a JSON6902 patch addresses.
type frontDoorHost struct {
	kind, name, path string
}

// localFrontDoorHosts maps every host the local overlay commits at the default
// domain to the value a cluster served at domain must carry there instead. It
// reads the resources the overlay's kustomization LISTS -- following a
// directory resource only while it stays inside the overlay, because base and
// the components name no domain -- so a file that is never rendered cannot
// satisfy the gate, and a front door that is rendered cannot hide from it.
func localFrontDoorHosts(t *testing.T, domain string) map[frontDoorHost]string {
	t.Helper()
	const def = "memql.localhost"
	overlay, err := filepath.Abs(filepath.Join("..", "..", "deploy", "k8s", "overlays", "local"))
	if err != nil {
		t.Fatal(err)
	}
	repoint := func(host string) (string, bool) {
		if host == def || strings.HasSuffix(host, "."+def) {
			return strings.TrimSuffix(host, def) + domain, true
		}
		return "", false
	}
	want := map[frontDoorHost]string{}
	record := func(file string) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		for {
			var obj struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name string `yaml:"name"`
				} `yaml:"metadata"`
				Spec map[string]any `yaml:"spec"`
			}
			if err := dec.Decode(&obj); err == io.EOF {
				return
			} else if err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			at := func(path string) frontDoorHost { return frontDoorHost{obj.Kind, obj.Metadata.Name, path} }
			switch obj.Kind {
			case "Ingress":
				for i, rule := range yamlList(obj.Spec["rules"]) {
					if v, ok := repoint(yamlString(yamlField(rule, "host"))); ok {
						want[at(fmt.Sprintf("/spec/rules/%d/host", i))] = v
					}
				}
				for i, tls := range yamlList(obj.Spec["tls"]) {
					for j, host := range yamlList(yamlField(tls, "hosts")) {
						if v, ok := repoint(yamlString(host)); ok {
							want[at(fmt.Sprintf("/spec/tls/%d/hosts/%d", i, j))] = v
						}
					}
				}
			case "IngressRoute", "IngressRouteTCP":
				// traefik's own routes carry the host inside a match rule,
				// `HostSNI(...)` / `Host(...)`, and their `tls` is a mapping.
				for i, route := range yamlList(obj.Spec["routes"]) {
					if match := yamlString(yamlField(route, "match")); strings.Contains(match, def) {
						want[at(fmt.Sprintf("/spec/routes/%d/match", i))] = strings.ReplaceAll(match, def, domain)
					}
				}
			}
		}
	}
	var walk func(dir string)
	walk = func(dir string) {
		raw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var k struct {
			Resources []string `yaml:"resources"`
		}
		if err := yaml.Unmarshal(raw, &k); err != nil {
			t.Fatalf("%s: %v", filepath.Join(dir, "kustomization.yaml"), err)
		}
		for _, r := range k.Resources {
			p := filepath.Join(dir, r)
			if rel, err := filepath.Rel(overlay, p); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if info.IsDir() {
				walk(p)
				continue
			}
			record(p)
		}
	}
	walk(overlay)
	return want
}

// emittedHostReplacements parses what kustomize_source_block hands the
// Application into the replace each host patch makes, keyed like the
// overlay's hosts. The block is the real YAML ArgoCD receives, so a value
// kustomize would misread (an unquoted `*.domain` is a YAML ALIAS) fails here
// the way it would fail the sync.
func emittedHostReplacements(t *testing.T, block string) map[frontDoorHost]string {
	t.Helper()
	var doc struct {
		Kustomize struct {
			Patches []struct {
				Target struct {
					Kind string `yaml:"kind"`
					Name string `yaml:"name"`
				} `yaml:"target"`
				Patch string `yaml:"patch"`
			} `yaml:"patches"`
		} `yaml:"kustomize"`
	}
	if err := yaml.Unmarshal([]byte(dedent(block)), &doc); err != nil {
		t.Fatalf("the emitted kustomize block is not YAML: %v\n%s", err, block)
	}
	got := map[frontDoorHost]string{}
	for _, p := range doc.Kustomize.Patches {
		var ops []struct {
			Op    string `yaml:"op"`
			Path  string `yaml:"path"`
			Value any    `yaml:"value"`
		}
		if err := yaml.Unmarshal([]byte(p.Patch), &ops); err != nil {
			t.Fatalf("the patch for %s/%s is not a JSON6902 op list: %v\n%s", p.Target.Kind, p.Target.Name, err, p.Patch)
		}
		for _, op := range ops {
			if op.Op != "replace" {
				t.Errorf("%s/%s %s: op %q, want replace -- the overlay commits every host, so a domain only ever replaces one", p.Target.Kind, p.Target.Name, op.Path, op.Op)
				continue
			}
			value, ok := op.Value.(string)
			if !ok {
				t.Errorf("%s/%s %s: value %v is a %T, not a hostname string", p.Target.Kind, p.Target.Name, op.Path, op.Value, op.Value)
				continue
			}
			got[frontDoorHost{p.Target.Kind, p.Target.Name, op.Path}] = value
		}
	}
	return got
}

// dedent strips the indent the emitter writes at -- the block is spliced into
// the Application's spec.source -- so it parses as a document of its own.
func dedent(s string) string {
	lines := strings.Split(s, "\n")
	indent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if n := len(l) - len(strings.TrimLeft(l, " ")); indent < 0 || n < indent {
			indent = n
		}
	}
	if indent <= 0 {
		return s
	}
	for i, l := range lines {
		if len(l) >= indent {
			lines[i] = l[indent:]
		} else {
			lines[i] = strings.TrimLeft(l, " ")
		}
	}
	return strings.Join(lines, "\n")
}

func yamlList(v any) []any          { l, _ := v.([]any); return l }
func yamlString(v any) string       { s, _ := v.(string); return s }
func yamlField(v any, k string) any { m, _ := v.(map[string]any); return m[k] }
