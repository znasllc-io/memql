package app

import (
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/integrations/planner"
)

// Compile-time proof that the cluster guard (the #561 DB-PK claim with
// memql#1142 bounded fail-open) satisfies the planner's cross-replica
// plan-execution claim seam (memql#1363). The wiring itself
// (integrations_planner_init.go passing a.clusterGuard to
// planner.WithClusterClaimer) only compiles under -tags planner, which CI
// does not build for tests (memql#1338); this untagged assertion keeps the
// contract checked on every `go test ./...`.
//
// NOTE: this deliberately lives in the app test package, NOT in
// integrations/planner's tests -- importing component/automations there
// registers the real Gate 1 automation-compile hook
// (automations/sandbox_bridge.go init) into the planner test binary and
// changes what the authoring Gate 1 tests exercise.
var _ planner.ExecutionClaimer = (*automations.ClusterExecutionGuard)(nil)
