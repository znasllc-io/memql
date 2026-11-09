package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/auth"
	"github.com/znasllc-io/memql/cockpit/internal/config"
)

func TestBackupSignInNamesExplicitBackgroundStoreAndRecovers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	previous := config.ActiveStore()
	store := &config.FileStore{Dir: filepath.Join(t.TempDir(), "credentials")}
	config.SetActiveStore(store)
	t.Cleanup(func() { config.SetActiveStore(previous) })
	cluster := config.ClusterConfig{Name: "test cluster", Endpoint: "https://api.example.test", Issuer: "https://identity.example.test"}
	if err := config.UpdateClusters(context.Background(), func(f *config.ClustersFile) error {
		f.Clusters = []config.ClusterConfig{cluster}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	bearer := backupBearer(cluster.Endpoint, nil)
	if bearer == nil {
		t.Fatal("missing bearer resolver")
	}
	_, err := bearer(context.Background())
	if !errors.Is(err, auth.ErrLoginRequired) {
		t.Fatalf("missing typed sign-in refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "MEMQL_COCKPIT_CRED_STORE=file memql login 'test cluster'") || !strings.Contains(err.Error(), "explicitly") || !strings.Contains(err.Error(), "owner-only files") {
		t.Fatalf("remediation must name opt-in, actual store and exact cluster: %v", err)
	}
	if _, err := os.Stat(store.Dir); !os.IsNotExist(err) {
		t.Fatal("diagnosis created or migrated credentials")
	}
	if config.ActiveStore() != store {
		t.Fatal("diagnosis changed credential storage")
	}
	// Only an explicit credential write in this isolated fixture enables the
	// existing resolver. No worker restart or new resolver is needed.
	if err := store.Put(cluster.Name, &config.StoredToken{AccessToken: "fixture-access", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	token, err := bearer(context.Background())
	if err != nil || token != "fixture-access" {
		t.Fatalf("did not recover from the existing store: %v", err)
	}
}

func TestBackupLoginCommandQuotesClusterLiterally(t *testing.T) {
	got := backupLoginCommand("team's $(not-a-command)")
	if got != "MEMQL_COCKPIT_CRED_STORE=file memql login 'team'\"'\"'s $(not-a-command)'" {
		t.Fatalf("unsafe shell argument: %s", got)
	}
}
