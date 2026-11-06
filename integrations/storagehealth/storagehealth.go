// Package storagehealth reads the installation's existing database monitor.
// It does not probe Postgres or write another health history into it.
package storagehealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/num"
)

type clusterAPI interface {
	Do(context.Context, string, string, string, []byte) ([]byte, error)
}
type Integration struct {
	api       clusterAPI
	namespace string
}

func init() {
	memql.RegisterPlugin("storageHealth", func(memql.PluginContext) (memql.IntegrationProvider, error) {
		return New(), nil
	})
}

// New builds the reader the plug-in registers: the in-cluster Kubernetes client
// and this pod's namespace, or neither, in which case every read answers that
// monitoring is unavailable. Exported for the closed-chunk compaction's room
// check (component/retention/compact.go), which reads the same report natively
// behind its own cluster-owner gate rather than through the builtin.
func New() *Integration {
	api, err := deploycontrol.NewClusterAPI()
	namespace, readErr := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	i := &Integration{namespace: strings.TrimSpace(string(namespace))}
	if err == nil && readErr == nil && i.namespace != "" {
		i.api = api
	}
	return i
}

// Reading is a database storage report the monitor published, verified as the
// builtin verifies it: its source, the current database installation and
// complete measurements.
type Reading struct {
	ObservedAt        time.Time
	DataBytes         float64
	DataCapacityBytes float64
	WALBytes          float64
	WALCapacityBytes  float64
	Issues            []string
}

// Read returns the current report, or an error carrying the reason there is
// none. It does not judge the report's age: what is fresh enough is the
// caller's decision, as it is the OS's (fifteen minutes there).
func (i *Integration) Read(ctx context.Context) (Reading, error) {
	r := i.readReport(ctx)
	if !r.Available {
		return Reading{}, errors.New(r.Reason)
	}
	// A report measures seconds since the epoch as a float; a value past
	// int64 saturates rather than wrapping (core/num), and the date it then
	// names is far in the future, which no freshness rule accepts.
	seconds, fraction := math.Modf(r.ObservedAt)
	return Reading{
		ObservedAt:        time.Unix(num.ClampFloat64ToInt64(seconds), num.ClampFloat64ToInt64(fraction*1e9)).UTC(),
		DataBytes:         *r.DataBytes,
		DataCapacityBytes: *r.DataCapacityBytes,
		WALBytes:          *r.WALBytes,
		WALCapacityBytes:  *r.WALCapacityBytes,
		Issues:            append([]string(nil), r.Issues...),
	}, nil
}

func (*Integration) IntegrationName() string { return "storageHealth" }
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{{Name: "read", Description: "Read this installation's database storage report. Cluster owner only.", Handler: i.read}}
}

type report struct {
	Available         bool     `json:"available"`
	Reason            string   `json:"reason,omitempty"`
	ObservedAt        float64  `json:"observedAt,omitempty"`
	DataBytes         *float64 `json:"dataBytes,omitempty"`
	DataCapacityBytes *float64 `json:"dataCapacityBytes,omitempty"`
	WALBytes          *float64 `json:"walBytes,omitempty"`
	WALCapacityBytes  *float64 `json:"walCapacityBytes,omitempty"`
	Issues            []string `json:"issues,omitempty"`
}

func (i *Integration) read(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	actor, ok := auth.AccessFromContext(ctx)
	if !ok || actor == nil || !actor.IsClusterOwner() {
		return nil, fmt.Errorf("storage reports require a cluster owner")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r := i.readReport(ctx)
	payload, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: "clusterStorageStatus", Concept: "integration:storageHealth:read", Type: "object", CreatedAt: time.Now().UTC(), Payload: payload}}, nil
}

func (i *Integration) readReport(ctx context.Context) report {
	unavailable := func(reason string) report { return report{Reason: reason} }
	if i.api == nil {
		return unavailable("Storage monitoring is unavailable on this node.")
	}
	ns := url.PathEscape(i.namespace)
	raw, err := i.api.Do(ctx, http.MethodGet, "api/v1/namespaces/"+ns+"/events/database-health", "", nil)
	if err != nil {
		return unavailable("The database storage report could not be read.")
	}
	var event struct {
		Source         struct{ Component string }
		InvolvedObject struct{ APIVersion, Kind, Name, Namespace, UID string }
		Message        string
	}
	if json.Unmarshal(raw, &event) != nil || event.Source.Component != "database-health" || event.InvolvedObject.APIVersion != "postgresql.cnpg.io/v1" || event.InvolvedObject.Kind != "Cluster" || event.InvolvedObject.Name != "memql-db" || event.InvolvedObject.Namespace != i.namespace || event.InvolvedObject.UID == "" {
		return unavailable("The database storage report is invalid.")
	}
	// Bind the report to the current database installation, including recreation.
	raw, err = i.api.Do(ctx, http.MethodGet, "apis/postgresql.cnpg.io/v1/namespaces/"+ns+"/clusters/memql-db", "", nil)
	if err != nil {
		return unavailable("The database report's source could not be verified.")
	}
	var cluster struct{ Metadata struct{ UID string } }
	if json.Unmarshal(raw, &cluster) != nil || cluster.Metadata.UID != event.InvolvedObject.UID {
		return unavailable("The storage report belongs to an earlier database installation.")
	}
	var r report
	if json.Unmarshal([]byte(event.Message), &r) != nil || r.ObservedAt <= 0 || math.IsNaN(r.ObservedAt) || math.IsInf(r.ObservedAt, 0) {
		return unavailable("The database storage report is incomplete.")
	}
	for _, pair := range [][2]*float64{{r.DataBytes, r.DataCapacityBytes}, {r.WALBytes, r.WALCapacityBytes}} {
		if pair[0] == nil || pair[1] == nil || *pair[0] < 0 || *pair[1] <= 0 {
			return unavailable("Storage measurements are missing from the database report.")
		}
	}
	r.Available, r.Reason = true, ""
	return r
}
