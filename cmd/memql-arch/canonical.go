package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Captured before canonical setup or tests alter ambient environment.
var generatorGOROOT = runtime.GOROOT()

// A canonical output is the declared default build on linux/amd64, not the
// operator's GOFLAGS, alternate workspace, external package driver or mounted
// product DSL. The executable itself runs on the host and is built from this
// checkout by make; only source loading is cross-targeted.
func prepareCanonical(root string) error {
	raw, err := os.ReadFile(filepath.Join(root, "go.work"))
	if err != nil {
		return err
	}
	if err := requireToolchain(raw, runtime.Version()); err != nil {
		return err
	}
	for key, value := range canonicalEnvironment(root) {
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	if err := verifyCanonicalDriver(); err != nil {
		return err
	}
	fmt.Printf("memql-arch: canonical %s linux/amd64/v1 cgo=0 explicit-tags=none experiments=default fips=off driver=off\n", runtime.Version())
	return nil
}

func requireToolchain(work []byte, actual string) error {
	var expected string
	for _, line := range strings.Split(string(work), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "toolchain" {
			if len(fields) != 2 || expected != "" {
				return fmt.Errorf("ambiguous workspace toolchain")
			}
			expected = fields[1]
		}
	}
	if expected == "" || actual != expected {
		return fmt.Errorf("toolchain %q differs from workspace pin %q", actual, expected)
	}
	return nil
}

func canonicalEnvironment(root string) map[string]string {
	return map[string]string{
		"GOROOT": generatorGOROOT, "PATH": filepath.Join(generatorGOROOT, "bin") + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GOWORK": filepath.Join(root, "go.work"), "GOENV": "off", "GOFLAGS": "-mod=readonly -p=1",
		"GOPACKAGESDRIVER": "off", "GOTOOLCHAIN": "local", "GOOS": "linux", "GOARCH": "amd64",
		"CGO_ENABLED": "0", "GOEXPERIMENT": "", "GOAMD64": "v1", "GOFIPS140": "off", "GODEBUG": "", "MEMQL_DSL_PATH": "",
	}
}

// go/packages starts "go" through PATH. Pin and check that actual child driver,
// not only this generator's version. No automatic toolchain selection is allowed.
func verifyCanonicalDriver() error {
	driver := filepath.Join(generatorGOROOT, "bin", "go")
	info, err := os.Stat(driver)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("canonical Go driver is not an executable file")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, driver, "version").Output()
	if err != nil {
		return fmt.Errorf("canonical Go driver: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 4 || fields[0] != "go" || fields[1] != "version" || fields[2] != runtime.Version() {
		return fmt.Errorf("canonical Go driver differs from generator toolchain")
	}
	return nil
}
