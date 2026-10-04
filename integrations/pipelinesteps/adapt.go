package pipelinesteps

import (
	"io"
	"maps"
	"slices"
	"sort"
	"strings"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// adapt.go -- a pl.StepRequest becomes the StepRun both of the agent's paths
// hand a step (epic memql#5478). Computed once, on the agent, so the cluster's
// Job and a fleet machine run the same contract: the same environment, the
// same secrets, the same deadline.

// stepRunFor is the StepRun for one request, given its effective timeout and
// the bound that set it (pl.CodeStepTimeout or pl.CodeRunCeiling).
//
// Every slice and map is the StepRun's own: the driver's request is not the
// runner's to edit, nor the other way around.
func stepRunFor(req pl.StepRequest, timeoutSeconds int, deadlineCode string) StepRun {
	env, secrets := splitEnvironment(req)
	var services map[string]pl.Service
	if len(req.Step.Services) > 0 {
		services = make(map[string]pl.Service, len(req.Step.Services))
		for name, svc := range req.Step.Services {
			svc.Env = maps.Clone(svc.Env)
			services[name] = svc
		}
	}
	return StepRun{
		RunID:          req.RunID,
		WorkRunID:      req.WorkRunID,
		StepKey:        req.StepKey,
		Attempt:        max(req.Attempt, 1),
		OwnerUserID:    req.OwnerUserID,
		Repository:     req.Repository,
		SHA:            req.SHA,
		InstallationID: req.InstallationID,
		Image:          req.Step.Image,
		Command:        req.Step.Run,
		Env:            env,
		Secrets:        secrets,
		Services:       services,
		Caches:         slices.Clone(req.Step.Caches),
		Artifacts:      slices.Clone(req.Step.Artifacts),
		TimeoutSeconds: timeoutSeconds,
		DeadlineCode:   deadlineCode,
		GoTimings:      wantsGoTimings(req.Step),
	}
}

// splitEnvironment renders the step's environment through the seam's own
// StepRequest.Environment() -- the one rendering of the contract -- and
// separates the secrets from it: Env is every contract variable as a plain
// value, Secrets the resolved values, which travel only as secrets.
//
// A secret named like a contract variable is not carried at all. Environment()
// keeps the platform's value for such a name (the compiler refuses one first),
// so the union of the two maps is exactly what Environment() renders, and no
// runner is handed a secret that could win where the platform's value must.
func splitEnvironment(req pl.StepRequest) (env, secrets map[string]string) {
	contract := req
	contract.Secrets = nil
	env = contract.Environment()
	for name, value := range req.Secrets {
		if _, reserved := env[name]; reserved {
			continue
		}
		if secrets == nil {
			secrets = make(map[string]string, len(req.Secrets))
		}
		secrets[name] = value
	}
	return env, secrets
}

// wantsGoTimings says whether a step's output is worth reading for Go test
// timings: a step that selects Go packages (MEMQL_PACKAGES) or runs `go test`
// itself. Reading is cheap and only anchored passing-package lines count, but
// the archive can be 64 MiB, so it is read for the steps that print them.
func wantsGoTimings(step pl.Step) bool {
	return len(step.Packages) > 0 || strings.Contains(step.Run, "go test")
}

// goTestTimings reads each passing Go package's wall time out of a step's
// archived log with the seam's one reader (pl.ParseGoTestOutput) -- for both
// of a step's paths, the cluster's and a fleet machine's. A log it cannot read
// for them is a note (pl.CodeTimingsUnreadable), never the step's failure: the
// timings only steer how later runs are sharded. The note quotes the log, so
// the caller masks it.
func goTestTimings(archive io.Reader) (map[string]float64, *pl.Failure) {
	timings, err := pl.ParseGoTestOutput(archive)
	switch {
	case err != nil:
		return nil, &pl.Failure{Code: pl.CodeTimingsUnreadable, Message: "the step's Go test timings could not be read from its log: " + err.Error()}
	case len(timings) == 0:
		return nil, nil
	}
	return timings, nil
}

// secretValues is every value a step's output must be masked for: the
// resolved secrets and each clone token it may have run with. Sorted, blanks
// dropped.
func secretValues(run StepRun, tokens ...string) []string {
	out := make([]string, 0, len(run.Secrets)+len(tokens))
	for _, v := range run.Secrets {
		if v != "" {
			out = append(out, v)
		}
	}
	for _, token := range tokens {
		if token != "" {
			out = append(out, token)
		}
	}
	sort.Strings(out)
	return out
}
