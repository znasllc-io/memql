package app

import (
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/core/emailconfig"
	"github.com/znasllc-io/memql/integrations/email"
)

func (a *App) wireEmailPackageDefaults() {
	integration, ok := a.engine.IntegrationByName("email").(*email.Integration)
	if !ok {
		return
	}
	integration.SetPackageDefaults(func() (*emailconfig.Campaigns, error) {
		manifest, err := packages.ReadInstalledManifest()
		if err != nil || manifest == nil {
			return nil, err
		}
		return manifest.Campaigns, nil
	})
}
