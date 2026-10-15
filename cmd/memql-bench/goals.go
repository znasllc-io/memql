package main

import (
	"context"
	"crypto/x509"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/znasllc-io/memql/component/proving"
	"github.com/znasllc-io/memql/component/proving/capability"
	"github.com/znasllc-io/memql/component/proving/goalfixture"
	"github.com/znasllc-io/memql/component/work/goaleval"
	"github.com/znasllc-io/memql/sdk/go/client"
)

func doGoalEvaluation(c *capability.Capability) int {
	name := c.Param("goal-catalog", "test/proving/goals/catalog.json")
	if filepath.IsAbs(name) {
		return c.Fail(capability.ExitBadParam, "goal catalog must be relative to the current source tree")
	}
	catalog, err := goaleval.Load(os.DirFS("."), filepath.ToSlash(name))
	if err != nil {
		return c.Fail(capability.ExitBadParam, "%v", err)
	}
	b, err := os.ReadFile(c.Param("matrix", "test/proving/goals/matrix.json"))
	if err != nil {
		return c.Fail(capability.ExitBadParam, "%v", err)
	}
	matrix, err := goaleval.Decode[goaleval.Matrix](b)
	if err != nil {
		return c.Fail(capability.ExitBadParam, "%v", err)
	}
	if err := matrix.Validate(catalog); err != nil {
		return c.Fail(capability.ExitRefused, "%v", err)
	}
	var readiness []map[string]any
	for _, id := range matrix.ScenarioIDs {
		for _, s := range catalog.Scenarios {
			if s.ID == id {
				reason := goaleval.Availability(s)
				readiness = append(readiness, map[string]any{"scenarioId": id, "available": reason == "", "reason": reason, "fingerprint": s.Fingerprint()})
			}
		}
	}
	c.Set("readiness", readiness)
	if c.Param("do", "") == "goal-plan" {
		return c.OK()
	}
	endpoint, engineCommit := c.Param("endpoint", ""), c.Param("engine-commit", "")
	if !strings.HasPrefix(endpoint, "https://") || !regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(engineCommit) {
		return c.Fail(capability.ExitBadParam, "goal-run requires a TLS front door and exact --engine-commit")
	}
	token := strings.TrimSpace(os.Getenv("MEMQL_EVALUATION_TOKEN"))
	if token == "" {
		return c.Fail(capability.ExitPrerequisite, "MEMQL_EVALUATION_TOKEN is required")
	}
	var roots *x509.CertPool
	if path := c.Param("ca-file", ""); path != "" {
		pem, err := os.ReadFile(path)
		if err != nil {
			return c.Fail(capability.ExitBadParam, "cannot read configured CA file")
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return c.Fail(capability.ExitBadParam, "CA file contains no certificates")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	connectCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	conn, err := client.Connect(connectCtx, client.ConnectConfig{Endpoint: endpoint, Token: token, RootCAs: roots})
	cancel()
	if err != nil {
		return c.Fail(capability.ExitPrerequisite, "connect to evaluation cluster: %v", err)
	}
	defer conn.Close()
	built := conn.ServerInfo().EngineCommit
	if len(built) < 12 || strings.Contains(built, "dirty") || !strings.HasPrefix(engineCommit, built) {
		return c.Fail(capability.ExitRefused, "server build %q does not match the exact engine commit", built)
	}
	harness, err := goalfixture.Command(ctx, ".", "git", "rev-parse", "HEAD")
	if err != nil {
		return c.Fail(capability.ExitPrerequisite, "read harness provenance: %v", err)
	}
	dirty, err := goalfixture.Command(ctx, ".", "git", "status", "--porcelain")
	if err != nil || strings.TrimSpace(dirty) != "" {
		return c.Fail(capability.ExitRefused, "commit the harness before measuring it")
	}
	output := c.Param("output", "")
	if output == "" {
		return c.Fail(capability.ExitBadParam, "--output must name a new evidence directory")
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return c.Fail(capability.ExitBadParam, "%v", err)
	}
	if err := os.Mkdir(output, 0700); err != nil {
		return c.Fail(capability.ExitRefused, "evidence directory must be new: %v", err)
	}
	runner := proving.GoalRunner{LessonID: c.Param("lesson-id", ""), LessonReceipt: c.Param("lesson-receipt", ""), Client: client.NewQueryClient(conn.Dispatcher()), Files: os.DirFS("."), CatalogDir: filepath.ToSlash(filepath.Dir(name)), OutputDir: output, EngineCommit: engineCommit, HarnessCommit: strings.TrimSpace(harness)}
	c.Step("running local goal matrix; paid providers disabled; receipts in %s", output)
	report, err := runner.Run(ctx, catalog, matrix)
	c.Changed()
	c.Set("report", filepath.Join(output, "report.json"))
	c.Set("attempts", len(report.Attempts))
	if err != nil {
		return c.Fail(capability.ExitOpFailed, "%v", err)
	}
	var measured, successful int
	for _, a := range report.Attempts {
		if a.Result != nil {
			measured++
			if a.Result.Outcome == "autonomous" || a.Result.Outcome == "assisted" {
				successful++
			}
		}
	}
	c.Set("measured", measured)
	c.Set("successful", successful)
	if measured == 0 {
		return c.Fail(capability.ExitPrerequisite, "no live scenarios ran; inspect prerequisite records")
	}
	if successful != measured {
		return c.Fail(capability.ExitOpFailed, "%s", fmt.Sprintf("%d of %d measured scenarios did not pass; raw failures are preserved", measured-successful, measured))
	}
	return c.OK()
}
