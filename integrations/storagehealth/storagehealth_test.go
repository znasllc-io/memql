package storagehealth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/deploycontrol"
)

func TestStorageReportOwnerGateAndIndependentReplicaReads(t *testing.T) {
	calls := 0
	usage := 81.0
	uid := "current"
	malformed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, http.MethodGet, r.Method)
		switch r.URL.Path {
		case "/api/v1/namespaces/memql/events/database-health":
			message, _ := json.Marshal(map[string]any{"observedAt": time.Now().Unix(), "dataBytes": usage, "dataCapacityBytes": 100, "walBytes": 10, "walCapacityBytes": 100, "issues": []string{"data-volume-over-80-percent"}})
			if malformed {
				message = []byte(`{"observedAt":1}`)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"source": map[string]string{"component": "database-health"}, "involvedObject": map[string]string{"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "name": "memql-db", "namespace": "memql", "uid": "current"}, "message": string(message)})
		case "/apis/postgresql.cnpg.io/v1/namespaces/memql/clusters/memql-db":
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]string{"uid": uid}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	first := &Integration{api: deploycontrol.NewClusterAPIWith(server.URL, "test", server.Client()), namespace: "memql"}
	second := &Integration{api: deploycontrol.NewClusterAPIWith(server.URL, "test", server.Client()), namespace: "memql"}
	for _, role := range []auth.Role{"", auth.RoleAdmin, auth.RoleDeveloper, auth.RoleReader} {
		_, err := first.read(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "reader", Role: role}), nil, 0)
		require.Error(t, err)
	}
	require.Zero(t, calls, "unauthorized reads must not contact Kubernetes")
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner", Role: auth.RoleOwner})
	rows, err := first.read(ctx, nil, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var result report
	require.NoError(t, json.Unmarshal(rows[0].Payload, &result))
	require.True(t, result.Available)
	require.Equal(t, 81.0, *result.DataBytes)
	usage = 90
	rows, err = second.read(ctx, nil, 0)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(rows[0].Payload, &result))
	require.Equal(t, 90.0, *result.DataBytes, "another replica reads the shared monitor, not process-local evidence")
	uid = "replacement"
	require.False(t, first.readReport(ctx).Available)
	uid = "current"
	malformed = true
	require.False(t, first.readReport(ctx).Available, "missing measurements cannot become zero usage")
}

func TestStorageReportMissingAndUnavailable(t *testing.T) {
	require.False(t, (&Integration{}).readReport(context.Background()).Available)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "not found", 404) }))
	defer server.Close()
	i := &Integration{api: deploycontrol.NewClusterAPIWith(server.URL, "test", server.Client()), namespace: "memql"}
	got := i.readReport(context.Background())
	require.False(t, got.Available)
	require.True(t, strings.Contains(got.Reason, "could not be read"))
}
