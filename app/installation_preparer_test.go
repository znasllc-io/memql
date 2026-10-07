package app

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
)

func TestInstallationPreparerRefusesMissingNativeConfigurationAndIdentity(t *testing.T) {
	t.Setenv("MEMQL_INSTALLATION_CONFIG_NAME", "")
	var absent *App
	_, err := absent.installationPreparer(nil, "")
	require.ErrorContains(t, err, "named receiving configuration")
	t.Setenv("MEMQL_INSTALLATION_CONFIG_NAME", "installation-receiver")
	_, err = absent.installationPreparer(nil, "")
	require.ErrorContains(t, err, "serving engine")
	engine := &memql.MemQLEngine{} // The refusal occurs before any engine operation.
	app := &App{engine: engine, Logger: quietLogger()}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	_, err = app.installationPreparer(nil, "")
	require.ErrorContains(t, err, "projected cluster identity")
	require.Nil(t, engine.IntegrationByName("installation"), "native wiring must not enable an unqualified public update capability")
}
