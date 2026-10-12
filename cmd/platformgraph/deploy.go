package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/znasllc-io/memql/component/architecture/model"
	"github.com/znasllc-io/memql/component/node"
)

// The deploy pass reads the engine's deployment SHAPE: deploy/k8s/base, which
// every overlay composes, plus the engine-bff component, which engine-only
// overlays add (the bff lives in a component so a product cluster can bring
// its own). Values an overlay changes -- replicas, images, domains -- are not
// the shape and are not recorded.
//
// A Deployment runs one of two things, and the pass records which. Most run a
// NODE ROLE: MEMQL_NODE_TYPE names it, and the role's runs_on edge points at the
// Deployment. One may run a SUBCOMMAND of the engine binary instead --
// `command: ["/app/memql", "<subcommand>"]`, the way the error pages run
// `memql error-pages` on the edge image -- which never joins the mesh and so has
// no role to run: it is recorded as a Deployment carrying a `subcommand`
// attribute and no runs_on edge. Giving the error pages MEMQL_NODE_TYPE=edge to
// satisfy the pass would put a second edge in the graph. A Deployment that
// names neither is still refused: nothing then says what it runs.
//
// The error pages themselves are NOT in the graph. They live in
// deploy/k8s/components/error-pages, which only the ingress-nginx overlays
// (cloud, cloud-entry) compose, and this pass reads base and engine-bff alone,
// the shape every engine overlay shares. The rule above stands for any base
// Deployment that runs a subcommand.
//
// NOT RENDERED BY kustomize, deliberately. The render gates under
// deploy/k8s/overlays shell out to kustomize or kubectl and SKIP when neither
// is installed; a generator whose output decided a drift gate cannot skip, and
// cannot depend on which renderer version is on PATH. The base and the
// component are resource lists with only a namespace and labels on top, which
// change nothing recorded here -- so reading the resources directly is the
// render, and readKustomization REFUSES any other key (a patch, an image
// override, a nested component) rather than silently reading around it.
var (
	deployBase      = filepath.Join("deploy", "k8s", "base")
	deployEngineBFF = filepath.Join("deploy", "k8s", "components", "engine-bff")
)

// dbPoolConfigMap is the ConfigMap every database-connecting node mounts
// (deploy/k8s/base/db-pool-config.yaml): a Deployment that mounts it connects
// to the database, which is the only statement the base makes about one.
const dbPoolConfigMap = "memql-db-pool"

type kustomization struct {
	APIVersion string    `yaml:"apiVersion"`
	Kind       string    `yaml:"kind"`
	Namespace  string    `yaml:"namespace"`
	Resources  []string  `yaml:"resources"`
	Labels     yaml.Node `yaml:"labels"`
}

// readKustomization reads dir's kustomization.yaml, refusing any key outside
// the recorded set: those are what would make reading the resources directly
// differ from rendering them.
func readKustomization(root, dir string) (*kustomization, error) {
	raw, err := os.ReadFile(filepath.Join(root, dir, "kustomization.yaml"))
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var k kustomization
	if err := dec.Decode(&k); err != nil {
		return nil, fmt.Errorf("%s/kustomization.yaml uses something the deploy pass does not render "+
			"(it reads resources, namespace and labels only): %w", dir, err)
	}
	return &k, nil
}

type container struct {
	Name    string   `yaml:"name"`
	Command []string `yaml:"command"`
	Ports   []struct {
		Name          string `yaml:"name"`
		ContainerPort int    `yaml:"containerPort"`
	} `yaml:"ports"`
	Env []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"env"`
	EnvFrom []struct {
		ConfigMapRef *struct {
			Name string `yaml:"name"`
		} `yaml:"configMapRef"`
	} `yaml:"envFrom"`
}

// k8sDoc is the slice of a manifest the pass reads. Selector is a node because
// it has two shapes: a Service's is a label map, a Deployment's wraps one in
// matchLabels.
type k8sDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Selector yaml.Node `yaml:"selector"`
		Ports    []struct {
			Name string `yaml:"name"`
			Port int    `yaml:"port"`
		} `yaml:"ports"`
		Template struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec struct {
				Containers []container `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// readResources decodes every document of every resource dir lists.
func readResources(root, dir string) ([]k8sDoc, error) {
	k, err := readKustomization(root, dir)
	if err != nil {
		return nil, err
	}
	var docs []k8sDoc
	for _, res := range k.Resources {
		if !strings.HasSuffix(res, ".yaml") {
			return nil, fmt.Errorf("%s lists %q, which is not a manifest file; the deploy pass reads files only", dir, res)
		}
		raw, err := os.ReadFile(filepath.Join(root, dir, res))
		if err != nil {
			return nil, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		for {
			var d k8sDoc
			err := dec.Decode(&d)
			if errors.Is(err, io.EOF) {
				break
			}
			// Fatal, not a break: stopping at a bad document would record a
			// truncated prefix of the deployment and call it the shape.
			if err != nil {
				return nil, fmt.Errorf("%s/%s: document %d: %w", dir, res, len(docs)+1, err)
			}
			if d.Kind != "" {
				docs = append(docs, d)
			}
		}
	}
	return docs, nil
}

func selectorLabels(kind string, n yaml.Node) (map[string]string, error) {
	var out map[string]string
	if n.Kind == 0 {
		return nil, nil
	}
	if kind == "Deployment" {
		var s struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		}
		if err := n.Decode(&s); err != nil {
			return nil, err
		}
		return s.MatchLabels, nil
	}
	if err := n.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// addressRe finds a `<host>:<port>` in an env value, bare or inside a URL.
var addressRe = regexp.MustCompile(`(?:^|[=,/\s])([a-z0-9]([-a-z0-9]*[a-z0-9])?):([0-9]+)`)

type deployment struct {
	name     string
	nodeType string
	// subcommand is the engine-binary subcommand a Deployment with no node role
	// runs (`memql error-pages`); empty for a Deployment that runs a role.
	subcommand string
	labels     map[string]string
	ports      []string
	dials      map[string]map[string]string // k8sService name -> {env, port}
	database   bool
}

type k8sService struct {
	name     string
	selector map[string]string
	ports    []string
}

func portList(name string, port int) string { return name + ":" + strconv.Itoa(port) }

func deployPass(b *builder, root string) error {
	var docs []k8sDoc
	for _, dir := range []string{deployBase, deployEngineBFF} {
		d, err := readResources(root, dir)
		if err != nil {
			return err
		}
		docs = append(docs, d...)
	}

	var deps []deployment
	var svcs []k8sService
	for _, d := range docs {
		switch d.Kind {
		case "Deployment":
			dep := deployment{name: d.Metadata.Name, labels: d.Spec.Template.Metadata.Labels, dials: map[string]map[string]string{}}
			for _, c := range d.Spec.Template.Spec.Containers {
				for _, p := range c.Ports {
					dep.ports = append(dep.ports, portList(p.Name, p.ContainerPort))
				}
				for _, e := range c.Env {
					if e.Name == "MEMQL_NODE_TYPE" {
						dep.nodeType = e.Value
					}
				}
				if sub, ok := engineSubcommand(c.Command); ok {
					dep.subcommand = sub
				}
				for _, ef := range c.EnvFrom {
					if ef.ConfigMapRef != nil && ef.ConfigMapRef.Name == dbPoolConfigMap {
						dep.database = true
					}
				}
			}
			deps = append(deps, dep)
		case "Service":
			sel, err := selectorLabels("Service", d.Spec.Selector)
			if err != nil {
				return fmt.Errorf("Service %s selector: %w", d.Metadata.Name, err)
			}
			s := k8sService{name: d.Metadata.Name, selector: sel}
			for _, p := range d.Spec.Ports {
				s.ports = append(s.ports, portList(p.Name, p.Port))
			}
			svcs = append(svcs, s)
		}
	}
	if len(deps) == 0 || len(svcs) == 0 {
		return fmt.Errorf("the deploy base declares %d Deployments and %d Services", len(deps), len(svcs))
	}

	// dials needs every Service name first: an env value names one.
	known := map[string]bool{}
	for _, s := range svcs {
		known[s.name] = true
	}
	for i := range deps {
		for _, doc := range docs {
			if doc.Kind != "Deployment" || doc.Metadata.Name != deps[i].name {
				continue
			}
			for _, c := range doc.Spec.Template.Spec.Containers {
				for _, e := range c.Env {
					for _, m := range addressRe.FindAllStringSubmatch(e.Value, -1) {
						host, port := m[1], m[3]
						if !known[host] {
							continue
						}
						if _, seen := deps[i].dials[host]; !seen {
							deps[i].dials[host] = map[string]string{"env": e.Name, "port": port}
						}
					}
				}
			}
		}
	}

	hasDatabase := false
	for _, dep := range deps {
		hasDatabase = hasDatabase || dep.database
	}
	if hasDatabase {
		b.node(model.DatabaseID("primary"), model.PlatformDatabase, "database", map[string]string{
			"source": "the overlay's database component; the base names it through " + dbPoolConfigMap,
		})
	}
	for _, s := range svcs {
		sort.Strings(s.ports)
		b.node(model.K8sServiceID(s.name), model.PlatformK8sService, s.name, map[string]string{"ports": strings.Join(s.ports, ",")})
	}
	for _, dep := range deps {
		sort.Strings(dep.ports)
		b.node(model.DeploymentID(dep.name), model.PlatformDeployment, dep.name, map[string]string{
			"ports":      strings.Join(dep.ports, ","),
			"nodeType":   dep.nodeType,
			"subcommand": dep.subcommand,
		})
		if dep.nodeType == "" && dep.subcommand == "" {
			return fmt.Errorf("Deployment %s sets no MEMQL_NODE_TYPE and runs no engine subcommand, so nothing says what it runs", dep.name)
		}
		if dep.nodeType != "" && dep.subcommand != "" {
			return fmt.Errorf("Deployment %s sets MEMQL_NODE_TYPE=%q and runs `memql %s`: the command runs the subcommand and the role is never started, so a runs_on edge from %s would be false; set one or the other", dep.name, dep.nodeType, dep.subcommand, dep.nodeType)
		}
		if dep.nodeType != "" {
			if _, ok := node.RoleFor(node.NodeType(dep.nodeType)); !ok {
				return fmt.Errorf("Deployment %s runs MEMQL_NODE_TYPE=%q, which is not a role in component/node/roles.go", dep.name, dep.nodeType)
			}
			b.edge(model.ServiceID(dep.nodeType), model.DeploymentID(dep.name), model.PlatformRunsOn, nil)
		}
		if dep.database {
			b.edge(model.DeploymentID(dep.name), model.DatabaseID("primary"), model.PlatformConnectsTo, map[string]string{"via": "configMapRef " + dbPoolConfigMap})
		}
		for svc, attrs := range dep.dials {
			b.edge(model.DeploymentID(dep.name), model.K8sServiceID(svc), model.PlatformDials, attrs)
		}
	}
	for _, s := range svcs {
		if len(s.selector) == 0 {
			continue
		}
		for _, dep := range deps {
			if labelsMatch(s.selector, dep.labels) {
				b.edge(model.K8sServiceID(s.name), model.DeploymentID(dep.name), model.PlatformSelects, nil)
			}
		}
	}

	// Every role must run somewhere in the base: a role with no Deployment is
	// a binary nothing deploys.
	for _, r := range node.Roles() {
		found := false
		for _, dep := range deps {
			found = found || dep.nodeType == string(r.Type)
		}
		if !found {
			return fmt.Errorf("role %s has no Deployment in %s or %s", r.Type, deployBase, deployEngineBFF)
		}
	}
	return nil
}

// engineSubcommand reports the subcommand of the engine binary a container
// runs when its command is `<path>/memql <subcommand> ...`, which is how a
// Deployment states that it runs a subcommand rather than the node role the
// image's entrypoint would start. A container with no command, or a command
// that is some other program, runs no engine subcommand. A Deployment that
// runs one and also sets MEMQL_NODE_TYPE is refused (deployPass): the command
// wins, so the role it names would be recorded as running where it does not.
func engineSubcommand(command []string) (string, bool) {
	if len(command) < 2 || filepath.Base(command[0]) != "memql" || command[1] == "" || strings.HasPrefix(command[1], "-") {
		return "", false
	}
	return command[1], true
}

func labelsMatch(selector, labels map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}
