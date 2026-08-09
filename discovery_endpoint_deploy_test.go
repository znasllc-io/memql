package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The deploy half of the discovery-endpoint contract (memql#3399).
//
// The Go handler (component/identity/discovery.go) guarantees the FORM of the
// advertised dial address; the client parser
// (editors/vscode/src/connection/endpoint.ts) guarantees what a client accepts;
// those two are pinned to each other by test/fixtures/discovery-endpoint-contract.json.
//
// Neither says anything about the HOST, and the host is where memql#3399
// actually lived: the local cluster advertised bff.local.znas.io, a name with no
// ingress anywhere in the overlay. Which host is right is a deployment fact, so
// it is asserted here, against the manifests -- the shape (the env var exists at
// all) in base, the value per overlay, per the parity rule in CLAUDE.md.
//
// This is a text scan, not a `kustomize build`: the gate must run in a plain
// `go test` with no kustomize binary on PATH, and every occurrence of the env
// var is a value some environment will publish regardless of which file it is
// written in.

const discoveryEndpointEnvVar = "MEMQL_DISCOVERY_GRPC_ENDPOINT"

// dialAddressRE is the form the discovery document publishes and the form every
// client parser accepts: a bare host with an explicit port. No scheme, no path.
var dialAddressRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.\-]*:[0-9]+$`)

// frontDoorPrefix names the ONE host in a memQL deployment whose ingress carries
// gRPC to the bff. identity.<domain> serves HTTP only; bff.<domain> is not a
// host any overlay creates. Pinning the prefix is what makes this test catch the
// original defect rather than merely its spelling.
const frontDoorPrefix = "cockpit."

// inlineValueRE reads the flow spelling base uses: `{ name: X, value: "Y" }`.
var inlineValueRE = regexp.MustCompile(
	`name:\s*` + discoveryEndpointEnvVar + `\s*,\s*value:\s*"?([^",}\s]+)"?`)

// blockValueRE reads the value line of the block spelling the overlay patches
// use, where `name:` and `value:` are separate lines.
var blockValueRE = regexp.MustCompile(`^\s*value:\s*"?([^"\n]*?)"?\s*$`)

// discoveryEndpointValues returns every value the given manifest assigns to the
// env var, in either k8s spelling.
//
// A line scan rather than one regex: the first version of this test used a
// single multi-line pattern, and it silently matched only the flow form -- so it
// scanned the base manifest and skipped the overlay patch entirely, which is
// where the wrong value actually lived. A check that cannot see the file the bug
// was in is worse than no check, so the two spellings are read separately and
// the caller asserts it found something.
func discoveryEndpointValues(manifest string) []string {
	var out []string
	lines := strings.Split(manifest, "\n")
	for i, line := range lines {
		if !strings.Contains(line, discoveryEndpointEnvVar) {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if m := inlineValueRE.FindStringSubmatch(line); m != nil {
			out = append(out, strings.TrimSpace(m[1]))
			continue
		}
		// Block form: the value is on a following line.
		for j := i + 1; j < len(lines) && j <= i+2; j++ {
			if m := blockValueRE.FindStringSubmatch(lines[j]); m != nil {
				out = append(out, strings.TrimSpace(m[1]))
				break
			}
		}
	}
	return out
}

func TestDiscoveryEndpointIsDeclaredInBase(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("deploy", "k8s", "base", "identity.yaml"))
	if err != nil {
		t.Fatalf("read base identity manifest: %v", err)
	}
	if !strings.Contains(string(raw), discoveryEndpointEnvVar) {
		t.Fatalf("deploy/k8s/base/identity.yaml does not declare %s.\n"+
			"Without it every environment falls back to deriving the identity host as the gRPC\n"+
			"dial address, which is wrong everywhere and correct only where an operator set the\n"+
			"env var by hand -- the shape of memql#3399. The shape belongs in base; the value\n"+
			"belongs in the overlay.", discoveryEndpointEnvVar)
	}
}

func TestDiscoveryEndpointManifestValuesAreDialable(t *testing.T) {
	root := filepath.Join("deploy", "k8s")
	found := 0

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, value := range discoveryEndpointValues(string(raw)) {
			if value == "" {
				t.Errorf("%s: %s is declared with an empty value", path, discoveryEndpointEnvVar)
				continue
			}
			found++
			if !dialAddressRE.MatchString(value) {
				t.Errorf("%s: %s=%q is not a dial address.\n"+
					"The field is host:port. A URL is refused by the client parser by name\n"+
					"(editors/vscode/src/connection/endpoint.ts), which is half of memql#3399.",
					path, discoveryEndpointEnvVar, value)
				continue
			}
			if !strings.HasPrefix(value, frontDoorPrefix) {
				t.Errorf("%s: %s=%q does not name the front door (%s<domain>).\n"+
					"That is the only host whose ingress carries gRPC to the bff; identity.<domain>\n"+
					"serves HTTP only, and bff.<domain> has no ingress in any overlay -- advertising\n"+
					"it is the other half of memql#3399.",
					path, discoveryEndpointEnvVar, value, frontDoorPrefix)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if found == 0 {
		t.Fatalf("no %s value found under %s -- the scan matched nothing, so it proves nothing",
			discoveryEndpointEnvVar, root)
	}
}

// TestDiscoveryEndpointContractFixtureIsReadable keeps the two language halves
// honest about the file they share: a rename that silently orphaned one side
// would leave that side testing nothing.
func TestDiscoveryEndpointContractFixtureIsReadable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("test", "fixtures", "discovery-endpoint-contract.json"))
	if err != nil {
		t.Fatalf("read shared contract fixture: %v", err)
	}
	var contract struct {
		Cases []struct {
			Advertised string `json:"advertised"`
		} `json:"cases"`
		Rejected []string `json:"rejected"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		t.Fatalf("parse shared contract fixture: %v", err)
	}
	if len(contract.Cases) == 0 || len(contract.Rejected) == 0 {
		t.Fatal("shared contract fixture must carry both accepted cases and rejected forms")
	}
	for _, c := range contract.Cases {
		if !dialAddressRE.MatchString(c.Advertised) {
			t.Errorf("fixture advertises %q, which is not a dial address", c.Advertised)
		}
	}
}
