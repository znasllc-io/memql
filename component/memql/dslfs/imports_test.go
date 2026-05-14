package dslfs

import (
	"errors"
	"strings"
	"testing"
)

// TestResolveImport_HappyPaths locks the common-case resolutions.
func TestResolveImport_HappyPaths(t *testing.T) {
	cases := []struct {
		name         string
		importingFile string
		rawPath      string
		want         string
	}{
		{
			name:         "sibling file, default extension",
			importingFile: "cognition/queries/spaceParticipants.memql",
			rawPath:      "./other",
			want:         "cognition/queries/other.memql",
		},
		{
			name:         "sibling file, explicit extension",
			importingFile: "cognition/queries/spaceParticipants.memql",
			rawPath:      "./other.memql",
			want:         "cognition/queries/other.memql",
		},
		{
			name:         "parent directory",
			importingFile: "cognition/queries/spaceParticipants.memql",
			rawPath:      "../participant",
			want:         "cognition/participant.memql",
		},
		{
			name:         "grandparent directory",
			importingFile: "cognition/queries/sub/foo.memql",
			rawPath:      "../../participant",
			want:         "cognition/participant.memql",
		},
		{
			name:         "cross-directory sibling",
			importingFile: "cognition/queries/foo.memql",
			rawPath:      "../../common/space",
			want:         "common/space.memql",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveImport(c.importingFile, c.rawPath)
			if err != nil {
				t.Fatalf("ResolveImport: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestResolveImport_RejectsAbsolute locks the absolute-path rejection.
func TestResolveImport_RejectsAbsolute(t *testing.T) {
	_, err := ResolveImport("foo/bar.memql", "/etc/passwd")
	if err == nil {
		t.Fatal("expected error for absolute path, got nil")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error %q should mention 'absolute'", err.Error())
	}
}

// TestResolveImport_RejectsURL locks the URL-shape rejection.
func TestResolveImport_RejectsURL(t *testing.T) {
	for _, raw := range []string{"http://example.com/foo", "file:///tmp/foo"} {
		_, err := ResolveImport("foo/bar.memql", raw)
		if err == nil {
			t.Errorf("expected error for %q, got nil", raw)
		}
	}
}

// TestResolveImport_RejectsRootEscape locks the prefix check that
// keeps imports inside the DSL root.
func TestResolveImport_RejectsRootEscape(t *testing.T) {
	cases := []struct {
		importingFile string
		rawPath      string
	}{
		{"foo.memql", "../escape"},
		{"a/b.memql", "../../../escape"},
		{"deep/nested/file.memql", "../../../../../etc/passwd"},
	}
	for _, c := range cases {
		_, err := ResolveImport(c.importingFile, c.rawPath)
		if err == nil {
			t.Errorf("ResolveImport(%q, %q): expected escape error, got nil", c.importingFile, c.rawPath)
			continue
		}
		if !strings.Contains(err.Error(), "escapes") {
			t.Errorf("ResolveImport(%q, %q): error %q should mention 'escapes'", c.importingFile, c.rawPath, err.Error())
		}
	}
}

// TestResolveImport_RejectsNonMemqlExtension locks the .memql-only
// extension rule.
func TestResolveImport_RejectsNonMemqlExtension(t *testing.T) {
	for _, raw := range []string{"./foo.txt", "./bar.json", "./baz.go"} {
		_, err := ResolveImport("foo.memql", raw)
		if err == nil {
			t.Errorf("expected error for %q, got nil", raw)
		}
	}
}

// TestResolveImport_RejectsEmpty locks the empty-arg rejections.
func TestResolveImport_RejectsEmpty(t *testing.T) {
	if _, err := ResolveImport("", "./foo"); err == nil {
		t.Error("expected error for empty importingFile")
	}
	if _, err := ResolveImport("foo.memql", ""); err == nil {
		t.Error("expected error for empty rawPath")
	}
}

// TestDefaultAlias_BasenameStrip locks the basename-derivation rule.
func TestDefaultAlias_BasenameStrip(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"cognition/participant.memql", "participant"},
		{"common/space.memql", "space"},
		{"deep/nested/foo.memql", "foo"},
	}
	for _, c := range cases {
		got, err := DefaultAlias(c.path)
		if err != nil {
			t.Errorf("DefaultAlias(%q): %v", c.path, err)
			continue
		}
		if got != c.want {
			t.Errorf("DefaultAlias(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestDefaultAlias_RejectsReserved locks the reserved-name rejection.
func TestDefaultAlias_RejectsReserved(t *testing.T) {
	for _, name := range []string{"actor", "now", "partition", "config", "trace"} {
		_, err := DefaultAlias("dir/" + name + ".memql")
		if err == nil {
			t.Errorf("expected error for reserved alias %q, got nil", name)
			continue
		}
		if !errors.Is(err, ErrAliasReserved) {
			t.Errorf("DefaultAlias for %q: got %v, want errors.Is(err, ErrAliasReserved)", name, err)
		}
	}
}

// TestDefaultAlias_RejectsNonIdentifier locks the basename-must-be-
// identifier rule.
func TestDefaultAlias_RejectsNonIdentifier(t *testing.T) {
	for _, p := range []string{"dir/my-file.memql", "dir/1foo.memql", "dir/foo bar.memql"} {
		_, err := DefaultAlias(p)
		if err == nil {
			t.Errorf("expected error for non-identifier basename %q, got nil", p)
		}
	}
}

// TestValidateAlias_HappyPaths locks the legal-alias acceptance.
func TestValidateAlias_HappyPaths(t *testing.T) {
	for _, alias := range []string{"cog", "myAlias", "_internal", "x", "foo123"} {
		if err := ValidateAlias(alias); err != nil {
			t.Errorf("ValidateAlias(%q): unexpected error %v", alias, err)
		}
	}
}

// TestValidateAlias_RejectsBad locks the alias-validation error paths.
func TestValidateAlias_RejectsBad(t *testing.T) {
	cases := []struct {
		alias string
		want  string
	}{
		{"", "empty"},
		{"1foo", "identifier"},
		{"my-alias", "identifier"},
		{"actor", "reserved"},
		{"now", "reserved"},
	}
	for _, c := range cases {
		err := ValidateAlias(c.alias)
		if err == nil {
			t.Errorf("ValidateAlias(%q): expected error, got nil", c.alias)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("ValidateAlias(%q): error %q should mention %q", c.alias, err.Error(), c.want)
		}
	}
}
