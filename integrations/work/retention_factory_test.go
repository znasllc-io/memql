package work

import (
	"context"
	"database/sql"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"testing"
)

func TestProductionFactoryWiresVerifiableArchive(t *testing.T) {
	t.Setenv(EnvArchiveContainer, "archive-test")
	// Synthetic credential: SDK client construction performs no network request.
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", "DefaultEndpointsProtocol=https;AccountName=retentiontest;AccountKey=dGVzdA==;EndpointSuffix=core.windows.net")
	for _, p := range memql.RegisteredPlugins() {
		if p.Name == "work" {
			// GateDB as a node wires it: a resolver of the gate pool, here one with no
			// pool behind it (this takes no gate; the plug-in refuses a context without
			// the resolver).
			provider, err := p.Factory(memql.PluginContext{
				AdmitSourceRow: func(context.Context, memorynodes.MemoryNode) bool { return true },
				GateDB:         func() *sql.DB { return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			integration := provider.(*Integration)
			if integration.archiverRef() == nil {
				t.Fatal("production work retention has no archive client")
			}
			if _, ok := integration.archiverRef().(interface {
				DownloadWithLimit(context.Context, string, string, int64) ([]byte, error)
			}); !ok {
				t.Fatal("production archiver cannot verify uploaded bytes")
			}
			return
		}
	}
	t.Fatal("work factory is absent")
}
