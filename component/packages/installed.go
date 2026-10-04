package packages

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// The optional ConfigMap is supplied by the instance repository. A projected
// directory (never subPath) means every replica sees subsequent GitOps updates.
const InstalledManifestPath = "/etc/memql/package/memql-package.yaml"

func ReadInstalledManifest() (*Manifest, error) {
	return readInstalledManifest(InstalledManifestPath)
}

func readInstalledManifest(path string) (*Manifest, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read the installation's package configuration")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read the installation's package configuration")
	}
	if len(data) > 2<<20 {
		return nil, fmt.Errorf("installation package manifest exceeds 2 MiB")
	}
	return ParseManifest(data)
}
