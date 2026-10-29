package scan

import "testing"

// The package credential is read by downstream CI. Exempt only its exact
// registered name; an NPM_ prefix exemption would hide unrelated settings.
func TestPipelinePackageReadCredentialExemptionIsExact(t *testing.T) {
	if !isExternal("NPM_PACKAGES_READ_TOKEN") {
		t.Fatal("package-read credential must be classified as external CI input")
	}
	for _, name := range []string{"NPM_PACKAGES_WRITE_TOKEN", "NPM_PACKAGES_READ_TOKEN_EXTRA", "NPM_NEW_SETTING"} {
		if isExternal(name) {
			t.Errorf("package credential exemption unexpectedly covers %s", name)
		}
	}
}
