// Tests for WHICH image fields the drift detector reads
// (scripts/deploy/drift-check.sh, epic memql#3842 / memql#3847).
//
// The --rendered gate asserts every image an overlay pins is pinned by
// @sha256: digest. That is only as good as the set of fields it looks at, and
// the set was one field: `image:`, which is what core workload kinds use.
//
// A CustomResource names its image wherever it likes. CloudNativePG's Cluster
// uses `imageName:`, so the DATABASE image -- the most sensitive image in the
// deployment, and the only stateful one -- was outside the gate while every
// stateless node was held to a digest. That omission could not have shown up as
// a failure: it shows up as the check passing over an overlay it had not read.
//
// These cases pin the field set by running the SCRIPT'S OWN awk program against
// a fixture, so the test cannot drift from the implementation the way a
// re-typed copy of the program would.
package deploy

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// awkImageProgram extracts the image-extraction awk program from
// drift-check.sh. Read out of the file rather than duplicated here: a copy
// would keep passing after the script changed, which is the failure this whole
// file is about.
func awkImageProgram(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(aksScript(t, "drift-check.sh"))
	if err != nil {
		t.Fatalf("reading drift-check.sh: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "| awk '") {
			continue
		}
		start := strings.Index(trimmed, "'")
		end := strings.LastIndex(trimmed, "'")
		if start < 0 || end <= start {
			continue
		}
		return trimmed[start+1 : end]
	}
	t.Fatal("could not find the image-extraction awk program in drift-check.sh; " +
		"this guard is no longer reading what it claims to")
	return ""
}

// runAwk feeds input through the given awk program.
func runAwk(t *testing.T, program, input string) []string {
	t.Helper()
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("awk not available")
	}
	cmd := exec.Command("awk", program)
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("awk failed: %v\n%s", err, out)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// TestDriftCheckReadsBothImageFields is the regression test: the extractor must
// see a CNPG Cluster's `imageName:` as well as a container's `image:`.
func TestDriftCheckReadsBothImageFields(t *testing.T) {
	program := awkImageProgram(t)

	// A rendered overlay in miniature: a Deployment container, a CNPG Cluster,
	// and lines that must NOT match.
	const rendered = `
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: bff
          image: acrmemql.azurecr.io/memql-bff@sha256:aaaa
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
spec:
  instances: 2
  imageName: acrmemql.azurecr.io/memql-db@sha256:bbbb
  imagePullPolicy: IfNotPresent
---
apiVersion: v1
kind: ConfigMap
data:
  # These must not be mistaken for image declarations.
  imagePullSecrets: "not-an-image"
  note: "image: this is prose, not a field"
`

	got := runAwk(t, program, rendered)

	for _, want := range []string{
		"acrmemql.azurecr.io/memql-bff@sha256:aaaa",
		"acrmemql.azurecr.io/memql-db@sha256:bbbb",
	} {
		var found bool
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("the extractor did not emit %q.\nGot: %v\n"+
				"If this is the CNPG `imageName:` case: the database image would then sit outside the "+
				"digest-pinning gate entirely, and the gate would report success having never read it.",
				want, got)
		}
	}

	// `imagePullPolicy:` starts with "image" too. It must not be collected --
	// a value like IfNotPresent would then be compared against cluster digests
	// as though it were an image reference.
	for _, g := range got {
		if strings.Contains(g, "IfNotPresent") || strings.Contains(g, "not-an-image") {
			t.Errorf("the extractor collected a non-image field (%q); `imagePullPolicy:` and prose "+
				"must not be mistaken for an image declaration", g)
		}
	}
}
