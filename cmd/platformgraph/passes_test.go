package main

// passes_test.go -- the passes' pure halves, on fixtures. The whole generator
// over the real tree is exercised by the root package's
// TestPlatformGraphIsNotStale, which regenerates through `make platform-graph`.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/architecture/model"
	"github.com/znasllc-io/memql/component/node"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A kustomization key the pass does not render -- a patch -- is refused, not
// read around: reading the resources directly is only the render while the
// kustomization adds nothing but a namespace and labels.
func TestReadKustomizationRefusesWhatItDoesNotRender(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "k", "kustomization.yaml"), "resources:\n  - a.yaml\nnamespace: x\n")
	if _, err := readKustomization(root, "k"); err != nil {
		t.Fatalf("a plain resource list was refused: %v", err)
	}
	writeFile(t, filepath.Join(root, "p", "kustomization.yaml"), "resources:\n  - a.yaml\npatches:\n  - path: p.yaml\n")
	if _, err := readKustomization(root, "p"); err == nil {
		t.Fatal("a kustomization with patches was read as if it rendered to its resources")
	}
}

func TestReadResourcesDecodesEveryDocumentOrFails(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "k", "kustomization.yaml"), "resources:\n  - a.yaml\n")
	writeFile(t, filepath.Join(root, "k", "a.yaml"), `apiVersion: apps/v1
kind: Deployment
metadata: {name: agent}
spec:
  selector: {matchLabels: {app: agent}}
  template:
    metadata: {labels: {app: agent}}
    spec:
      containers:
        - name: agent
          ports: [{name: grpc, containerPort: 50051}]
          env:
            - {name: MEMQL_NODE_TYPE, value: agent}
            - {name: MEMQL_PARENT_ADDRESS, value: "bff-active:50058"}
          envFrom:
            - configMapRef: {name: memql-db-pool}
---
apiVersion: v1
kind: Service
metadata: {name: agent}
spec:
  selector: {app: agent}
  ports: [{name: grpc, port: 50051}]
`)
	docs, err := readResources(root, "k")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].Kind != "Deployment" || docs[1].Kind != "Service" {
		t.Fatalf("docs = %+v", docs)
	}
	sel, err := selectorLabels("Service", docs[1].Spec.Selector)
	if err != nil || !labelsMatch(sel, docs[0].Spec.Template.Metadata.Labels) {
		t.Fatalf("the Service's selector %v does not select the Deployment (%v)", sel, err)
	}

	writeFile(t, filepath.Join(root, "k", "a.yaml"), "kind: Deployment\nmetadata: {name: [unclosed\n")
	if _, err := readResources(root, "k"); err == nil {
		t.Fatal("a malformed document was skipped; the pass would record a truncated deployment")
	}
}

func TestAddressRe(t *testing.T) {
	for value, want := range map[string]string{
		"bff-active:50058":               "bff-active:50058",
		"https://identity:8085":          "identity:8085",
		"workbench=workbench:50060":      "workbench:50060",
		"0.0.0.0:8085":                   "",
		"$(POD_IP):50055":                "",
		"https://identity.<domain>":      "",
		":50051":                         "",
		"agent=agent:50051,x=planner:50": "agent:50051,planner:50",
	} {
		var got []string
		for _, m := range addressRe.FindAllStringSubmatch(value, -1) {
			got = append(got, m[1]+":"+m[3])
		}
		if strings.Join(got, ",") != want {
			t.Errorf("%q -> %v, want %q", value, got, want)
		}
	}
}

func TestFrontDoorHostsAreWrittenAgainstThePlaceholder(t *testing.T) {
	routes, err := readFrontDoor([]byte(`kind: Certificate
---
kind: Ingress
spec:
  rules:
    - host: api.acme.test
      http:
        paths:
          - {path: /, pathType: Prefix, backend: {service: {name: bff, port: {number: 50051}}}}
    - host: "*.acme.test"
      http:
        paths:
          - {path: /, pathType: Prefix, backend: {service: {name: edge, port: {number: 8085}}}}
    - host: acme.test
      http:
        paths:
          - {path: /, pathType: Prefix, backend: {service: {name: edge, port: {number: 8085}}}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) != 3 {
		t.Fatalf("routes = %+v", routes)
	}
	domain, err := domainOf(routes)
	if err != nil || domain != "acme.test" {
		t.Fatalf("domain = %q, %v", domain, err)
	}
	for host, want := range map[string]string{
		"api.acme.test": "api.<domain>",
		"*.acme.test":   "*.<domain>",
		"acme.test":     "<domain>",
	} {
		if got := placeholderHost(host, domain); got != want {
			t.Errorf("placeholderHost(%q) = %q, want %q", host, got, want)
		}
	}
	if _, err := domainOf([]frontDoorRoute{{host: "a.one.test"}, {host: "b.two.test"}}); err == nil {
		t.Error("two unrelated hosts produced a domain")
	}
}

// The serves derivation, on a fixture tree: a registering method's
// constructor called from a file of one role and not the other's.
func TestServesFollowsTheConstructorIntoTheRolesFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "worker", "worker.go"), `package worker

type Service struct{}

func NewService() (*Service, error) { return &Service{}, nil }

func (s *Service) Register(r registrar) { gen.RegisterWorkerServiceServer(r, s) }
`)
	writeFile(t, filepath.Join(dir, "app", "agent.go"), `package app

import "example.test/m/worker"

func wire() { svc, _ := worker.NewService(); _ = svc }
`)
	writeFile(t, filepath.Join(dir, "app", "bff.go"), `package app

import "example.test/m/worker"

func other(r *worker.Registry) {}
`)
	files := newFileSet()
	workerPkg := listedPackage{ImportPath: "example.test/m/worker", Dir: filepath.Join(dir, "worker"), GoFiles: []string{"worker.go"}}
	regs, err := findRegistrants(files, []listedPackage{workerPkg}, map[string]string{"WorkerService": "acme.v1.WorkerService"})
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 1 || regs[0].Recv != "Service" || strings.Join(regs[0].Constructors, ",") != "NewService" {
		t.Fatalf("registrants = %+v", regs)
	}
	for file, want := range map[string]bool{"agent.go": true, "bff.go": false} {
		f, imports, err := files.parse(filepath.Join(dir, "app", file))
		if err != nil {
			t.Fatal(err)
		}
		_, got := callsConstructor(f, imports, "example.test/m/app", regs[0])
		if got != want {
			t.Errorf("%s constructs the worker service = %t, want %t (a reference to another type of the "+
				"package is not construction)", file, got, want)
		}
	}
}

func TestBuilderRefusesAConflictAndADanglingEdge(t *testing.T) {
	b := newBuilder()
	b.node("a:1", model.PlatformRole, "one", nil)
	b.node("a:1", model.PlatformRole, "one", nil)
	if _, err := b.graph(); err != nil {
		t.Fatalf("an identical re-add was refused: %v", err)
	}
	b.node("a:1", model.PlatformRole, "different", nil)
	if _, err := b.graph(); err == nil {
		t.Fatal("two passes disagreeing about one node were merged")
	}

	b = newBuilder()
	b.node("a:1", model.PlatformRole, "one", nil)
	b.edge("a:1", "a:2", model.PlatformRunsOn, nil)
	if _, err := b.graph(); err == nil {
		t.Fatal("an edge to a node no pass added was accepted")
	}
}

// roleListFixture writes the three role lists the generator checks, agreeing
// with component/node/roles.go, into a fresh root.
func roleListFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	var names []string
	var arch strings.Builder
	arch.WriteString("service: acme\nroles:\n")
	for _, r := range node.Roles() {
		names = append(names, string(r.Type))
		writeFile(t, filepath.Join(root, "app", "build_"+string(r.Type)+".go"), "package app\n")
		fmt.Fprintf(&arch, "  - name: %s\n    mesh: %t\n    description: %q\n", r.Type, r.Mesh, r.Description)
	}
	writeFile(t, filepath.Join(root, "app", "build_default.go"), "package app\n")
	writeFile(t, filepath.Join(root, "scripts", "lib", "engine_build_args.sh"), "ENGINE_NODE_TYPES=("+strings.Join(names, " ")+")\n")
	writeFile(t, filepath.Join(root, "arch.yaml"), arch.String())
	return root
}

func TestVerifyRoleListsRefusesADisagreeingList(t *testing.T) {
	root := roleListFixture(t)
	if err := verifyRoleLists(root); err != nil {
		t.Fatalf("agreeing lists refused: %v", err)
	}

	extra := roleListFixture(t)
	writeFile(t, filepath.Join(extra, "app", "build_voice.go"), "package app\n")
	if err := verifyRoleLists(extra); err == nil {
		t.Error("a build file for a role the table does not have was accepted")
	}

	reordered := roleListFixture(t)
	names := []string{}
	for _, r := range node.Roles() {
		names = append([]string{string(r.Type)}, names...)
	}
	writeFile(t, filepath.Join(reordered, "scripts", "lib", "engine_build_args.sh"), "ENGINE_NODE_TYPES=("+strings.Join(names, " ")+")\n")
	if err := verifyRoleLists(reordered); err == nil {
		t.Error("ENGINE_NODE_TYPES in another order was accepted")
	}

	drifted := roleListFixture(t)
	raw, _ := os.ReadFile(filepath.Join(drifted, "arch.yaml"))
	writeFile(t, filepath.Join(drifted, "arch.yaml"), strings.Replace(string(raw), "mesh: true", "mesh: false", 1))
	if err := verifyRoleLists(drifted); err == nil {
		t.Error("an arch.yaml role whose mesh column disagrees was accepted")
	}
}

// deployFixture writes a deploy tree the deploy pass accepts: one Deployment
// and Service per role (the bff in the engine-bff component, the rest in the
// base, as in the real tree), plus whatever extra the caller adds to the base.
func deployFixture(t *testing.T, extra string) string {
	t.Helper()
	root := t.TempDir()
	workload := func(name string) string {
		return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata: {name: %[1]s}
spec:
  selector: {matchLabels: {app: %[1]s}}
  template:
    metadata: {labels: {app: %[1]s}}
    spec:
      containers:
        - name: %[1]s
          ports: [{name: http, containerPort: 8085}]
          env:
            - {name: MEMQL_NODE_TYPE, value: %[1]s}
---
apiVersion: v1
kind: Service
metadata: {name: %[1]s}
spec:
  selector: {app: %[1]s}
  ports: [{name: http, port: 8085}]
`, name)
	}
	var base strings.Builder
	for _, r := range node.Roles() {
		if r.Type == node.NodeTypeBFF {
			writeFile(t, filepath.Join(root, deployEngineBFF, "kustomization.yaml"), "resources:\n  - bff.yaml\n")
			writeFile(t, filepath.Join(root, deployEngineBFF, "bff.yaml"), workload("bff"))
			continue
		}
		base.WriteString(workload(string(r.Type)))
		base.WriteString("---\n")
	}
	base.WriteString(extra)
	writeFile(t, filepath.Join(root, deployBase, "kustomization.yaml"), "resources:\n  - all.yaml\n")
	writeFile(t, filepath.Join(root, deployBase, "all.yaml"), base.String())
	return root
}

// A Deployment that runs a SUBCOMMAND of the engine binary rather than a node
// role -- the error pages are `memql error-pages` on the edge image -- is
// recorded as a Deployment with its Service selecting it, and as running no
// role: there is no runs_on edge to invent for something that never joins the
// mesh, and putting MEMQL_NODE_TYPE=edge on it to satisfy the pass would have
// the graph claim a second edge. One that states neither is still refused.
func TestDeployPassRecordsADeploymentThatRunsASubcommand(t *testing.T) {
	root := deployFixture(t, `---
apiVersion: apps/v1
kind: Deployment
metadata: {name: error-pages}
spec:
  selector: {matchLabels: {app: error-pages}}
  template:
    metadata: {labels: {app: error-pages}}
    spec:
      containers:
        - name: error-pages
          command: ["/app/memql", "error-pages"]
          ports: [{name: http, containerPort: 8080}, {name: health, containerPort: 8081}]
---
apiVersion: v1
kind: Service
metadata: {name: error-pages}
spec:
  selector: {app: error-pages}
  ports: [{name: http, port: 80}]
`)
	b := newBuilder()
	for _, r := range node.Roles() {
		b.node(model.ServiceID(string(r.Type)), model.PlatformRole, string(r.Type), nil)
	}
	if err := deployPass(b, root); err != nil {
		t.Fatalf("a Deployment that runs a subcommand was refused: %v", err)
	}
	g, err := b.graph()
	if err != nil {
		t.Fatal(err)
	}

	dep, ok := b.nodes[model.DeploymentID("error-pages")]
	if !ok {
		t.Fatal("the subcommand Deployment is not in the graph")
	}
	if dep.Attrs["subcommand"] != "error-pages" || dep.Attrs["nodeType"] != "" || dep.Attrs["ports"] != "health:8081,http:8080" {
		t.Errorf("the error-pages Deployment node carries %v, want subcommand=error-pages, no nodeType, ports health:8081,http:8080", dep.Attrs)
	}
	if _, ok := b.nodes[model.K8sServiceID("error-pages")]; !ok {
		t.Error("the error-pages Service is not in the graph")
	}

	var selected bool
	for _, e := range g.Edges {
		if e.To == model.DeploymentID("error-pages") && e.Kind == model.PlatformRunsOn {
			t.Errorf("%s -runs_on-> error-pages: a subcommand is not a node role", e.From)
		}
		if e.From == model.K8sServiceID("error-pages") && e.To == model.DeploymentID("error-pages") && e.Kind == model.PlatformSelects {
			selected = true
		}
	}
	if !selected {
		t.Error("the error-pages Service does not select its Deployment")
	}
	// Every real role still runs on its own Deployment: the new shape did not
	// loosen the rule for the old one.
	for _, r := range node.Roles() {
		var runs bool
		for _, e := range g.Edges {
			if e.From == model.ServiceID(string(r.Type)) && e.To == model.DeploymentID(string(r.Type)) && e.Kind == model.PlatformRunsOn {
				runs = true
			}
		}
		if !runs {
			t.Errorf("role %s lost its runs_on edge", r.Type)
		}
	}

	// Neither a role nor a subcommand: still a defect worth failing on.
	mystery := deployFixture(t, `---
apiVersion: apps/v1
kind: Deployment
metadata: {name: mystery}
spec:
  selector: {matchLabels: {app: mystery}}
  template:
    metadata: {labels: {app: mystery}}
    spec:
      containers:
        - name: mystery
          ports: [{name: http, containerPort: 9090}]
`)
	mb := newBuilder()
	for _, r := range node.Roles() {
		mb.node(model.ServiceID(string(r.Type)), model.PlatformRole, string(r.Type), nil)
	}
	if err := deployPass(mb, mystery); err == nil {
		t.Fatal("a Deployment that names neither a role nor a subcommand was accepted")
	}
}

// A Deployment that sets a valid MEMQL_NODE_TYPE AND runs `memql <subcommand>`
// runs the subcommand: the command decides, and the role the env names is never
// started. Recorded, it would carry a runs_on edge from that role -- the graph
// saying the role runs where it does not -- so the pass refuses it, and the
// refusal names both halves.
func TestDeployPassRefusesADeploymentThatSetsARoleAndRunsASubcommand(t *testing.T) {
	root := deployFixture(t, `---
apiVersion: apps/v1
kind: Deployment
metadata: {name: error-pages}
spec:
  selector: {matchLabels: {app: error-pages}}
  template:
    metadata: {labels: {app: error-pages}}
    spec:
      containers:
        - name: error-pages
          command: ["/app/memql", "error-pages"]
          ports: [{name: http, containerPort: 8080}]
          env:
            - {name: MEMQL_NODE_TYPE, value: edge}
`)
	b := newBuilder()
	for _, r := range node.Roles() {
		b.node(model.ServiceID(string(r.Type)), model.PlatformRole, string(r.Type), nil)
	}
	err := deployPass(b, root)
	if err == nil {
		t.Fatal("a Deployment that sets MEMQL_NODE_TYPE=edge and runs `memql error-pages` was accepted; it would be recorded as running the edge role")
	}
	for _, want := range []string{"error-pages", "MEMQL_NODE_TYPE", "edge"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
}
