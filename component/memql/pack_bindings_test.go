package memql

import "testing"

func TestBindPluginToPack(t *testing.T) {
	t.Cleanup(func() {
		unbindPluginFromPackForTest("testpackA")
		unbindPluginFromPackForTest("testpackB")
	})

	if _, ok := PackDomainForPlugin("testpackA"); ok {
		t.Fatalf("unbound plugin reported a pack domain")
	}

	BindPluginToPack("testpackA", "testdomain")
	BindPluginToPack("testpackB", "testdomain")

	domain, ok := PackDomainForPlugin("testpackA")
	if !ok || domain != "testdomain" {
		t.Fatalf("PackDomainForPlugin(testpackA) = %q, %v; want testdomain, true", domain, ok)
	}

	// Idempotent same-pair re-bind: init paths can re-run in tests.
	BindPluginToPack("testpackA", "testdomain")

	got := PluginsForPackDomain("testdomain")
	if len(got) != 2 || got[0] != "testpackA" || got[1] != "testpackB" {
		t.Fatalf("PluginsForPackDomain = %v; want [testpackA testpackB] (sorted)", got)
	}

	if got := PluginsForPackDomain("nosuchdomain"); len(got) != 0 {
		t.Fatalf("PluginsForPackDomain(nosuchdomain) = %v; want empty", got)
	}
}

func TestBindPluginToPackConflictPanics(t *testing.T) {
	t.Cleanup(func() { unbindPluginFromPackForTest("testconflict") })
	BindPluginToPack("testconflict", "domainone")

	defer func() {
		if recover() == nil {
			t.Fatalf("re-binding a plugin to a DIFFERENT domain must panic")
		}
	}()
	BindPluginToPack("testconflict", "domaintwo")
}

func TestBindPluginToPackEmptyArgsPanic(t *testing.T) {
	for _, tc := range []struct{ name, domain string }{
		{"", "d"},
		{"  ", "d"},
		{"p", ""},
		{"p", "  "},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("BindPluginToPack(%q, %q) must panic", tc.name, tc.domain)
				}
			}()
			BindPluginToPack(tc.name, tc.domain)
		}()
	}
}
