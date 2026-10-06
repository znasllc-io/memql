package envregistry

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalOverrideMissingFilePreservesClusterEnvironment(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "cluster.example")
	changed, err := ApplyLocalOverride(t.TempDir())
	if err != nil || len(changed) != 0 || os.Getenv("MEMQL_DOMAIN") != "cluster.example" {
		t.Fatalf("absent optional override changed cluster configuration: names=%v err=%v", changed, err)
	}
}

func TestLocalOverrideStillReportsAnInvalidPresentFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, LocalEnvFilename), []byte("invalid-line-without-equals\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyLocalOverride(dir); err == nil {
		t.Fatal("present malformed override was silently ignored")
	}
}
