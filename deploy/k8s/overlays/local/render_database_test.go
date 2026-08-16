package local

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The local database moved from a hand-rolled `postgres` Deployment to a
// CloudNativePG Cluster (epic memql#3842, task memql#3846). These gates cover
// the properties whose failure is silent, delayed, or destructive -- not the
// ones a broken render would surface on its own.

// databaseResource is the slice of the rendered CNPG objects these gates read.
type databaseResource struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		// Cluster
		Instances  int    `yaml:"instances"`
		ImageName  string `yaml:"imageName"`
		EnablePDB  *bool  `yaml:"enablePDB"`
		PostgreSQL struct {
			SharedPreloadLibraries []string          `yaml:"shared_preload_libraries"`
			Parameters             map[string]string `yaml:"parameters"`
		} `yaml:"postgresql"`
		Bootstrap struct {
			InitDB struct {
				Database string `yaml:"database"`
				Owner    string `yaml:"owner"`
				Secret   struct {
					Name string `yaml:"name"`
				} `yaml:"secret"`
			} `yaml:"initdb"`
		} `yaml:"bootstrap"`
		WalStorage *struct {
			Size string `yaml:"size"`
		} `yaml:"walStorage"`
		Plugins []struct {
			Name          string            `yaml:"name"`
			IsWALArchiver bool              `yaml:"isWALArchiver"`
			Parameters    map[string]string `yaml:"parameters"`
		} `yaml:"plugins"`
		// Database
		Extensions []struct {
			Name    string `yaml:"name"`
			Version string `yaml:"version"`
		} `yaml:"extensions"`
		// ScheduledBackup
		Schedule string `yaml:"schedule"`
		Method   string `yaml:"method"`
	} `yaml:"spec"`
}

func databaseObjects(t *testing.T) map[string]databaseResource {
	t.Helper()
	out := map[string]databaseResource{}
	dec := yaml.NewDecoder(strings.NewReader(render(t)))
	for {
		var r databaseResource
		if err := dec.Decode(&r); err != nil {
			break
		}
		if r.Kind == "" {
			continue
		}
		out[r.Kind+"/"+r.Metadata.Name] = r
	}
	if len(out) == 0 {
		t.Fatal("the rendered overlay parsed to zero resources")
	}
	return out
}

// TestLocalDatabaseIsACloudNativePGCluster is the parity gate: the local
// database must be the same four kinds staging and production declare, because
// that is the entire claim of this epic. A `postgres` Deployment reappearing
// here -- as a "quick fix" for a slow bring-up, say -- would restore the one
// place local proved nothing about the cloud.
func TestLocalDatabaseIsACloudNativePGCluster(t *testing.T) {
	objs := databaseObjects(t)

	for _, want := range []string{
		"Cluster/memql-db",
		"Database/memql-db-memql",
		"ObjectStore/memql-db-backup",
		"ScheduledBackup/memql-db-nightly",
	} {
		if _, ok := objs[want]; !ok {
			t.Errorf("the local overlay does not render %s", want)
		}
	}

	if _, ok := objs["Deployment/postgres"]; ok {
		t.Error("the local overlay renders a `postgres` Deployment again. The database is a CloudNativePG " +
			"Cluster in every environment now; a Deployment here is the one place environment parity used to bend, " +
			"with no backups, no WAL archiving and no operator behind it.")
	}
}

// TestSingleInstanceClusterDisablesThePDB catches the trap documented in
// deploy/cnpg/README.md, and it is worth a test because the symptom points
// nowhere near the cause.
//
// CNPG creates a PodDisruptionBudget per Cluster. With one instance that PDB
// permits ZERO disruptions, so any node drain -- `kubectl drain`, a k3d node
// restart, an AKS node-pool upgrade -- blocks forever on a pod nothing can
// safely evict. There is no error naming the PDB; the drain simply never
// returns.
func TestSingleInstanceClusterDisablesThePDB(t *testing.T) {
	c, ok := databaseObjects(t)["Cluster/memql-db"]
	if !ok {
		t.Fatal("no Cluster/memql-db rendered")
	}

	if c.Spec.Instances != 1 {
		t.Errorf("the local Cluster declares %d instances, want 1 (local is one instance; staging and production carry the HA counts)", c.Spec.Instances)
	}
	if c.Spec.Instances == 1 {
		if c.Spec.EnablePDB == nil {
			t.Error("the single-instance local Cluster does not set enablePDB. CNPG's default PDB permits zero " +
				"disruptions at one instance, so a node drain blocks forever with no error that points here.")
		} else if *c.Spec.EnablePDB {
			t.Error("the single-instance local Cluster sets enablePDB:true, which blocks every node drain forever")
		}
	}
}

// TestLocalClusterLoadsTimescaleAndSeparatesWAL covers the two Cluster
// settings that fail late rather than at apply.
func TestLocalClusterLoadsTimescaleAndSeparatesWAL(t *testing.T) {
	c, ok := databaseObjects(t)["Cluster/memql-db"]
	if !ok {
		t.Fatal("no Cluster/memql-db rendered")
	}

	var preloaded bool
	for _, lib := range c.Spec.PostgreSQL.SharedPreloadLibraries {
		if lib == "timescaledb" {
			preloaded = true
		}
	}
	if !preloaded {
		t.Error("the Cluster does not preload timescaledb. It is a preload library, not merely an extension: " +
			"without it CREATE EXTENSION fails with a message naming the library rather than the cause, and the " +
			"migrations that build the continuous aggregates never run.")
	}

	// WAL on its own volume is one of the few things that has to be the same
	// SHAPE everywhere to be worth anything: sharing a volume with data means a
	// WAL-archiving stall fills the data disk and stops the database, which is
	// the exact failure the staging/prod split exists to prevent. A local
	// overlay without it rehearses a different topology.
	if c.Spec.WalStorage == nil || c.Spec.WalStorage.Size == "" {
		t.Error("the local Cluster declares no separate walStorage; a WAL-archiving stall would then fill the " +
			"data volume and stop the database, which is the failure the separate volume exists to bound")
	}

	if c.Spec.Bootstrap.InitDB.Secret.Name != "memql-db-app-creds" {
		t.Errorf("bootstrap.initdb.secret is %q, want memql-db-app-creds -- the Secret `make secrets` seeds, "+
			"and the one whose username/password the DSN is built from",
			c.Spec.Bootstrap.InitDB.Secret.Name)
	}

	var archiver bool
	for _, p := range c.Spec.Plugins {
		if p.Name == "barman-cloud.cloudnative-pg.io" && p.IsWALArchiver {
			archiver = true
		}
	}
	if !archiver {
		t.Error("no barman-cloud plugin is declared as the WAL archiver; the ObjectStore would then be a " +
			"destination nothing writes to, and the backups would silently not exist")
	}
}

// TestScheduledBackupUsesASixFieldCron is a small gate for a mistake that is
// almost impossible to see.
//
// CNPG's schedule is a Go cron spec WITH A SECONDS FIELD -- six fields, unlike
// Kubernetes CronJob's five. A five-field spec is not rejected; it is
// interpreted one field over, so "0 3 * * *" (intended: 03:00 daily) becomes
// "second 0, minute 3, every hour". The backup still runs, just not when anyone
// thinks, and nothing anywhere reports a problem.
func TestScheduledBackupUsesASixFieldCron(t *testing.T) {
	sb, ok := databaseObjects(t)["ScheduledBackup/memql-db-nightly"]
	if !ok {
		t.Fatal("no ScheduledBackup/memql-db-nightly rendered")
	}

	if fields := strings.Fields(sb.Spec.Schedule); len(fields) != 6 {
		t.Errorf("the ScheduledBackup schedule %q has %d fields, want 6. CNPG's cron carries a SECONDS field; "+
			"a five-field spec is interpreted one field over rather than rejected, so the backup runs at a time "+
			"nobody intended and nothing reports it.", sb.Spec.Schedule, len(fields))
	}
	// The plugin method, matching the archiver above. `barmanObjectStore` is the
	// deprecated in-tree path and would need an operand image built on the
	// deprecated `system` base to work at all.
	if sb.Spec.Method != "plugin" {
		t.Errorf("the ScheduledBackup method is %q, want plugin -- the in-tree barmanObjectStore path is "+
			"deprecated, and the operand image is built on the `standard` base that does not carry its binaries",
			sb.Spec.Method)
	}
}

// TestLocalBackupsCanActuallyReachAzurite guards the two settings that make
// local WAL archiving work, both of which were found by running it and neither
// of which looks load-bearing in a diff.
//
// # 1. An emulator destination is a full HTTP URL, not `azure://`
//
// Against Azurite, barman rejects the scheme form outright:
//
//	ERROR: Barman cloud WAL archive check exception:
//	       emulated storage URL azure://memql-db-backups/ is malformed
//
// which reaches the operator as `ContinuousArchivingFailing` on the Cluster and
// an `exit status 4` in the sidecar -- neither of which names the URL shape.
// Staging and production, talking to real Azure Blob, use the `azure://` form;
// this is a property of the EMULATOR, not of the environment, which is why the
// difference is confined to this one line.
//
// # 2. Azurite must skip its API-version check
//
// Azurite refuses any request whose `x-ms-version` it does not recognise, and
// barman-cloud's Azure SDK sends a newer one than 3.34.0 knows:
//
//	ErrorCode: InvalidHeaderValue
//	The API version 2026-06-06 is not supported by Azurite.
//
// Same symptom, different exit code (2 rather than 4), and it points at barman
// rather than at the emulator. `--skipApiVersionCheck` is Azurite's own
// documented remedy.
//
// The two are asserted together because they FAIL together: with either one
// missing there are no backups at all, and the Cluster still reports Ready --
// so nothing else in this repository would notice.
func TestLocalBackupsCanActuallyReachAzurite(t *testing.T) {
	rendered := render(t)

	if _, ok := databaseObjects(t)["ObjectStore/memql-db-backup"]; !ok {
		t.Fatal("no ObjectStore/memql-db-backup rendered")
	}

	// Read destinationPath out of the rendered stream: it sits under
	// spec.configuration, which the shared struct above deliberately does not
	// model (every other gate here reads Cluster/Database fields).
	var dest string
	for _, line := range strings.Split(rendered, "\n") {
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "destinationPath:") {
			dest = strings.Trim(strings.TrimSpace(strings.TrimPrefix(s, "destinationPath:")), `"'`)
		}
	}
	if dest == "" {
		t.Fatal("the rendered overlay declares no destinationPath")
	}
	if strings.HasPrefix(dest, "azure://") {
		t.Errorf("the local ObjectStore destinationPath is %q. barman refuses the azure:// form against an "+
			"emulator (\"emulated storage URL ... is malformed\"), and the only symptom is "+
			"ContinuousArchivingFailing on a Cluster that still reports Ready. Local needs the full HTTP form "+
			"including the account: http://azurite:10000/<account>/<container>", dest)
	}
	if !strings.Contains(dest, "azurite:10000") {
		t.Errorf("the local ObjectStore destinationPath is %q, which does not point at the in-cluster Azurite", dest)
	}

	// Azurite's own flag, on the Deployment this overlay ships.
	if !strings.Contains(rendered, "--skipApiVersionCheck") {
		t.Error("the azurite Deployment does not pass --skipApiVersionCheck. Azurite rejects any x-ms-version it " +
			"does not recognise, and barman-cloud's Azure SDK sends a newer one than 3.34.0 knows -- so WAL " +
			"archiving fails with InvalidHeaderValue while the Cluster still reports Ready.")
	}
}

// TestDatabaseExtensionVersionMatchesTheImage is the cross-file drift gate.
//
// The Database CR pins the TimescaleDB version, and the operand image ships a
// specific pair. Pinning a version the image does not carry is not a render
// error -- it is CNPG running an ALTER EXTENSION to a version whose library is
// absent, which fails at reconcile time against a database that is otherwise
// up.
func TestDatabaseExtensionVersionMatchesTheImage(t *testing.T) {
	db, ok := databaseObjects(t)["Database/memql-db-memql"]
	if !ok {
		t.Fatal("no Database/memql-db-memql rendered")
	}

	var pinned string
	var sawVector bool
	for _, e := range db.Spec.Extensions {
		switch e.Name {
		case "timescaledb":
			pinned = e.Version
		case "vector":
			sawVector = true
		}
	}
	if pinned == "" {
		t.Fatal("the Database CR does not pin a timescaledb version; it would then follow whatever the image " +
			"happens to carry, which is the opposite of a choreographed upgrade")
	}
	if !sawVector {
		t.Error("the Database CR does not declare the `vector` extension; the pgvector HNSW indexes the " +
			"migrations build would fail to create")
	}

	// deploy/db-image/Dockerfile is the authority on what the image carries.
	root := filepath.Join("..", "..", "..", "..")
	b, err := os.ReadFile(filepath.Join(root, "deploy", "db-image", "Dockerfile"))
	if err != nil {
		t.Fatalf("reading the operand Dockerfile: %v", err)
	}
	var current string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "ARG TIMESCALEDB_VERSION=") {
			current = strings.TrimPrefix(line, "ARG TIMESCALEDB_VERSION=")
			break
		}
	}
	if current == "" {
		t.Fatal("could not read TIMESCALEDB_VERSION out of deploy/db-image/Dockerfile; this guard is no longer reading what it claims to")
	}
	if pinned != current {
		t.Errorf("the Database CR pins timescaledb %s but the operand image is built with %s.\n"+
			"A version the image does not carry is not a render error -- CNPG runs ALTER EXTENSION to a version "+
			"whose library is absent, which fails at reconcile against a database that is otherwise up.\n"+
			"An upgrade is deliberately two PRs: move the image first, then this pin.", pinned, current)
	}
}

// TestSeedSecretsPointsAtTheRenderedCluster ties the DSN to the Cluster's name.
//
// CNPG derives its Services from the Cluster's name: `<name>-rw` follows the
// current primary. Renaming the Cluster without editing seed-secrets.sh leaves
// every node dialling a Service that does not exist -- and the symptom is the
// whole mesh failing to reach a database that is running perfectly well.
func TestSeedSecretsPointsAtTheRenderedCluster(t *testing.T) {
	c, ok := databaseObjects(t)["Cluster/memql-db"]
	if !ok {
		t.Fatal("no Cluster/memql-db rendered")
	}
	wantHost := c.Metadata.Name + "-rw"

	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "scripts", "k3d", "seed-secrets.sh"))
	if err != nil {
		t.Fatalf("reading seed-secrets.sh: %v", err)
	}
	body := string(b)

	if !strings.Contains(body, "@"+wantHost+":5432/") {
		t.Errorf("seed-secrets.sh does not build its DSN against %q. CNPG names that Service from the Cluster's "+
			"metadata.name, so the two move together or the whole mesh dials a Service that does not exist.", wantHost)
	}
	if strings.Contains(body, "@postgres:5432/") {
		t.Error("seed-secrets.sh still points the DSN at the retired `postgres` Service")
	}
	// The credential the Cluster bootstraps from must be the basic-auth shape
	// CNPG reads, not the POSTGRES_USER/POSTGRES_PASSWORD pair the old
	// Deployment consumed as container env.
	if !strings.Contains(body, "--type=kubernetes.io/basic-auth") {
		t.Error("memql-db-app-creds is not seeded as a kubernetes.io/basic-auth Secret; that is the shape " +
			"CNPG's bootstrap.initdb.secret reads, and initdb fails without it")
	}
}
