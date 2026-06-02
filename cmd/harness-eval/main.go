// harness-eval runs the MemQL-native agent harness eval scaffold (#589):
// a fixed set of task fixtures driven through the harness reconciler over an
// in-memory graph (no Postgres, no LLM), scored on task success, step count,
// tool-call count, token cost, and wall-clock, then checked against a
// regression threshold. It is the CI gate that fails on a harness
// regression -- see the "harness eval" lane in .github/workflows/ci.yml.
//
// Usage:
//
//	harness-eval [flags]
//
//	go run ./cmd/harness-eval
//	go run ./cmd/harness-eval --json
//
// Flags:
//
//	--json   Emit the suite report as JSON (for trend tracking).
//	--help   Print usage.
//
// Exit codes:
//
//	0  the suite cleared the regression threshold
//	1  the suite regressed below the threshold (CI gate trips)
//	2  invalid usage / internal error
//
// The fixtures + default threshold live in component/harness
// (DefaultFixtures / DefaultThreshold) so the gate, the importable eval
// package, and the cockpit share one definition.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/znasllc-io/memql/component/harness"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("harness-eval", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "emit the suite report as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx := context.Background()
	fixtures := harness.DefaultFixtures()
	threshold := harness.DefaultThreshold()

	report, err := harness.RunEval(ctx, fixtures)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: eval run failed: %v\n", err)
		return 2
	}
	verdict := report.CheckThreshold(threshold)

	if *asJSON {
		emitJSON(report, threshold, verdict)
	} else {
		emitText(report, verdict)
	}

	if !verdict.Passed {
		return 1
	}
	return 0
}

func emitText(report harness.EvalReport, verdict harness.GateVerdict) {
	fmt.Println("=========================================")
	fmt.Println("Harness Eval (#589)")
	fmt.Println("=========================================")
	for _, s := range report.Scores {
		status := "PASS"
		if !s.Passed {
			status = "FAIL"
		}
		fmt.Printf("%-4s %-20s success=%-5v steps=%d dispatches=%d toolCalls=%d tokens=%d wall=%s\n",
			status, s.Fixture, s.Success, s.StepCount, s.StepDispatches,
			s.ToolCalls, s.TokenCost, s.WallClock.Round(time.Microsecond))
		if !s.Passed {
			fmt.Printf("       reason: %s\n", s.Reason)
		}
	}
	fmt.Println("-----------------------------------------")
	fmt.Printf("fixtures=%d passed=%d successRate=%.3f totalToolCalls=%d totalTokens=%d totalWall=%s\n",
		report.Total, report.PassedCount, report.SuccessRate,
		report.TotalToolCalls, report.TotalTokens, report.TotalWallClock.Round(time.Microsecond))
	if verdict.Passed {
		fmt.Println("RESULT: PASS -- suite cleared the regression threshold")
	} else {
		fmt.Println("RESULT: FAIL -- suite regressed below the threshold:")
		for _, v := range verdict.Violations {
			fmt.Printf("  - %s\n", v)
		}
	}
}

func emitJSON(report harness.EvalReport, threshold harness.EvalThreshold, verdict harness.GateVerdict) {
	out := map[string]any{
		"report":    report,
		"threshold": threshold,
		"verdict":   verdict,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
