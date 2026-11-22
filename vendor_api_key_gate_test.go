package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestNoVendorApiKeyEntryPoint: the CLUSTER has no vendor API key, and none of
// the ways one used to be entered comes back (epic memql#5088, design D5; design
// record docs/superpowers/specs/2026-11-21-vendor-accounts-and-api-keys-design.md,
// decisions O2 and K19).
//
// The cluster reaches both cloud vendors by workload identity federation, as
// itself. It holds no key of its own, and the work it runs as itself never
// spends one. The names in `banned` below are how a key for the whole cluster
// used to be entered: four environment variables and one builtin. Every one of
// them was individually easy to re-add and none would have failed anything.
// This gate is what keeps that list at zero.
//
// WHAT IS NOT BANNED, and why the list is names rather than concepts:
//
//   - A PERSON'S OWN API KEY. A person adds a key of their own in MemQL OS,
//     under Settings > API keys. It is checked with the vendor, sealed as that
//     person's own row (v1:router:vendorKey), and pays only for calls made by
//     that person or by somebody they lent it to. It is entered through none
//     of the names below. That is the design, not a way around this gate: the
//     cluster still has no key, and its own work still spends none
//     (integrations/vendorkeys).
//   - `vendor_api_key` as a globalSecret KIND is legitimate and still in use.
//     The Shopify connector seals three store credentials under it. Those are
//     credentials for a vendor MemQL does not federate with, entered by the
//     person they belong to. Banning the kind would have deleted a working
//     feature to enforce a rule about a different one.
//   - `MEMQL_AI_OPENAI_PROJECT_ID` is not a key. It names a project.
//   - `MEMQL_TEST_OPENAI_BEARER` is a test input read by exactly one live,
//     env-gated test and by nothing in the product; it is how an engineer
//     answers the open question in the OpenAI runbook about whether the
//     Realtime WebSocket accepts a federated bearer.
//
// The walk is `git ls-files`, which is this repo's convention for a gate:
// untracked files are invisible to it, so a new file must be `git add`ed
// before this test can see it.
func TestNoVendorApiKeyEntryPoint(t *testing.T) {
	banned := []string{
		"MEMQL_AI_ANTHROPIC_API_KEY",
		"MEMQL_AI_OPENAI_API_KEY",
		"MEMQL_ANTHROPIC_API_KEY",
		"MEMQL_OPENAI_API_KEY",
		"providerKeySet",
	}

	// The allow-list is DESIGN RECORDS AND THIS FILE, and nothing else.
	//
	// A design record is a dated account of a decision. The three named here
	// argue about these exact names -- two of them made the key the design and
	// the third removed it -- and rewriting history to satisfy a gate would
	// destroy the only explanation of why the gate exists. Everything else in
	// the tree describes the product as it IS, and the product has no key.
	allowed := map[string]bool{
		"docs/superpowers/specs/2026-09-06-openai-federation-and-key-removal-design.md":      true,
		"docs/superpowers/specs/2026-08-22-anthropic-workload-identity-federation-design.md": true,
		"docs/superpowers/specs/2026-08-23-zero-key-install-design.md":                       true,
		"docs/superpowers/specs/2026-08-08-local-cluster-install-wizard-design.md":           true,
		"docs/superpowers/specs/2026-09-01-integration-config-design.md":                     true,
		"docs/superpowers/specs/2026-09-06-configuration-readiness-design.md":                true,
		"docs/superpowers/specs/2026-09-06-portal-removal-design.md":                         true,
		"docs/internal/design/dsl-syntax-audit-964.md":                                       true,
		"docs/internal/ops/codeql-alert-triage.md":                                           true,
		"vendor_api_key_gate_test.go":                                                        true,

		// A NEGATIVE CONTROL, asserting the same absence from the other side.
		// editors/vscode/test/inferenceFreeInstall.test.ts reads the install
		// graph and fails if any of these names appears in it. It has to spell
		// them out to look for them, and it runs in the vscode-extension lane
		// where this Go gate does not, so the two are complementary rather
		// than redundant.
		"editors/vscode/test/inferenceFreeInstall.test.ts": true,
	}

	out, err := exec.Command("git", "ls-files").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	type hit struct {
		path string
		line int
		name string
		text string
	}
	var hits []hit

	for _, path := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path = strings.TrimSpace(path)
		if path == "" || allowed[path] {
			continue
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".go", ".memql", ".ts", ".tsx", ".js", ".yaml", ".yml", ".json", ".md", ".sh", ".tmpl", ".env":
		default:
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			// A path git knows about that cannot be read is a broken checkout,
			// not a passing gate.
			t.Fatalf("read %s: %v", path, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, name := range banned {
				if strings.Contains(line, name) {
					hits = append(hits, hit{path: path, line: i + 1, name: name, text: strings.TrimSpace(line)})
				}
			}
		}
	}

	if len(hits) == 0 {
		return
	}

	sort.Slice(hits, func(i, j int) bool {
		if hits[i].path != hits[j].path {
			return hits[i].path < hits[j].path
		}
		return hits[i].line < hits[j].line
	})

	var b strings.Builder
	b.WriteString("a vendor API key for the whole cluster has a way back into the product.\n\n")
	b.WriteString("The cluster reaches every cloud vendor it calls by workload identity federation " +
		"(docs/public/operate/auth/{anthropic,openai}-federation.md) and holds no key of its own. " +
		"These names are how a key for the whole cluster used to be entered, and this gate keeps " +
		"them out.\n\n" +
		"A person's own API key is a different thing and is not what this gate is about: it is " +
		"added in MemQL OS under Settings > API keys, sealed as that person's row, and entered " +
		"through none of these names (docs/public/operate/api-keys.md).\n\n")
	for _, h := range hits {
		text := h.text
		if len(text) > 120 {
			text = text[:120] + "..."
		}
		b.WriteString("  " + h.path + ":" + itoa(h.line) + "  " + h.name + "\n      " + text + "\n")
	}
	b.WriteString("\nIf one of these is a DESIGN RECORD explaining the removal, add its path to " +
		"the allow-list above rather than editing the record.\n")
	t.Fatal(b.String())
}
