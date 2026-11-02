package azureblob

import (
	"io"
	"log/slog"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// plugin_test.go -- the storage plug-in exactly as a node builds it, through
// the factory init() registers.

// unusedBlobConnectionString parses and is never dialled: building a client
// does no I/O. It carries a shared access signature rather than an account key,
// so nothing here is shaped like a credential.
const unusedBlobConnectionString = "BlobEndpoint=http://127.0.0.1:1/devaccount;SharedAccessSignature=sv=2022-11-02&sig=unsigned"

// materializeStoragePlugin runs the factory registered under "storage".
func materializeStoragePlugin(t *testing.T) memql.IntegrationProvider {
	t.Helper()
	for _, reg := range memql.RegisteredPlugins() {
		if reg.Name != "storage" {
			continue
		}
		provider, err := reg.Factory(memql.PluginContext{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err != nil {
			t.Fatalf("the storage plug-in's factory failed: %v", err)
		}
		return provider
	}
	t.Fatal("no plug-in is registered as storage, so no node hosts the storage readiness module")
	return nil
}

// TestTheStoragePlugInMaterializesWhereBlobStorageIsUsable holds the reason the
// registration exists. The `storage` readiness module is
// `hostedBy: integrations: [storage]` (component/envregistry/manifest.yaml), so
// a factory that stopped materializing, or a registration under another name,
// would leave every node reporting the module as not applicable -- which the
// first-run wizard and the Files, Logs and Materializer apps read as storage
// missing.
func TestTheStoragePlugInMaterializesWhereBlobStorageIsUsable(t *testing.T) {
	t.Setenv("MEMQL_AZURE_BLOB_CONTAINER", "probe-container")
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", unusedBlobConnectionString)

	provider := materializeStoragePlugin(t)
	if provider == nil {
		t.Fatal("the storage plug-in did not materialize on a node with blob storage configured")
	}
	if provider.IntegrationName() != "storage" {
		t.Fatalf("the plug-in registers as %q; the storage readiness module is hosted by the name storage", provider.IntegrationName())
	}
}

// TestTheStoragePlugInOffersTheDSLNoCapability. A capability on this plug-in
// would act with the node's storage credential for whoever called it, and
// native producers write through their own clients. This test is the stop a new
// one has to pass: give it a narrow path with its own gate, not a general one
// here.
func TestTheStoragePlugInOffersTheDSLNoCapability(t *testing.T) {
	t.Setenv("MEMQL_AZURE_BLOB_CONTAINER", "probe-container")
	t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", unusedBlobConnectionString)

	provider := materializeStoragePlugin(t)
	if provider == nil {
		t.Fatal("the storage plug-in did not materialize on a node with blob storage configured")
	}
	for _, c := range provider.Capabilities() {
		t.Errorf("the storage plug-in offers the DSL %q", c.Name)
	}
}

// TestTheStoragePlugInOptsOutWhereBlobStorageIsNotUsable. A node whose blob
// client cannot be built hosts no storage verdict, so it reports the module as
// not applicable rather than voting on a client it does not have.
func TestTheStoragePlugInOptsOutWhereBlobStorageIsNotUsable(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		container, connection string
	}{
		{"no container", "", unusedBlobConnectionString},
		{"no connection string", "probe-container", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("MEMQL_AZURE_BLOB_CONTAINER", tc.container)
			t.Setenv("MEMQL_AZURE_STORAGE_CONNECTION_STRING", tc.connection)
			if provider := materializeStoragePlugin(t); provider != nil {
				t.Fatalf("the storage plug-in materialized with %s", tc.name)
			}
		})
	}
}
