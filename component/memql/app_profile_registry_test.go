package memql

import (
	"os"
	"path/filepath"
	"testing"
)

func TestActiveAppProfileEmptyByDefault(t *testing.T) {
	restore := setActiveAppProfileForTest(nil)
	defer restore()
	if _, ok := ActiveAppProfile(); ok {
		t.Fatalf("pure-engine binary must have no active app profile")
	}
	if _, content, ok := LoadActiveAppProfile(); ok || content != "" {
		t.Fatalf("LoadActiveAppProfile must be a no-op with an empty registry")
	}
}

func TestRegisterAppProfilePanics(t *testing.T) {
	restore := setActiveAppProfileForTest(nil)
	defer restore()
	assertPanics := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Fatalf("%s: expected panic", name)
			}
		}()
		fn()
	}
	assertPanics("empty name", func() { RegisterAppProfile(AppProfile{Content: func() string { return "x" }}) })
	assertPanics("nil content", func() { RegisterAppProfile(AppProfile{Name: "a"}) })
	RegisterAppProfile(AppProfile{Name: "one", Content: func() string { return "x" }})
	assertPanics("duplicate", func() {
		RegisterAppProfile(AppProfile{Name: "two", Content: func() string { return "y" }})
	})
}

func TestLoadActiveAppProfileDirOverrideWins(t *testing.T) {
	restore := setActiveAppProfileForTest(&AppProfile{
		Name:    "testapp",
		Content: func() string { return "embedded content" },
	})
	defer restore()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "testapp.md"), []byte("disk content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMQL_OPERATOR_APP_PROFILES_DIR", dir)

	// The cache is keyed by name and never invalidated; use a fresh map so
	// prior tests can't have primed it.
	appProfileCacheMu.Lock()
	appProfileCache = map[string]string{}
	appProfileCacheMu.Unlock()

	_, content, ok := LoadActiveAppProfile()
	if !ok || content != "disk content" {
		t.Fatalf("dir override must win: got ok=%v content=%q", ok, content)
	}
}

func TestLoadAppProfileContentRejectsBadNames(t *testing.T) {
	for _, name := range []string{"", "../etc/passwd", "UPPER", "a b", string(make([]byte, 70))} {
		if got := loadAppProfileContent(AppProfile{Name: name, Content: func() string { return "x" }}); got != "" {
			t.Fatalf("name %q must be rejected, got %q", name, got)
		}
	}
}
