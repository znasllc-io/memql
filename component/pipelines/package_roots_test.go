package pipelines

import (
	"reflect"
	"testing"
)

func TestPackageRootsPreserveDependencySelectionAndSkipUnrelatedFleetWork(t *testing.T) {
	g := scanTree(t, true)
	spec := &Spec{Select: &Select{Go: SelectImportGraph}, Stages: []StageSpec{{Name: "native", Steps: []StepSpec{{Name: "docker", Run: "go test $MEMQL_PACKAGES", Packages: PackagesAffected, PackageRoots: []string{"sub"}, Placement: PlacementFleet, Execution: ExecutionNative, Platform: "linux/amd64", Needs: map[string]bool{"docker": true}}}}}}
	for _, tc := range []struct {
		name, path string
		want       []string
	}{
		{"nested source", "sub/client/client.go", []string{"example.test/shop/sub/client"}},
		{"shared dependency", "a/a.go", []string{"example.test/shop/sub/client"}},
		{"unrelated source", "b/b.go", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selector := GraphSelector(g, affectedOrFatal(t, g, []string{tc.path}))
			plan, refusal := Compile(spec, CompileInput{Mode: ModeAffected, Event: EventPullRequest, Compute: ComputeClusterAndFleet, Selector: selector, PackagePolicies: testPackagePolicies(spec, ModeAffected, selector)})
			if refusal != nil {
				t.Fatal(refusal)
			}
			steps := plan.Steps()
			if len(steps) != 1 {
				t.Fatalf("steps = %v", steps)
			}
			if !reflect.DeepEqual(steps[0].Packages, tc.want) {
				t.Fatalf("packages = %v, want %v", steps[0].Packages, tc.want)
			}
			if (steps[0].Skip != nil) != (len(tc.want) == 0) {
				t.Fatalf("fleet skip = %v, packages = %v", steps[0].Skip, steps[0].Packages)
			}
		})
	}
}

func TestPackageRootsRefuseUnknownPackageDirectories(t *testing.T) {
	spec := &Spec{Select: &Select{Go: SelectImportGraph}, Stages: []StageSpec{{Name: "tests", Steps: []StepSpec{{Name: "go", Run: "go test $MEMQL_PACKAGES", Packages: PackagesAffected, PackageRoots: []string{"component"}}}}}}
	selector := selectorStub{all: []string{"example.test/unknown"}, affected: Selection{Packages: []string{"example.test/unknown"}}}
	_, refusal := Compile(spec, CompileInput{Mode: ModeAffected, Selector: selector, PackagePolicies: testPackagePolicies(spec, ModeAffected, selector)})
	if refusal == nil || refusal.Code != CodeSelectInvalid {
		t.Fatalf("unknown directory silently skipped: %v", refusal)
	}
	for _, invalid := range []string{"../outside", "/absolute", "component/**"} {
		spec.Stages[0].Steps[0].PackageRoots = []string{invalid}
		if refusal := Validate(spec); refusal == nil {
			t.Fatalf("accepted package root %q", invalid)
		}
	}
}
