package repositoryauthority

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTextEditsChangeALargeVersionFileWithoutRepeatingIt(t *testing.T) {
	content := strings.Repeat("// unchanged code\n", 20000) + "const version = \"0.22.2\"\n"
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	source := RepositorySnapshot{Files: []Replacement{{Path: "cockpit/main.go", BeforeSHA256: hash, Content: content}, {Path: "new.txt"}}}
	edits := []TextEdit{{Path: "cockpit/main.go", BeforeSHA256: hash, Before: "const version = \"0.22.2\"", After: "const version = \"0.22.3\""}, {Path: "new.txt", After: "new file\n"}}
	result, err := applyTextEdits(source, edits)
	require.NoError(t, err)
	require.Len(t, result, 2)
	require.Equal(t, strings.Replace(content, "0.22.2", "0.22.3", 1), result[0].Content)
	require.Equal(t, hash, result[0].BeforeSHA256)
	require.Equal(t, "new file\n", result[1].Content)
}
func TestTextEditsRefuseAmbiguousChangedOrOutOfScopeInput(t *testing.T) {
	content := "version version"
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	source := RepositorySnapshot{Files: []Replacement{{Path: "VERSION", BeforeSHA256: hash, Content: content}}}
	for _, edit := range []TextEdit{
		{Path: "other", BeforeSHA256: hash, Before: content, After: "changed"},
		{Path: "VERSION", BeforeSHA256: strings.Repeat("a", 64), Before: content, After: "changed"},
		{Path: "VERSION", BeforeSHA256: hash, Before: "version", After: "changed"},
		{Path: "VERSION", BeforeSHA256: hash, Before: "absent", After: "changed"},
		{Path: "VERSION", BeforeSHA256: hash, Before: "", After: "changed"},
		{Path: "VERSION", BeforeSHA256: hash, Before: content, After: content},
	} {
		_, err := applyTextEdits(source, []TextEdit{edit})
		require.Error(t, err)
	}
}
