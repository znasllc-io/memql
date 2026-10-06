package pipelinesteps

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// Readiness reports this runner's last isolation proof without creating
// anything. A cached pass is useful only within the same TTL the run gate uses.
func (r *Runner) Readiness() pl.RunnerReadiness {
	if err := r.cfg.ValidatePlacement(); err != nil {
		return pl.RunnerReadiness{NodeID: r.cfg.NodeID, Namespace: r.cfg.Namespace, Isolation: "not_proven", Detail: err.Error()}
	}
	v := r.Isolation()
	report := pl.RunnerReadiness{NodeID: r.cfg.NodeID, Namespace: r.cfg.Namespace,
		Available: true, Isolation: "not_proven", Detail: "Isolation has not been proven on this replica. The first step must prove it before starting."}
	if v.At.IsZero() {
		return report
	}
	report.CheckedAt = v.At.UTC().Format(time.RFC3339Nano)
	report.Detail = v.Detail
	switch {
	case v.Inconclusive:
		report.Isolation = "inconclusive"
	case !v.Isolated:
		report.Isolation = "failed"
	case r.now().Before(v.At) || !r.now().Before(v.At.Add(r.cfg.IsolationTTL)):
		report.Isolation = "expired"
	default:
		report.Isolation = "passed"
	}
	if v.Isolated {
		report.ValidUntil = v.At.Add(r.cfg.IsolationTTL).UTC().Format(time.RFC3339Nano)
	}
	return report
}

// Readiness asks every known workbench separately, in one bounded interval.
// Forward's pin is a preference: reject a substituted answer so a lost replica
// cannot borrow another replica's proof and appear healthy.
func (e *Executor) Readiness(ctx context.Context) []pl.RunnerReadiness {
	inventory, ok := e.fwd.(interface{ WorkbenchNodeIDs() []string })
	if !ok {
		return nil
	}
	nodes := inventory.WorkbenchNodeIDs()
	sort.Strings(nodes)
	reports := make([]pl.RunnerReadiness, len(nodes))
	ctx, cancel := context.WithTimeout(ctx, workbenchCallTimeout)
	defer cancel()
	var wg sync.WaitGroup
	// Bound the number of outstanding forwards even in a large cluster.
	slots := make(chan struct{}, 8)
	for index, nodeID := range nodes {
		reports[index] = pl.RunnerReadiness{NodeID: nodeID, Isolation: "unknown", Detail: "This workbench did not answer its readiness request."}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			req, err := e.request(workbench.PipelineReadinessAction, "", "", nil, 0)
			if err != nil {
				return
			}
			reply, servedBy, err := e.fwd.Forward(ctx, req, nodeID)
			if err != nil || servedBy != nodeID || reply == nil {
				return
			}
			if reply.ErrorCode == workbench.ErrCodePipelinesNotConfigured {
				reports[index].Detail = "This workbench has no pipeline runner. Its startup log explains the missing configuration."
				return
			}
			if reply.ErrorCode != "" {
				return
			}
			var report pl.RunnerReadiness
			if json.Unmarshal(reply.PayloadJson, &report) != nil || report.NodeID != nodeID {
				return
			}
			reports[index] = report
		}()
	}
	wg.Wait()
	return reports
}
