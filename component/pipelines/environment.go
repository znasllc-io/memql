package pipelines

import (
	"strconv"
	"strings"
)

// Environment renders the environment every runner exports to a step's
// command (D10). One rendering, here, so the driver and every runner agree on
// the contract without restating it.
//
// The platform's own names win over a secret of the same name. The compiler
// already refuses a secret named MEMQL_*, so this is the second wall, not the
// first.
func (r StepRequest) Environment() map[string]string {
	env := map[string]string{
		"MEMQL_RUN_ID":      r.RunID,
		"MEMQL_WORK_RUN_ID": r.WorkRunID,
		"MEMQL_STEP":        r.StepKey,
		"MEMQL_REPOSITORY":  r.Repository.FullName(),
		"MEMQL_SHA":         r.SHA,
		"MEMQL_MODE":        string(r.Mode),
		"MEMQL_EVENT":       string(r.Event),
		"MEMQL_VERSION":     r.Version,
		"MEMQL_PACKAGES":    strings.Join(r.Step.Packages, " "),
	}
	if r.Step.Shard.Count > 0 {
		env["MEMQL_SHARD"] = strconv.Itoa(r.Step.Shard.Index) + "/" + strconv.Itoa(r.Step.Shard.Count)
	}
	for name, value := range r.Secrets {
		if _, reserved := env[name]; reserved {
			continue
		}
		env[name] = value
	}
	return env
}
