package ci

// Bound native commands, including a newly added step that omits its timeout.
// Queue wait time is separate from this execution budget.
import (
	"github.com/znasllc-io/memql/component/pipelines"
	"testing"
	"time"
)

func TestEveryNativeStepHasABoundedTimeout(t *testing.T) {
	if pipelines.DefaultStepTimeout <= 0 || pipelines.DefaultStepTimeout > 30*time.Minute {
		t.Fatalf("unbounded or excessive default: %s", pipelines.DefaultStepTimeout)
	}
	count := 0
	for _, stage := range nativePipeline(t).Stages {
		for _, step := range stage.Steps {
			count++
			timeout := pipelines.DefaultStepTimeout
			if step.Timeout != "" {
				var err error
				timeout, err = time.ParseDuration(step.Timeout)
				if err != nil {
					t.Fatal(err)
				}
			}
			if timeout <= 0 || timeout > pipelines.MaxStepTimeout {
				t.Errorf("%s.%s has invalid timeout %s", stage.Name, step.Name, timeout)
			}
		}
	}
	if count == 0 {
		t.Fatal("no native steps checked")
	}
}

// Historical measured medians remain a conservative floor while native timing
// observations accumulate. These are retained suite contracts, not job names.
func TestNativeSuiteTimeoutsRetainHeadroom(t *testing.T) {
	medians := map[string]float64{"go-checks": 6.3, "db-tests": 7.4, "editor-packages": 4.0, "build-identity": 1.9, "mcp-conformance": 1.6, "sdk-ts-typecheck": 0.3}
	seen := map[string]bool{}
	for _, stage := range nativePipeline(t).Stages {
		for _, step := range stage.Steps {
			median, ok := medians[step.Name]
			if !ok {
				continue
			}
			seen[step.Name] = true
			timeout := pipelines.DefaultStepTimeout
			if step.Timeout != "" {
				var err error
				timeout, err = time.ParseDuration(step.Timeout)
				if err != nil {
					t.Fatal(err)
				}
			}
			if timeout.Minutes() < 2*median {
				t.Errorf("%s timeout %s is below twice its %.1fm measured median", step.Name, timeout, median)
			}
		}
	}
	for name := range medians {
		if !seen[name] {
			t.Errorf("timed suite %s disappeared", name)
		}
	}
}
