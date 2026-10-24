package access

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/cockpit/internal/config"
)

func file(selected string, names ...string) *config.ClustersFile {
	f := &config.ClustersFile{SelectedCluster: selected}
	for _, n := range names {
		f.Clusters = append(f.Clusters, config.ClusterConfig{Name: n, Endpoint: "https://api." + n + ".test"})
	}
	return f
}

func TestResolveClusterByName(t *testing.T) {
	got, err := resolveCluster(file("", "acme", "beta"), "beta")
	if err != nil {
		t.Fatalf("resolveCluster: %v", err)
	}
	if got.Name != "beta" {
		t.Errorf("Name = %q, want beta", got.Name)
	}
}

// A name that is not registered must say so AND say what is, so the reader
// does not have to run a second command to find the spelling.
func TestResolveClusterNamesTheRegisteredOnesWhenTheNameIsWrong(t *testing.T) {
	_, err := resolveCluster(file("", "acme", "beta"), "acmee")
	if err == nil {
		t.Fatal("resolveCluster succeeded for an unregistered name")
	}
	for _, want := range []string{"acmee", "acme", "beta"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q: %v", want, err)
		}
	}
}

// With no argument the working cluster the operator already chose is the
// obvious answer -- it is the same value the VS Code extension reads.
func TestResolveClusterFallsBackToTheSelectedCluster(t *testing.T) {
	got, err := resolveCluster(file("beta", "acme", "beta"), "")
	if err != nil {
		t.Fatalf("resolveCluster: %v", err)
	}
	if got.Name != "beta" {
		t.Errorf("Name = %q, want beta", got.Name)
	}
}

func TestResolveClusterUsesTheOnlyClusterWhenThereIsExactlyOne(t *testing.T) {
	got, err := resolveCluster(file("", "acme"), "")
	if err != nil {
		t.Fatalf("resolveCluster: %v", err)
	}
	if got.Name != "acme" {
		t.Errorf("Name = %q, want acme", got.Name)
	}
}

// Several clusters and nothing chosen is genuinely ambiguous. Picking one
// would report somebody's access on a cluster they did not ask about.
func TestResolveClusterRefusesToGuessBetweenSeveral(t *testing.T) {
	_, err := resolveCluster(file("", "acme", "beta", "gamma"), "")
	if err == nil {
		t.Fatal("resolveCluster guessed between three clusters")
	}
	for _, want := range []string{"acme", "beta", "gamma"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q: %v", want, err)
		}
	}
}

// A stale selection with exactly ONE cluster left is not ambiguous, and the
// usage text promises "the selected cluster ... or the only registered one".
// `memql cluster remove` does not clear selected_cluster, so remove-then-add
// reaches this state through ordinary use -- and refusing there dead-ends the
// command with nothing the reader can act on but a name they did not choose.
func TestResolveClusterUsesTheOnlyClusterEvenWhenTheSelectionIsStale(t *testing.T) {
	got, err := resolveCluster(file("removed", "beta"), "")
	if err != nil {
		t.Fatalf("resolveCluster refused with one cluster left: %v", err)
	}
	if got.Name != "beta" {
		t.Errorf("Name = %q, want beta", got.Name)
	}
}

func TestResolveClusterPointsAtClusterAddWhenNothingIsRegistered(t *testing.T) {
	_, err := resolveCluster(file(""), "")
	if err == nil {
		t.Fatal("resolveCluster succeeded with no clusters registered")
	}
	if !strings.Contains(err.Error(), "memql cluster add") {
		t.Errorf("error does not name the repair: %v", err)
	}
}

// A stale selected_cluster naming a removed cluster must not be silently
// ignored -- the operator's chosen working cluster disappearing is worth a
// sentence.
func TestResolveClusterReportsAStaleSelectedCluster(t *testing.T) {
	_, err := resolveCluster(file("removed", "acme", "beta"), "")
	if err == nil {
		t.Fatal("resolveCluster succeeded with a stale selected_cluster")
	}
	if !strings.Contains(err.Error(), "removed") {
		t.Errorf("error does not name the stale selection: %v", err)
	}
}
