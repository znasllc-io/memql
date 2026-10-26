//go:build linux

package imageverify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"time"
)

// This fixed read-only port lets owner setup inspect the exact installed code,
// mappings, loaded policy and listening socket before pausing the worker. It
// neither creates nor requires a qualification attestation and never contacts
// the build service. Normal pipeline admission still requires genuine evidence.
func operatorServiceConfig() (ServiceConfig, error) {
	c, err := LoadServiceConfig()
	if err != nil {
		return ServiceConfig{}, err
	}
	if runtime.GOARCH != "amd64" || c.OwnerUID != os.Geteuid() || os.Geteuid() == 0 {
		return ServiceConfig{}, errors.New("image installation inspection requires its fixed non-root account and architecture")
	}
	return c, nil
}

func operatorServiceHostFacts(ctx context.Context, c ServiceConfig) (ServiceHostFacts, error) {
	host, err := collectServiceHostFacts(ctx, c, installedServiceHostReader{})
	if err != nil {
		return ServiceHostFacts{}, err
	}
	socket, err := readLoadedSocket(ctx, c)
	if err != nil {
		return ServiceHostFacts{}, err
	}
	if err = verifyServiceSocket(installedServiceFiles{}, c, socket); err != nil {
		return ServiceHostFacts{}, err
	}
	return host, nil
}

func checkServiceInstallationForOperator(output io.Writer) error {
	c, err := operatorServiceConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	observation, err := inspectServiceInstallationObservation(ctx, installedServiceFiles{}, c, operatorServiceHostFacts)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(observation)
}
