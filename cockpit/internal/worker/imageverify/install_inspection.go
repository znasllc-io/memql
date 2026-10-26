package imageverify

import "context"

type serviceInstallationObservation struct {
	SchemaVersion      int              `json:"schemaVersion"`
	InstallationSHA256 string           `json:"installationSHA256"`
	Host               ServiceHostFacts `json:"host"`
	Qualified          bool             `json:"qualified"`
}

func inspectServiceInstallationObservation(ctx context.Context, fs serviceFiles, c ServiceConfig, observe func(context.Context, ServiceConfig) (ServiceHostFacts, error)) (serviceInstallationObservation, error) {
	var result serviceInstallationObservation
	if err := ctx.Err(); err != nil {
		return result, err
	}
	// Verify fixed executable/dependency bytes before even read-only systemctl
	// metadata collection. No previous advertisement cache authorizes this port.
	if err := inspectServiceInstallation(fs, c); err != nil {
		return result, err
	}
	host, err := observe(ctx, c)
	if err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	digest, err := ServiceInstallationDigest(c)
	if err != nil {
		return result, err
	}
	return serviceInstallationObservation{SchemaVersion: 1, InstallationSHA256: digest, Host: host, Qualified: false}, nil
}
