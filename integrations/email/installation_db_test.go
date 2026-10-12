package email

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

func TestEmailInstallationResumesOnIndependentReplica(t *testing.T) {
	t.Setenv("MEMQL_MASTER_KEY", strings.Repeat("ac", 32))
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "email installation", dbtest.DSN(), err)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	ctx := emailStateContext(context.Background())
	prefix := fmt.Sprintf("%s:%d", t.Name(), time.Now().UnixNano())
	instances := make([]*azureSetup, 2)
	for n := range instances {
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		if err = e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		instances[n] = &azureSetup{store: &connectionStore{engine: installationTestStore{ConfigWriter: NewConfigWriter(e), prefix: prefix}, gateDB: replicaGatePool(t)}, installation: installationFixture(), readAssertion: func(string) ([]byte, error) { return []byte("pod.projected.signature"), nil }}
	}
	created := map[string]bool{}
	clusterID := func() string {
		var c azureClusterConnection
		if _, _, err := instances[0].store.read(ctx, azureClusterKey, &c); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	protocol := installationProtocol(t, created, clusterID)
	for _, a := range instances {
		a.protocol = protocol
	}
	for attempt := 0; attempt < 4; attempt++ {
		err := instances[attempt%2].bootstrapInstallation(ctx)
		if attempt < 2 && (err == nil || !strings.Contains(err.Error(), "preparing")) {
			t.Fatalf("create %d must remain pending: %v", attempt, err)
		}
		if attempt >= 2 && err != nil {
			t.Fatalf("resume %d: %v", attempt, err)
		}
	}
	var c azureConnection
	if _, _, err := instances[1].store.read(ctx, "organization:self", &c); err != nil {
		t.Fatal(err)
	}
	if c.Status != "ready" || c.SenderIdentityID == "" || !strings.HasSuffix(c.Config.Default, ".azurecomm.net") {
		t.Fatalf("installation did not finish: %+v", c)
	}
	// The scoped sender identity is an ordinary campaign identity, and a replica
	// can resolve its transport without the creator's token/session in memory.
	if sender, err := instances[1].sender(ctx, "self"); err != nil || sender == nil {
		t.Fatalf("replica transport: %v %v", sender, err)
	}
	// Tokens never become durable credentials. Every new management operation
	// exchanges the current pod assertion, while ACS send keys stay sealed.
	var cluster azureClusterConnection
	if _, _, err := instances[0].store.read(ctx, azureClusterKey, &cluster); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(cluster)
	if strings.Contains(string(encoded), "ephemeral") || cluster.Session.AccessToken != "" || cluster.Session.RefreshToken != "" {
		t.Fatal("workload credential persisted")
	}
	if _, err := instances[0].clusterAction(ctx, "disconnectCluster", map[string]any{"confirmed": true}); err != nil {
		t.Fatal(err)
	}
	if err := instances[1].bootstrapInstallation(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := instances[1].store.read(ctx, azureClusterKey, &cluster); err != nil || cluster.Status != "disconnected" {
		t.Fatal("maintenance reconnected a user-disconnected identity")
	}
}

// Prefix only sealed setup-row identities, so this test exercises the real
// fresh-state path on a shared test database without erasing other fixtures.
// Both replicas use the same durable namespace and still acquire DB gates.
type installationTestStore struct {
	ConfigWriter
	prefix string
}

var installationTestID = regexp.MustCompile(`((?:stateId|id): )"([^"]+)"`)

func (s installationTestStore) Execute(ctx context.Context, query string) (any, error) {
	if strings.Contains(query, "emailConnectionStateById(") || strings.Contains(query, "storeEmailConnectionState(") {
		query = installationTestID.ReplaceAllStringFunc(query, func(m string) string {
			parts := installationTestID.FindStringSubmatch(m)
			return parts[1] + fmt.Sprintf("%q", emailStateID(s.prefix+parts[2]))
		})
	}
	return s.ConfigWriter.Execute(ctx, query)
}
