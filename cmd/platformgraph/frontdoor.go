package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/znasllc-io/memql/component/architecture/model"
	"github.com/znasllc-io/memql/component/frontdoor"
)

// frontDoorManifest is the generated cloud front door (cmd/frontdoorhosts +
// cmd/frontdoorpaths): every host rule and every routed path, in the shape an
// installation receives. Its domain is the overlay's placeholder, which the
// domain-derive component rewrites per install, so the pass writes hosts
// against placeholderDomain instead.
var frontDoorManifest = filepath.Join("deploy", "k8s", "overlays", "cloud", "front-door.generated.yaml")

const placeholderDomain = "<domain>"

type ingressDoc struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Rules []struct {
			Host string `yaml:"host"`
			HTTP struct {
				Paths []struct {
					Path     string `yaml:"path"`
					PathType string `yaml:"pathType"`
					Backend  struct {
						Service struct {
							Name string `yaml:"name"`
							Port struct {
								Number int `yaml:"number"`
							} `yaml:"port"`
						} `yaml:"service"`
					} `yaml:"backend"`
				} `yaml:"paths"`
			} `yaml:"http"`
		} `yaml:"rules"`
	} `yaml:"spec"`
}

type frontDoorRoute struct {
	host, path, pathType, service string
	port                          int
}

// readFrontDoor decodes every Ingress rule path in the manifest.
func readFrontDoor(raw []byte) ([]frontDoorRoute, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var out []frontDoorRoute
	for i := 1; ; i++ {
		var d ingressDoc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		if d.Kind != "Ingress" {
			continue
		}
		for _, r := range d.Spec.Rules {
			for _, p := range r.HTTP.Paths {
				out = append(out, frontDoorRoute{
					host: r.Host, path: p.Path, pathType: p.PathType,
					service: p.Backend.Service.Name, port: p.Backend.Service.Port.Number,
				})
			}
		}
	}
	return out, nil
}

// domainOf finds the manifest's domain: the apex rule's host, which every
// other host ends in (frontdoor.Hosts ends with the apex).
func domainOf(routes []frontDoorRoute) (string, error) {
	hosts := map[string]bool{}
	for _, r := range routes {
		hosts[r.host] = true
	}
	for candidate := range hosts {
		all := true
		for h := range hosts {
			if h != candidate && !strings.HasSuffix(h, "."+candidate) {
				all = false
				break
			}
		}
		if all {
			return candidate, nil
		}
	}
	return "", errors.New("no host is the apex every other front-door host sits under")
}

// placeholderHost rewrites host from the manifest's domain to the placeholder.
func placeholderHost(host, domain string) string {
	if host == domain {
		return placeholderDomain
	}
	return strings.TrimSuffix(host, domain) + placeholderDomain
}

func frontdoorPass(b *builder, root string) error {
	raw, err := os.ReadFile(filepath.Join(root, frontDoorManifest))
	if err != nil {
		return err
	}
	routes, err := readFrontDoor(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", frontDoorManifest, err)
	}
	if len(routes) == 0 {
		return fmt.Errorf("%s routes nothing", frontDoorManifest)
	}
	domain, err := domainOf(routes)
	if err != nil {
		return fmt.Errorf("%s: %w", frontDoorManifest, err)
	}

	// The host set is frontdoor.Hosts'; a manifest host outside it is a stale
	// generated file, which `make frontdoor` fixes and this pass refuses.
	declared := map[string]frontdoor.Host{}
	for _, h := range frontdoor.Hosts(placeholderDomain) {
		declared[h.Name] = h
	}
	for _, r := range routes {
		host := placeholderHost(r.host, domain)
		h, ok := declared[host]
		if !ok {
			return fmt.Errorf("%s routes host %s, which frontdoor.Hosts does not declare; run `make frontdoor`", frontDoorManifest, host)
		}
		b.node(model.HostID(host), model.PlatformHost, host, map[string]string{
			"role":     h.Role,
			"wildcard": strconv.FormatBool(h.Wildcard),
		})
		b.node(model.PathID(host, r.path), model.PlatformPath, r.path, map[string]string{"pathType": r.pathType})
		if !b.has(model.K8sServiceID(r.service)) {
			return fmt.Errorf("%s routes %s%s to Service %q, which the deploy base does not declare", frontDoorManifest, host, r.path, r.service)
		}
	}

	// Edges after every node, deduplicated: a path routed twice on one host
	// (the api host's HTTP and gRPC Ingresses both carry "/") is one path with
	// two backends.
	type edgeKey struct{ from, to model.ID }
	contains := map[edgeKey]bool{}
	routesTo := map[string]map[string]string{}
	var keys []string
	for _, r := range routes {
		host := placeholderHost(r.host, domain)
		ck := edgeKey{model.HostID(host), model.PathID(host, r.path)}
		if !contains[ck] {
			contains[ck] = true
			b.edge(ck.from, ck.to, model.PlatformContains, nil)
		}
		k := string(model.PathID(host, r.path)) + "\x00" + r.service + "\x00" + strconv.Itoa(r.port)
		if _, seen := routesTo[k]; !seen {
			routesTo[k] = map[string]string{"port": strconv.Itoa(r.port)}
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts := strings.SplitN(k, "\x00", 3)
		b.edge(model.ID(parts[0]), model.K8sServiceID(parts[1]), model.PlatformRoutesTo, routesTo[k])
	}
	return nil
}
