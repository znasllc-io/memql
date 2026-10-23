package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClustersFileGet(t *testing.T) {
	file := &ClustersFile{
		Clusters: []ClusterConfig{
			{Name: "local", Endpoint: "localhost:50051"},
			{Name: "staging", Endpoint: "staging.example.com:50051"},
		},
	}

	t.Run("found", func(t *testing.T) {
		c, ok := file.Get("local")
		if !ok {
			t.Fatal("expected to find 'local'")
		}
		if c.Endpoint != "localhost:50051" {
			t.Errorf("expected localhost:50051, got %s", c.Endpoint)
		}
	})

	t.Run("not found", func(t *testing.T) {
		_, ok := file.Get("production")
		if ok {
			t.Error("expected 'production' not to be found")
		}
	})
}

// TestClusterLocalFieldRoundTrip pins the half of the shared-file
// contract the cockpit owns: ~/.memql/clusters.yaml is written by BOTH
// the cockpit and the memQL VS Code extension, and `local:` -- which
// gates the extension's non-local write confirmation -- has to survive
// a cockpit load/save cycle rather than being dropped on the floor
// (znasllc-io/memql#3313).
//
// This test used to close by asserting that an UNKNOWN key did not
// survive, and told whoever fixed that to come back and change it. That
// is memql-cockpit#333, now done: ClusterConfig.Extra is a
// `yaml:",inline"` catch-all, so the cockpit preserves what it does not
// model and the two tools are finally symmetric -- the extension's
// Document API and the cockpit's struct marshal both hand unknown keys
// back unchanged. The assertion below is inverted to match.
//
// `local` still gets its own modelled field rather than riding in Extra,
// and should keep it: the cockpit WRITES this key on its own (a
// cockpit-created entry has to carry it), and preservation only covers
// keys some other tool put there first.
func TestClusterLocalFieldRoundTrip(t *testing.T) {
	// A file as the extension writes it: `local: true` plus a key this
	// Go version does not know about.
	const onDisk = `clusters:
    - name: staging
      endpoint: https://cockpit.staging.example.com
      local: true
      future_key: kept-by-the-extension
selected_cluster: staging
`

	var loaded ClustersFile
	if err := yaml.Unmarshal([]byte(onDisk), &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(loaded.Clusters) != 1 {
		t.Fatalf("clusters = %d, want 1", len(loaded.Clusters))
	}
	if !loaded.Clusters[0].Local {
		t.Error("local: true did not decode onto ClusterConfig.Local")
	}

	// Save it back the way the cockpit does, then reload: Local has to
	// be there both on the wire and in the decoded struct.
	saved, err := yaml.Marshal(&loaded)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(saved), "local: true") {
		t.Errorf("re-marshalled file lost `local: true`:\n%s", saved)
	}

	var reloaded ClustersFile
	if err := yaml.Unmarshal(saved, &reloaded); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if !reloaded.Clusters[0].Local {
		t.Error("Local did not survive marshal -> unmarshal")
	}

	// A cluster the cockpit writes with Local unset stays quiet on the
	// wire -- omitempty, so an untouched registry does not grow a
	// `local: false` on every save.
	quiet, err := yaml.Marshal(&ClustersFile{
		Clusters: []ClusterConfig{{Name: "prod", Endpoint: "https://cockpit.example.com"}},
	})
	if err != nil {
		t.Fatalf("marshal quiet: %v", err)
	}
	if strings.Contains(string(quiet), "local:") {
		t.Errorf("Local=false should be omitted, got:\n%s", quiet)
	}

	// The former limitation, now inverted (memql-cockpit#333): a key the
	// cockpit does not model rides through the struct round trip on
	// ClusterConfig.Extra, value intact.
	if !strings.Contains(string(saved), "future_key: kept-by-the-extension") {
		t.Errorf("an unmodelled key was dropped on the cockpit's round trip:\n%s", saved)
	}
	if got := reloaded.Clusters[0].Extra["future_key"]; got != "kept-by-the-extension" {
		t.Errorf("future_key reloaded as %#v, want %q", got, "kept-by-the-extension")
	}
}

// TestUnknownKeysSurviveRoundTrip is memql-cockpit#333's acceptance
// criterion on its own terms: a read-modify-write through the cockpit
// preserves every key it does not model, at BOTH levels of the file, and
// does not invent any.
//
// The scenario is the real one. clusters.yaml is shared with the memQL VS
// Code extension, which ships on its own cadence and adds per-cluster
// state the engine-side design already anticipates more of. Before this,
// each such key silently vanished the first time an operator touched a
// cluster in the cockpit -- no error, no log line, just a setting gone.
func TestUnknownKeysSurviveRoundTrip(t *testing.T) {
	// A file with unmodelled keys at both levels, of three shapes: a
	// scalar, a nested map, and a list. A catch-all that only carried
	// scalars would still quietly flatten the other two.
	const onDisk = `clusters:
    - name: staging
      endpoint: https://cockpit.staging.example.com
      local: true
      version: v0.18.0
      future_scalar: kept
      future_map:
        nested: value
        depth: 2
      future_list:
        - one
        - two
selected_cluster: staging
future_top_level: also-kept
`

	var loaded ClustersFile
	if err := yaml.Unmarshal([]byte(onDisk), &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Modify something the way the cockpit would, so this is a genuine
	// read-MODIFY-write rather than a byte-for-byte copy.
	loaded.SelectedCluster = "local"

	saved, err := yaml.Marshal(&loaded)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reloaded ClustersFile
	if err := yaml.Unmarshal(saved, &reloaded); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}

	if len(reloaded.Clusters) != 1 {
		t.Fatalf("clusters = %d, want 1", len(reloaded.Clusters))
	}
	c := reloaded.Clusters[0]

	// The modelled fields still land on their fields, not in the map --
	// a catch-all that swallowed known keys would break both tools.
	if c.Name != "staging" || !c.Local || c.Version != "v0.18.0" {
		t.Errorf("modelled fields did not survive: %+v", c)
	}
	for _, known := range []string{"name", "endpoint", "local", "version"} {
		if _, stolen := c.Extra[known]; stolen {
			t.Errorf("modelled key %q was captured into Extra", known)
		}
	}

	if got := c.Extra["future_scalar"]; got != "kept" {
		t.Errorf("future_scalar = %#v, want %q", got, "kept")
	}
	nested, ok := c.Extra["future_map"].(map[string]any)
	if !ok {
		t.Fatalf("future_map reloaded as %T, want a map", c.Extra["future_map"])
	}
	if nested["nested"] != "value" || nested["depth"] != 2 {
		t.Errorf("future_map = %#v, want nested:value depth:2", nested)
	}
	list, ok := c.Extra["future_list"].([]any)
	if !ok {
		t.Fatalf("future_list reloaded as %T, want a list", c.Extra["future_list"])
	}
	if len(list) != 2 || list[0] != "one" || list[1] != "two" {
		t.Errorf("future_list = %#v, want [one two]", list)
	}

	if got := reloaded.Extra["future_top_level"]; got != "also-kept" {
		t.Errorf("top-level future_top_level = %#v, want %q", got, "also-kept")
	}
	if reloaded.SelectedCluster != "local" {
		t.Errorf("the modification itself was lost: selected_cluster = %q", reloaded.SelectedCluster)
	}

	// A cluster with nothing unmodelled must not grow an empty mapping
	// or a `extra: {}` key. Every entry in every operator's file is in
	// this state, so a save that dirtied all of them would be worse than
	// the bug this fixes.
	quiet, err := yaml.Marshal(&ClustersFile{
		Clusters: []ClusterConfig{{Name: "prod", Endpoint: "https://cockpit.example.com"}},
	})
	if err != nil {
		t.Fatalf("marshal quiet: %v", err)
	}
	if strings.Contains(string(quiet), "extra") || strings.Contains(string(quiet), "{}") {
		t.Errorf("an empty Extra leaked into the wire form:\n%s", quiet)
	}
}

// TestUnknownKeysStableAcrossRepeatedSaves guards the shape of the fix
// rather than its presence. Go map iteration is randomised, so an inline
// catch-all that emitted its keys in map order would rewrite the file
// differently on every save -- which turns `memql cluster remove` into a
// diff across every untouched entry, and makes the file useless to keep
// in version control or to diff after an incident.
//
// yaml.v3 sorts inline-map keys; this pins that so an encoder change
// cannot silently take it away.
func TestUnknownKeysStableAcrossRepeatedSaves(t *testing.T) {
	const onDisk = `clusters:
    - name: staging
      endpoint: https://cockpit.staging.example.com
      zulu: 1
      alpha: 2
      mike: 3
      bravo: 4
`
	var loaded ClustersFile
	if err := yaml.Unmarshal([]byte(onDisk), &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	first, err := yaml.Marshal(&loaded)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := range 20 {
		var round ClustersFile
		if err := yaml.Unmarshal(first, &round); err != nil {
			t.Fatalf("re-unmarshal %d: %v", i, err)
		}
		again, err := yaml.Marshal(&round)
		if err != nil {
			t.Fatalf("re-marshal %d: %v", i, err)
		}
		if string(again) != string(first) {
			t.Fatalf("save %d differs from save 0:\n--- first ---\n%s\n--- again ---\n%s", i, first, again)
		}
	}
}

// TestClusterVersionFieldRoundTrip is TestClusterLocalFieldRoundTrip's twin,
// for the `version:` key the memQL VS Code plugin records per cluster
// (znasllc-io/memql#3994).
//
// The hazard is the same and worth restating, because it is the whole reason
// this test exists rather than the field just being added: the cockpit saves
// by marshalling ClusterConfig over the file, so any key it does not model is
// gone the next time an operator edits a cluster here. The plugin's recorded
// version is written far more often than a human edit -- it refreshes
// opportunistically from four sources -- so "the cockpit quietly deletes it"
// would present as the upgrade banner vanishing for no reason an operator
// could connect to anything they did.
//
// The version is a plain string, which is why this is simpler than `local`:
// there is no false-serialises-as-absent collapse for the two tools to agree
// about. omitempty drops only "", and "" is not a version either tool writes.
func TestClusterVersionFieldRoundTrip(t *testing.T) {
	// A file as the plugin writes it, with the version alongside `local`.
	const onDisk = `clusters:
    - name: staging
      endpoint: https://cockpit.staging.example.com
      local: true
      version: v0.18.0
selected_cluster: staging
`

	var loaded ClustersFile
	if err := yaml.Unmarshal([]byte(onDisk), &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(loaded.Clusters) != 1 {
		t.Fatalf("clusters = %d, want 1", len(loaded.Clusters))
	}
	if got := loaded.Clusters[0].Version; got != "v0.18.0" {
		t.Errorf("version did not decode onto ClusterConfig.Version: got %q", got)
	}

	// Save it back the way the cockpit does, then reload.
	saved, err := yaml.Marshal(&loaded)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(saved), "version: v0.18.0") {
		t.Errorf("re-marshalled file lost `version: v0.18.0`:\n%s", saved)
	}

	var reloaded ClustersFile
	if err := yaml.Unmarshal(saved, &reloaded); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if got := reloaded.Clusters[0].Version; got != "v0.18.0" {
		t.Errorf("Version did not survive marshal -> unmarshal: got %q", got)
	}

	// Whatever the cluster said, unmangled. The record is a report, not a
	// judgement: a branch name, a commit sha, or the `0.15.0-<epoch>` build
	// stamp every shipped engine reports are all legitimate values, and the
	// plugin's comparator can only refuse to compare a value that reached
	// disk intact. Nothing here may normalise or reject them.
	for _, recorded := range []string{"0.18.0", "0.15.0-1737072000", "feat/some-branch", "9fd53842"} {
		round, err := yaml.Marshal(&ClustersFile{
			Clusters: []ClusterConfig{{Name: "c", Version: recorded}},
		})
		if err != nil {
			t.Fatalf("marshal %q: %v", recorded, err)
		}
		var back ClustersFile
		if err := yaml.Unmarshal(round, &back); err != nil {
			t.Fatalf("unmarshal %q: %v", recorded, err)
		}
		if got := back.Clusters[0].Version; got != recorded {
			t.Errorf("version %q round-tripped as %q", recorded, got)
		}
	}

	// A cluster the cockpit writes with no known version stays quiet on the
	// wire. Every cluster in an operator's file predates this key, so absent
	// is the normal case and a save must not grow `version: ""` on all of
	// them.
	quiet, err := yaml.Marshal(&ClustersFile{
		Clusters: []ClusterConfig{{Name: "prod", Endpoint: "https://cockpit.example.com"}},
	})
	if err != nil {
		t.Fatalf("marshal quiet: %v", err)
	}
	if strings.Contains(string(quiet), "version:") {
		t.Errorf("an unset Version should be omitted, got:\n%s", quiet)
	}
}

func TestStoredTokenExpiry(t *testing.T) {
	t.Run("zero time is expired", func(t *testing.T) {
		token := &StoredToken{}
		if !token.IsExpired() {
			t.Error("zero expiry should be expired")
		}
	})
}
