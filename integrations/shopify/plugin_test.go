package shopify

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// TestPluginNameMatchesConnectorName pins the one place this package spells
// its own name twice.
//
// init() registers under a string LITERAL so the module-taxonomy gate's
// source scan can see it; everything else uses ConnectorName. Two spellings
// of one name is a thing that can drift, and the drift is quiet: the plugin
// would register as one name while the mirror's @origin, the inbound source
// prefix and the connector actor all used the other -- so deliveries would
// 404 and nothing would say why.
func TestPluginNameMatchesConnectorName(t *testing.T) {
	_, this, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(this), "plugin.go"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`memql\.RegisterPlugin\(\s*"([A-Za-z0-9_]+)"`).FindSubmatch(src)
	if m == nil {
		t.Fatal("plugin.go no longer registers under a string literal -- the module-taxonomy gate scans " +
			"source and reads a literal, so the registration would become invisible to it")
	}
	if got := string(m[1]); got != ConnectorName {
		t.Errorf("registered as %q but ConnectorName is %q -- the mirror's @origin, the inbound source "+
			"prefix and the connector actor all use the constant, so a delivery would 404 with nothing to say why",
			got, ConnectorName)
	}
}
