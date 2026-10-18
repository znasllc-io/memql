package backgroundcapacity

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

const queueFixture = `{"metadata":{"uid":"queue-1","resourceVersion":"42","generation":3},"spec":{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"memql-pipelines"}},"resourceGroups":[{"coveredResources":["cpu","memory","ephemeral-storage","pods"],"flavors":[{"name":"memql-background","resources":[{"name":"cpu","nominalQuota":"6.5"},{"name":"memory","nominalQuota":"22Gi"},{"name":"ephemeral-storage","nominalQuota":"80Gi"},{"name":"pods","nominalQuota":"6"}]}]}],"stopPolicy":"None","preemption":{"withinClusterQueue":"Never","reclaimWithinCohort":"Never","borrowWithinCohort":{"policy":"Never"}}},"status":{"conditions":[{"type":"Active","status":"True","observedGeneration":3}],"pendingWorkloads":2,"admittedWorkloads":1,"flavorsReservation":[{"name":"memql-background","resources":[{"name":"cpu","total":"260m"},{"name":"memory","total":"4160Mi"},{"name":"ephemeral-storage","total":"20Gi"},{"name":"pods","total":"1"}]}]}}`
const policyFixture = `{"metadata":{"uid":"policy-1","resourceVersion":"7"},"spec":{"cpuMilli":6500,"memoryMiB":22528,"diskMiB":81920,"jobs":6,"nodePool":"builds"}}`
const flavorFixture = `{"spec":{"nodeLabels":{"memql.io/pipeline-pool":"builds"},"nodeTaints":[{"key":"memql.io/pipeline-pool","value":"builds","effect":"NoSchedule"}]}}`

type fakeAPI struct {
	queue, policy, flavor []byte
	calls                 int
	patches               [][]byte
	patchErr              error
	afterWrite            bool
}

func fixture() (*Integration, *fakeAPI) {
	api := &fakeAPI{queue: []byte(queueFixture), policy: []byte(policyFixture), flavor: []byte(flavorFixture)}
	return &Integration{api: api}, api
}
func (a *fakeAPI) Do(_ context.Context, method, path, contentType string, body []byte) ([]byte, error) {
	a.calls++
	if method == http.MethodPatch {
		if path != queuePath || contentType != "application/json-patch+json" {
			return nil, fmt.Errorf("unexpected write")
		}
		a.patches = append(a.patches, body)
		if a.patchErr != nil {
			return nil, a.patchErr
		}
		a.afterWrite = true
		return []byte(`{}`), nil
	}
	switch path {
	case queuePath:
		return a.queue, nil
	case flavorPath:
		return a.flavor, nil
	case policyPath:
		return a.policy, nil
	}
	return nil, fmt.Errorf("unexpected read %s", path)
}
func owner() context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{Role: auth.RoleOwner, UserId: "test-owner"})
}
func args(r report) map[string]any {
	return map[string]any{"revision": r.Revision, "policyRevision": r.PolicyRevision, "cpuMilli": int64(5000), "memoryMiB": int64(16384), "jobs": int64(4), "accepting": false}
}

func TestOnlyOwnerMayReadOrChangeCapacity(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleDeveloper, auth.RoleWriter, auth.RoleReader} {
		i, a := fixture()
		ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{Role: role})
		_, err := i.read(ctx, nil, 0)
		require.Error(t, err)
		_, err = i.set(ctx, nil, 0)
		require.Error(t, err)
		require.Zero(t, a.calls, "deny before touching Kubernetes")
	}
}
func TestCapacityReadsReservationsAndRejectsUnverifiedIsolation(t *testing.T) {
	i, a := fixture()
	r, _, _, err := i.snapshot(owner())
	require.NoError(t, err)
	require.Equal(t, Budget{6500, 22528, 81920, 6}, *r.Budget)
	require.Equal(t, Budget{260, 4160, 20480, 1}, *r.Reserved)
	require.EqualValues(t, 2, *r.Waiting)
	for _, broken := range []string{`{}`, strings.ReplaceAll(flavorFixture, "builds", "serving"), strings.ReplaceAll(flavorFixture, "NoSchedule", "PreferNoSchedule")} {
		a.flavor = []byte(broken)
		_, _, _, err = i.snapshot(owner())
		require.ErrorContains(t, err, "dedicated")
	}
}
func TestUnavailableOrOldStatusIsUnknownRatherThanZero(t *testing.T) {
	i, a := fixture()
	a.queue = []byte(strings.Replace(queueFixture, `"observedGeneration":3`, `"observedGeneration":2`, 1))
	r, _, _, err := i.snapshot(owner())
	require.NoError(t, err)
	require.Nil(t, r.Reserved)
	require.Nil(t, r.Running)
	a.queue = []byte(strings.Replace(queueFixture, `"status":"True"`, `"status":"False"`, 1))
	r, _, _, err = i.snapshot(owner())
	require.NoError(t, err)
	require.Nil(t, r.Reserved)
	a.queue = []byte(`{}`)
	nodes, err := i.read(owner(), nil, 0)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(nodes[0].Payload, &r))
	require.False(t, r.Available)
}
func TestCapacityCannotBorrowFromACohort(t *testing.T) {
	i, a := fixture()
	a.queue = []byte(strings.Replace(queueFixture, `"spec":{`, `"spec":{"cohortName":"customer-services",`, 1))
	_, _, _, err := i.snapshot(owner())
	require.ErrorContains(t, err, "protected resource budget")
	_, err = i.set(owner(), map[string]any{}, 0)
	require.Error(t, err)
	require.Empty(t, a.patches)
}
func TestRevisionIgnoresStatusButTracksSettings(t *testing.T) {
	i, a := fixture()
	first, _, _, err := i.snapshot(owner())
	require.NoError(t, err)
	a.queue = []byte(strings.ReplaceAll(strings.ReplaceAll(queueFixture, `"42"`, `"43"`), `"pendingWorkloads":2`, `"pendingWorkloads":3`))
	next, _, _, err := i.snapshot(owner())
	require.NoError(t, err)
	require.Equal(t, first.Revision, next.Revision)
	a.queue = []byte(strings.Replace(queueFixture, `"stopPolicy":"None"`, `"stopPolicy":"Hold"`, 1))
	next, _, _, err = i.snapshot(owner())
	require.NoError(t, err)
	require.NotEqual(t, first.Revision, next.Revision)
}
func TestStaleOrExcessiveCapacityMakesNoWrite(t *testing.T) {
	for _, key := range []string{"revision", "policyRevision", "cpuMilli", "memoryMiB", "jobs"} {
		i, a := fixture()
		r, _, _, err := i.snapshot(owner())
		require.NoError(t, err)
		v := args(r)
		if strings.HasSuffix(key, "evision") {
			v[key] = "stale"
		} else {
			v[key] = int64(1 << 39)
		}
		_, err = i.set(owner(), v, 0)
		require.Error(t, err)
		require.Empty(t, a.patches)
	}
}
func TestWritesAreConditionalAndNeverRetried(t *testing.T) {
	i, a := fixture()
	r, _, _, err := i.snapshot(owner())
	require.NoError(t, err)
	a.patchErr = fmt.Errorf("connection lost after send")
	_, err = i.set(owner(), args(r), 0)
	require.ErrorContains(t, err, "not confirmed")
	require.Len(t, a.patches, 1)
	var patch []struct {
		Op, Path string
		Value    json.RawMessage
	}
	require.NoError(t, json.Unmarshal(a.patches[0], &patch))
	require.Equal(t, "test", patch[0].Op)
	require.Equal(t, "/metadata/uid", patch[0].Path)
	require.Equal(t, "test", patch[1].Op)
	require.Equal(t, "/metadata/resourceVersion", patch[1].Path)
	require.Contains(t, string(a.patches[0]), "test-owner")
	require.Contains(t, string(a.patches[0]), "Hold")
}

func TestSuccessfulWriteAcknowledgementStillRequiresMatchingReadback(t *testing.T) {
	i, a := fixture()
	r, _, _, err := i.snapshot(owner())
	require.NoError(t, err)
	_, err = i.set(owner(), args(r), 0)
	require.ErrorContains(t, err, "differs from the requested change")
	require.Len(t, a.patches, 1)
}
func TestQuantitiesRefuseMalformedAndOverflow(t *testing.T) {
	for _, v := range []string{"", "-1Gi", "NaN", "Inf", "99999999999999999999Ti", strings.Repeat("9", 100)} {
		_, err := quantity(v, 1)
		require.Error(t, err, v)
	}
	for _, v := range []any{math.NaN(), math.Inf(1), 1.5, "5"} {
		_, ok := whole(v)
		require.False(t, ok)
	}
	for _, pair := range []struct {
		raw        string
		unit, want int64
	}{{"1.5Gi", 1 << 20, 1536}, {"0", 1, 0}, {"1Ki", 1 << 20, 1}} {
		got, err := quantity(pair.raw, pair.unit)
		require.NoError(t, err)
		require.Equal(t, pair.want, got)
	}
}
