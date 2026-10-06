package pipelinerun

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/pipelines"
)

// secrets.go -- a step's secrets, resolved for one request and nowhere else
// (design record D8; plan Review Focus 5).
//
// A manifest names secrets; the pipeline row's secretNames is the owner's
// allowlist; the compile refuses a step naming a secret the allowlist does
// not hold (pipeline_secret_not_allowed) or one in the platform's MEMQL_
// namespace (pipeline_secret_invalid) before any step runs. This is the last
// wall, not the first: the same two rules are asked again here, so a value is
// never resolved for a name the owner did not allow, whatever reached this
// far.
//
// A VALUE IS NEVER WRITTEN AND NEVER LOGGED. It goes into the StepRequest the
// executor receives and into the drive's mask list, which every text the
// runner reports back is masked with before it reaches a row or the check run.
// A failure names the secret, never its value -- and a resolver's error is
// not repeated, because what a resolver says about a value it could not read
// is not this package's to pass on.

// resolveSecrets resolves the values of a step's secrets under the pipeline
// owner's borrowed actor (D8), or answers the failure that fails the step.
func (dr *runDriver) resolveSecrets(ctx context.Context, step pipelines.Step) (map[string]string, *pipelines.Failure) {
	names := step.SecretNames()
	if len(names) == 0 {
		return nil, nil
	}
	values := make(map[string]string, len(names))
	for _, name := range names {
		switch {
		case strings.HasPrefix(name, reservedSecretPrefix):
			return nil, &pipelines.Failure{Code: pipelines.CodeSecretInvalid, Message: fmt.Sprintf(
				"The step names %s, in the platform's own MEMQL_ namespace, which a step's secret never shadows; it was not resolved.", name)}
		case !slices.Contains(dr.p.SecretNames, name):
			return nil, &pipelines.Failure{Code: pipelines.CodeSecretNotAllowed, Message: fmt.Sprintf(
				"The step names %s, which the pipeline's owner has not allowed; it was not resolved.", name)}
		}
		value := ""
		if dr.d.Secrets != nil {
			v, err := dr.d.Secrets(auth.ContextWithUserActor(ctx, dr.p.OwnerUserID), name)
			if err == nil {
				value = v
			} else {
				dr.log.Info("pipelines: a step's secret did not resolve", "step", step.Key, "secret", name)
			}
		}
		if value == "" {
			return nil, &pipelines.Failure{Code: pipelines.CodeSecretMissing, Message: fmt.Sprintf(
				"No value is stored on this cluster for %s, which the step names and the pipeline allows. Store one under that name, then re-run.", name)}
		}
		values[name] = value
		dr.remember(value)
	}
	return values, nil
}
