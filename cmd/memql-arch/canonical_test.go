package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/znasllc-io/memql/component/architecture/model"
)

func TestCanonicalGenerationPinsActualLoadingEnvironment(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.26.8\ntoolchain "+runtime.Version()+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for key := range canonicalEnvironment(root) {
		t.Setenv(key, "untrusted-ambient-value")
	}
	if err := prepareCanonical(root); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"GOFLAGS": "-mod=readonly -p=1", "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0",
		"GOAMD64": "v1", "GOFIPS140": "off", "GODEBUG": "",
		"GOENV": "off", "GOPACKAGESDRIVER": "off", "GOTOOLCHAIN": "local", "GOEXPERIMENT": "", "MEMQL_DSL_PATH": "", "GOWORK": filepath.Join(root, "go.work"),
	} {
		if got := os.Getenv(key); got != want {
			t.Errorf("%s=%q want %q", key, got, want)
		}
	}
}

func TestCanonicalGenerationRejectsUnpinnedToolchain(t *testing.T) {
	for _, raw := range []string{"go 1.26.8\n", "toolchain go1.1\n", "toolchain " + runtime.Version() + "\ntoolchain " + runtime.Version() + "\n"} {
		if err := requireToolchain([]byte(raw), runtime.Version()); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	if err := requireToolchain([]byte("toolchain "+runtime.Version()+"\n"), runtime.Version()); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedOutputWorksWithoutExistingBuildDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "build", "architecture", model.CanonicalFilename)
	m := model.NewModel("")
	m.GeneratedAt = ""
	if err := writeGeneratedModel(m, path); err != nil {
		t.Fatal(err)
	}
	if _, err := model.ReadFile(path); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalGenerationIgnoresHostileGoOnPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.26.8\ntoolchain "+runtime.Version()+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "ambient-driver-ran")
	fake := "#!/bin/sh\nprintf bad > '" + sentinel + "'\nexit 97\n"
	if err := os.WriteFile(filepath.Join(root, "go"), []byte(fake), 0700); err != nil {
		t.Fatal(err)
	}
	for key := range canonicalEnvironment(root) {
		t.Setenv(key, os.Getenv(key))
	}
	t.Setenv("PATH", root)
	t.Setenv("GOROOT", root)
	if err := prepareCanonical(root); err != nil {
		t.Fatal(err)
	}
	resolved, err := exec.LookPath("go")
	if err != nil || resolved != filepath.Join(generatorGOROOT, "bin", "go") {
		t.Fatalf("loader driver=%q error=%v", resolved, err)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatalf("ambient Go driver was executed: %v", err)
	}
}
