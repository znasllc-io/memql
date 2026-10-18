// Package backgroundcapacity exposes the operator's bounded batch budget.
// Kubernetes owns reservations across replicas; this integration only reads
// and edits one queue under a separately managed, immutable-to-engine ceiling.
package backgroundcapacity

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"
)

const queuePath = "apis/kueue.x-k8s.io/v1beta2/clusterqueues/memql-background"
const policyPath = "apis/capacity.memql.io/v1alpha1/backgroundcapacitypolicies/memql-background-capacity"
const flavorPath = "apis/kueue.x-k8s.io/v1beta2/resourceflavors/memql-background"

type clusterAPI interface {
	Do(context.Context, string, string, string, []byte) ([]byte, error)
}
type Integration struct {
	api clusterAPI
}

func init() {
	memql.RegisterPlugin("backgroundCapacity", func(memql.PluginContext) (memql.IntegrationProvider, error) {
		api, err := deploycontrol.NewClusterAPI()
		i := &Integration{}
		if err == nil {
			i.api = api
		}
		return i, nil
	})
}
func (*Integration) IntegrationName() string { return "backgroundCapacity" }
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{Name: "read", Description: "Read this installation's background resource budget. Cluster owner only.", Handler: i.read},
		{Name: "set", Description: "Adjust background admission within the operator's resource ceiling. Does not resize machines or stop running work. Cluster owner only.", Handler: i.set},
	}
}

type metadata struct {
	UID, ResourceVersion string
	Generation           int64
	Annotations          map[string]string
}
type capacityPolicy struct {
	Metadata metadata
	Spec     struct {
		Budget
		NodePool string
	}
}
type resource struct{ Name, NominalQuota string }
type flavor struct {
	Name      string
	Resources []resource
}
type resourceGroup struct {
	CoveredResources []string
	Flavors          []flavor
}
type queue struct {
	Metadata metadata
	Spec     struct {
		NamespaceSelector struct {
			MatchLabels      map[string]string
			MatchExpressions []json.RawMessage
		}
		ResourceGroups []resourceGroup
		StopPolicy     string
		CohortName     string
		Preemption     struct {
			WithinClusterQueue, ReclaimWithinCohort string
			BorrowWithinCohort                      struct{ Policy string }
		}
	}
	Status *struct {
		Conditions []struct {
			Type, Status       string
			ObservedGeneration int64
		}
		PendingWorkloads   int64
		AdmittedWorkloads  int64
		FlavorsReservation []struct {
			Name      string
			Resources []struct{ Name, Total string }
		}
	}
}

type Budget struct {
	CPUMilli  int64 `json:"cpuMilli"`
	MemoryMiB int64 `json:"memoryMiB"`
	DiskMiB   int64 `json:"diskMiB"`
	Jobs      int64 `json:"jobs"`
}
type report struct {
	Available      bool    `json:"available"`
	Reason         string  `json:"reason,omitempty"`
	ObservedAt     string  `json:"observedAt"`
	Revision       string  `json:"revision,omitempty"`
	PolicyRevision string  `json:"policyRevision,omitempty"`
	Ceiling        *Budget `json:"ceiling,omitempty"`
	Budget         *Budget `json:"budget,omitempty"`
	Reserved       *Budget `json:"reserved,omitempty"`
	Accepting      bool    `json:"accepting"`
	Waiting        *int64  `json:"waiting,omitempty"`
	Running        *int64  `json:"running,omitempty"`
}

func authorize(ctx context.Context) error {
	actor, ok := auth.AccessFromContext(ctx)
	if !ok || actor == nil || !actor.IsClusterOwner() {
		return fmt.Errorf("background capacity requires a cluster owner")
	}
	return nil
}
func result(r report) ([]memorynodes.MemoryNode, error) {
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "clusterBackgroundCapacity", Concept: "integration:backgroundCapacity:read", Type: "object", CreatedAt: time.Now().UTC(), Payload: payload}}, nil
}
func (i *Integration) read(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, _, _, err := i.snapshot(ctx)
	if err != nil {
		return result(report{Reason: err.Error(), ObservedAt: time.Now().UTC().Format(time.RFC3339)})
	}
	return result(r)
}

func (i *Integration) snapshot(ctx context.Context) (report, capacityPolicy, queue, error) {
	var c capacityPolicy
	var q queue
	r := report{ObservedAt: time.Now().UTC().Format(time.RFC3339)}
	fail := func(s string) (report, capacityPolicy, queue, error) { return r, c, q, fmt.Errorf("%s", s) }
	if i.api == nil {
		return fail("Background resource admission is not configured on this node.")
	}
	raw, err := i.api.Do(ctx, http.MethodGet, policyPath, "", nil)
	if err != nil || json.Unmarshal(raw, &c) != nil || c.Metadata.UID == "" || c.Metadata.ResourceVersion == "" {
		return fail("The operator's background capacity ceiling could not be read.")
	}
	cap, err := policyBudget(c)
	if err != nil {
		return fail("The operator's background capacity ceiling is invalid.")
	}
	raw, err = i.api.Do(ctx, http.MethodGet, queuePath, "", nil)
	if err != nil || json.Unmarshal(raw, &q) != nil || q.Metadata.UID == "" || q.Metadata.ResourceVersion == "" {
		return fail("The background admission queue could not be read.")
	}
	budget, err := queueBudget(q)
	if err != nil || !within(budget, cap) {
		return fail("The background queue does not match its protected resource budget.")
	}
	raw, err = i.api.Do(ctx, http.MethodGet, flavorPath, "", nil)
	if err != nil || !dedicatedPool(raw, c.Spec.NodePool) {
		return fail("The queue's dedicated background pool could not be verified.")
	}
	reserved := Budget{}
	statusCurrent := false
	if q.Status != nil {
		for _, condition := range q.Status.Conditions {
			if condition.Type == "Active" && condition.ObservedGeneration >= q.Metadata.Generation && (condition.Status == "True" || q.Spec.StopPolicy == "Hold") {
				statusCurrent = true
			}
		}
	}
	if statusCurrent {
		if len(q.Status.FlavorsReservation) > 1 {
			return fail("The background reservation report is invalid.")
		}
		for _, f := range q.Status.FlavorsReservation {
			if f.Name != "memql-background" {
				return fail("The background reservation report names an unexpected pool.")
			}
			for _, resource := range f.Resources {
				if err := assignResource(&reserved, resource.Name, resource.Total); err != nil {
					return fail("The background reservation report is invalid.")
				}
			}
		}
		if q.Status.PendingWorkloads < 0 || q.Status.AdmittedWorkloads < 0 {
			return fail("The background queue counts are invalid.")
		}
		r.Reserved, r.Waiting, r.Running = &reserved, &q.Status.PendingWorkloads, &q.Status.AdmittedWorkloads
	}
	r.Available, r.Ceiling, r.Budget = true, &cap, &budget
	encoded, _ := json.Marshal(q.Spec)
	r.Revision, r.PolicyRevision = fmt.Sprintf("%s:%s", q.Metadata.UID, id.New().FromString(string(encoded))), c.Metadata.UID+":"+c.Metadata.ResourceVersion
	r.Accepting = q.Spec.StopPolicy == "" || q.Spec.StopPolicy == "None"
	return r, c, q, nil
}

func policyBudget(c capacityPolicy) (Budget, error) {
	b := c.Spec.Budget
	for _, n := range []int64{b.CPUMilli, b.MemoryMiB, b.DiskMiB, b.Jobs} {
		if n < 1 || n > 1<<40 {
			return b, fmt.Errorf("invalid ceiling")
		}
	}
	return b, nil
}
func queueBudget(q queue) (Budget, error) {
	var b Budget
	selector := q.Spec.NamespaceSelector
	if len(selector.MatchLabels) != 1 || selector.MatchLabels["kubernetes.io/metadata.name"] != "memql-pipelines" || len(selector.MatchExpressions) != 0 {
		return b, fmt.Errorf("unexpected queue namespace")
	}
	if len(q.Spec.ResourceGroups) != 1 || len(q.Spec.ResourceGroups[0].Flavors) != 1 || q.Spec.ResourceGroups[0].Flavors[0].Name != "memql-background" || q.Spec.CohortName != "" {
		return b, fmt.Errorf("unexpected resource pool")
	}
	if q.Spec.StopPolicy != "" && q.Spec.StopPolicy != "None" && q.Spec.StopPolicy != "Hold" {
		return b, fmt.Errorf("unexpected stop policy")
	}
	p := q.Spec.Preemption
	for _, value := range []string{p.WithinClusterQueue, p.ReclaimWithinCohort, p.BorrowWithinCohort.Policy} {
		if value != "" && value != "Never" {
			return b, fmt.Errorf("preemption is not permitted")
		}
	}
	seen := map[string]bool{}
	for _, r := range q.Spec.ResourceGroups[0].Flavors[0].Resources {
		if seen[r.Name] {
			return b, fmt.Errorf("duplicate resource")
		}
		seen[r.Name] = true
		if err := assignResource(&b, r.Name, r.NominalQuota); err != nil {
			return b, err
		}
	}
	if len(seen) != 4 || b.CPUMilli < 1 || b.MemoryMiB < 1 || b.DiskMiB < 1 || b.Jobs < 1 {
		return b, fmt.Errorf("incomplete resource budget")
	}
	covered := map[string]bool{}
	for _, name := range q.Spec.ResourceGroups[0].CoveredResources {
		if !seen[name] || covered[name] {
			return b, fmt.Errorf("unexpected covered resources")
		}
		covered[name] = true
	}
	if len(covered) != 4 {
		return b, fmt.Errorf("incomplete covered resources")
	}
	return b, nil
}
func within(b, max Budget) bool {
	return b.CPUMilli <= max.CPUMilli && b.MemoryMiB <= max.MemoryMiB && b.DiskMiB <= max.DiskMiB && b.Jobs <= max.Jobs
}

// Decode exact quantities without float rounding or a Kubernetes client SDK.
// Values are rounded upward to the displayed unit; overflow refuses the read.
func quantity(raw string, unit int64) (int64, error) {
	if len(raw) == 0 || len(raw) > 64 {
		return 0, fmt.Errorf("invalid quantity length")
	}
	value, scale := raw, int64(1)
	for _, suffix := range []struct {
		text  string
		scale int64
	}{{"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10}, {"G", 1000000000}, {"M", 1000000}, {"k", 1000}} {
		if strings.HasSuffix(value, suffix.text) {
			value = strings.TrimSuffix(value, suffix.text)
			scale = suffix.scale
			break
		}
	}
	r, ok := new(big.Rat).SetString(value)
	if !ok || r.Sign() < 0 || unit < 1 {
		return 0, fmt.Errorf("invalid quantity")
	}
	r.Mul(r, big.NewRat(scale, unit))
	n, rem := new(big.Int), new(big.Int)
	n.QuoRem(r.Num(), r.Denom(), rem)
	if rem.Sign() > 0 {
		n.Add(n, big.NewInt(1))
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("quantity overflow")
	}
	return n.Int64(), nil
}
func assignResource(b *Budget, name, value string) error {
	if len(value) == 0 || len(value) > 32 || strings.ContainsAny(value, "/eE") {
		return fmt.Errorf("invalid resource quantity")
	}
	var target *int64
	unit := int64(1)
	switch name {
	case "cpu":
		target = &b.CPUMilli
		if strings.HasSuffix(value, "m") {
			value = strings.TrimSuffix(value, "m")
		} else {
			r, ok := new(big.Rat).SetString(value)
			if !ok {
				return fmt.Errorf("invalid CPU")
			}
			value = r.Mul(r, big.NewRat(1000, 1)).RatString()
		}
	case "memory":
		target, unit = &b.MemoryMiB, 1<<20
	case "ephemeral-storage":
		target, unit = &b.DiskMiB, 1<<20
	case "pods":
		target = &b.Jobs
	default:
		return fmt.Errorf("unexpected resource")
	}
	n, err := quantity(value, unit)
	if err != nil {
		return err
	}
	*target = n
	return nil
}

func dedicatedPool(raw []byte, name string) bool {
	var f struct {
		Spec struct {
			NodeLabels map[string]string
			NodeTaints []struct{ Key, Value, Effect string }
		}
	}
	if name == "" || json.Unmarshal(raw, &f) != nil || f.Spec.NodeLabels["memql.io/pipeline-pool"] != name {
		return false
	}
	for _, t := range f.Spec.NodeTaints {
		if t.Key == "memql.io/pipeline-pool" && t.Value == name && t.Effect == "NoSchedule" {
			return true
		}
	}
	return false
}

func (i *Integration) set(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, _, q, err := i.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if args["revision"] != r.Revision || args["policyRevision"] != r.PolicyRevision {
		return nil, fmt.Errorf("Capacity changed since this view was loaded. Refresh it before saving.")
	}
	b := *r.Budget
	for key, target := range map[string]*int64{"cpuMilli": &b.CPUMilli, "memoryMiB": &b.MemoryMiB, "jobs": &b.Jobs} {
		v, ok := whole(args[key])
		if !ok || v < 1 || v > 1<<40 {
			return nil, fmt.Errorf("%s must be a positive whole number", key)
		}
		*target = v
	}
	accepting, ok := args["accepting"].(bool)
	if !ok {
		return nil, fmt.Errorf("Choose whether to accept new background work.")
	}
	if !within(b, *r.Ceiling) {
		return nil, fmt.Errorf("That allocation exceeds the protected background capacity ceiling. No change was made.")
	}
	stop := "None"
	if !accepting {
		stop = "Hold"
	}
	actor, _ := auth.AccessFromContext(ctx)
	audit, _ := json.Marshal(map[string]any{"actor": actor.UserId, "at": time.Now().UTC().Format(time.RFC3339Nano), "before": r.Budget, "after": b, "accepting": accepting})
	annotations := q.Metadata.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["memql.io/capacity-change"] = string(audit)
	resources := []map[string]string{{"name": "cpu", "nominalQuota": strconv.FormatInt(b.CPUMilli, 10) + "m"}, {"name": "memory", "nominalQuota": strconv.FormatInt(b.MemoryMiB, 10) + "Mi"}, {"name": "ephemeral-storage", "nominalQuota": strconv.FormatInt(b.DiskMiB, 10) + "Mi"}, {"name": "pods", "nominalQuota": strconv.FormatInt(b.Jobs, 10)}}
	patch, _ := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": q.Metadata.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": q.Metadata.ResourceVersion},
		{"op": "replace", "path": "/spec/resourceGroups/0/flavors/0/resources", "value": resources},
		{"op": "add", "path": "/spec/stopPolicy", "value": stop},
		{"op": "add", "path": "/metadata/annotations", "value": annotations},
	})
	if _, err := i.api.Do(ctx, http.MethodPatch, queuePath, "application/json-patch+json", patch); err != nil {
		return nil, fmt.Errorf("The capacity change was not confirmed. Refresh the current values before trying again: %w", err)
	}
	// Never claim success from a write acknowledgement alone. A disconnected
	// read leaves the UI unresolved; it must read again, never repeat the write.
	updated, _, _, err := i.snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("The change was accepted, but its current state could not be verified: %w", err)
	}
	if updated.Budget == nil || *updated.Budget != b || updated.Accepting != accepting {
		return nil, fmt.Errorf("The current allocation differs from the requested change. Refresh the current values before saving again.")
	}
	return result(updated)
}

func whole(raw any) (int64, bool) {
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		return num.WholeInt64(v)
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}
