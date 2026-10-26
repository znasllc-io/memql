//go:build linux

package imageverify

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

// Read only the candidate bytes on stdin and the fixed installed configuration.
// The root installation phase drops to the configured owner for this observation;
// it cannot select a different path, alter policy or ask the helper to write.
func validateServiceAttestationForOperator(input io.Reader, output io.Writer) error {
	c, err := operatorServiceConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := validateServiceAttestationObservation(ctx, installedServiceFiles{}, c, input, time.Now(), operatorServiceHostFacts)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
