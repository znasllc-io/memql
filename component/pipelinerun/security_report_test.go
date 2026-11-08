package pipelinerun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/pipelines"
)

const securityFixtureSARIF = `{"version":"2.1.0","runs":[{"tool":{"driver":{"name":"CodeQL"}},"invocations":[{"executionSuccessful":true}],"results":[{"ruleId":"go/sql-injection","level":"error","message":{"text":"Untrusted SQL input"},"locations":[{"physicalLocation":{"artifactLocation":{"uri":"src/query.go"},"region":{"startLine":12}}}]}]}]}`

func setSecurityReportBody(h *sarifHarness, body string) {
	h.library.body = []byte(body)
	digest := sha256.Sum256(h.library.body)
	h.library.receipt.SHA256 = hex.EncodeToString(digest[:])
	h.library.receipt.Size = int64(len(h.library.body))
}

func TestSecurityReportCreatesGoalsWithoutGitHubPublication(t *testing.T) {
	for _, state := range []string{"complete", "scanner failed", "malformed"} {
		t.Run(state, func(t *testing.T) {
			h, dh := newReportDriveHarness(t, state)
			manifest := strings.Replace(reportManifest, "pipeline:\n", "pipeline:\n  schedule: weekly\n", 1)
			manifest = strings.ReplaceAll(manifest, "      steps:\n", "      security: true\n      on: [schedule, manual]\n      steps:\n")
			manifest = strings.ReplaceAll(manifest, "sarif: {", "sarif: {destination: goals, ")
			dh.setTree(shaA, manifest)
			setSecurityReportBody(h, securityFixtureSARIF)
			if state == "malformed" {
				setSecurityReportBody(h, `{"version":"2.1.0","runs":[]}`)
			}
			goals := 0
			dh.integ.Configure(func(d *Deps) {
				next := d.SystemVariable
				d.SystemVariable = func(ctx context.Context, name string) (string, error) {
					if name == pipelines.SecuritySettingsVariable {
						return `{"enabled":true}`, nil
					}
					if next == nil {
						return "", nil
					}
					return next(ctx, name)
				}
				// No publisher is available at all. Checkout still has its ordinary
				// source client, which does not implement the SARIF upload interface.
				d.GitHub = h.remote.fakeGitHub
				d.OpenSecurityGoal = func(ctx context.Context, req SecurityGoalRequest) (string, error) {
					if err := req.Check(ctx); err != nil {
						return "", err
					}
					if req.OwnerUserID != h.owner || req.Input["sha"] != shaA || !strings.Contains(req.Statement, "go/sql-injection") {
						t.Error("incorrect finding goal", req)
					}
					goals++
					return "security-goal", nil
				}
			})
			op := pushOpening()
			op.Event, op.Trigger = pipelines.EventManual, TriggerManual
			run := dh.openRun(t, op)
			deliver(t, dh.integ, run)
			got, _ := dh.store.run(run.ID)
			want, wantGoals := ConclusionFailure, 0
			if state == "complete" {
				want, wantGoals = ConclusionSuccess, 1
			}
			if got.Conclusion != want || goals != wantGoals {
				t.Fatalf("state=%s conclusion=%s goals=%d\n%s", state, got.Conclusion, goals, dh.logs.String())
			}
			if h.remote.posts.Load() != 0 || h.remote.gets.Load() != 0 {
				t.Fatal("local goals called GitHub code scanning")
			}
		})
	}
}

func TestSecurityReportConsentAndSourceAreRecheckedBeforeGoals(t *testing.T) {
	h := newSARIFHarness(t)
	setSecurityReportBody(h, securityFixtureSARIF)
	enabled, goals := false, 0
	h.i.Configure(func(d *Deps) {
		d.SystemVariable = func(context.Context, string) (string, error) {
			if enabled {
				return `{"enabled":true}`, nil
			}
			return `{"enabled":false}`, nil
		}
		d.OpenSecurityGoal = func(context.Context, SecurityGoalRequest) (string, error) { goals++; return "goal", nil }
	})
	ctx := personCtx(h.owner)
	fence := func(context.Context) error { return nil }
	read := func() (*securityReportScope, error) {
		return h.i.readSecurityReport(ctx, h.run.ID, "analysis.go", ".reports/go.sarif", fence)
	}
	if _, err := read(); err == nil || h.library.opens.Load() != 0 {
		t.Fatal("disabled scan read report bytes")
	}
	enabled = true
	report, err := read()
	if err != nil || len(report.findings) != 1 {
		t.Fatal(report, err)
	}
	fingerprint := report.findings[0].Fingerprint
	if _, err := h.i.openSecurityFinding(ctx, report, "forged", "Resolve this"); err == nil {
		t.Fatal("accepted an unverified finding")
	}
	enabled = false
	if _, err := h.i.openSecurityFinding(ctx, report, fingerprint, "Resolve this"); err == nil || goals != 0 {
		t.Fatal("disabled in-flight scan created a goal")
	}
	enabled = true
	h.remote.fakeGitHub.tokenErr = errors.New("revoked")
	if _, err := h.i.openSecurityFinding(ctx, report, fingerprint, "Resolve this"); err == nil || goals != 0 {
		t.Fatal("revoked source created a goal")
	}
	h.remote.fakeGitHub.tokenErr = nil
	if _, err := h.i.openSecurityFinding(ctx, report, fingerprint, "Resolve this"); err != nil || goals != 1 {
		t.Fatal("enabled finding failed", err, goals)
	}
	// The test leaves its setting off, matching the shipped default.
	enabled = false
	if h.i.snapshot().securitySettings(ctx).Enabled {
		t.Fatal("security consent remained enabled")
	}
}
