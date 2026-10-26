package imageverify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"
)

type serviceAttestationValidation struct {
	SchemaVersion      int    `json:"schemaVersion"`
	InstallationSHA256 string `json:"installationSHA256"`
	AttestationSHA256  string `json:"attestationSHA256"`
	Valid              bool   `json:"valid"`
}

// This read-only result validates a candidate against the actual installation;
// it does not attest that arbitrary supplied evidence was observed. The owner
// setup first joins all eight real fixture receipts, source and cleanup proofs,
// then installs exactly these hashed bytes at the previously absent fixed path.
// Neither this port nor its response grants normal pipeline admission.
func validateServiceAttestationObservation(ctx context.Context, fs serviceFiles, c ServiceConfig, input io.Reader, now time.Time, observe func(context.Context, ServiceConfig) (ServiceHostFacts, error)) (serviceAttestationValidation, error) {
	var result serviceAttestationValidation
	raw, err := io.ReadAll(io.LimitReader(input, serviceConfigMaxBytes+1))
	if err != nil {
		return result, err
	}
	if len(raw) == 0 || len(raw) > serviceConfigMaxBytes {
		return result, errors.New("image service candidate attestation is absent or unbounded")
	}
	current, err := inspectServiceInstallationObservation(ctx, fs, c, observe)
	if err != nil {
		return result, err
	}
	if err = verifyServiceQualification(c, current.Host, raw, now); err != nil {
		return result, err
	}
	digest := sha256.Sum256(raw)
	return serviceAttestationValidation{1, current.InstallationSHA256, hex.EncodeToString(digest[:]), true}, nil
}
