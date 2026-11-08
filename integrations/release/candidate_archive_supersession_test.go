package release

import (
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestCatalogSupersedesSameOrHigherVersionOfTheSameComponent(t *testing.T) {
	want := pl.ReleaseComponent{Name: "engine", Version: "0.25.0"}
	for _, tc := range []struct {
		name, version string
		supersedes    bool
	}{
		{"engine", "0.25.0", true},
		{"engine", "v0.25.0", true},
		{"engine", "0.25.1", true},
		{"engine", "0.26.0", true},
		{"engine", "1.0.0", true},
		{"engine", "0.24.9", false},
		{"engine", "0.9.99", false},
		{"cockpit", "0.25.0", false},
		{"engine", "0.25", false},
		{"engine", "", false},
	} {
		published := pl.PublishedRelease{Components: []pl.PublishedComponent{{Name: tc.name, Version: tc.version}}}
		if got := catalogSupersedes(published, want); got != tc.supersedes {
			t.Errorf("%s %s supersedes engine 0.25.0 = %v, want %v", tc.name, tc.version, got, tc.supersedes)
		}
	}
	if catalogSupersedes(pl.PublishedRelease{Components: []pl.PublishedComponent{{Name: "engine", Version: "0.26.0"}}}, pl.ReleaseComponent{Name: "engine", Version: "bad"}) {
		t.Error("an unparseable candidate version was superseded")
	}
	if !catalogSupersedes(pl.PublishedRelease{Components: []pl.PublishedComponent{{Name: "cockpit", Version: "0.30.0"}, {Name: "engine", Version: "0.25.0"}}}, want) {
		t.Error("a catalog listing the component second did not supersede it")
	}
}
