package calls

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	callrules "github.com/znasllc-io/memql/component/calls"
)

// Pinned LiveKit v1.13.7 LimitConfig.CheckParticipantNameLength counts UTF-8
// bytes, while Callkey accepts Unicode code points. Keep the rendered provider
// contract compatible with both human/agent names and owner resource labels.
// This is a pure source/validation contract, not a live-provider join test.
func TestMediaParticipantNameLimitPreservesNativeUnicodeNames(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	template, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "../../deploy/media/livekit.yaml.tmpl"))
	require.NoError(t, err)
	matches := regexp.MustCompile(`(?m)^  max_participant_name_length: ([0-9]+)\s*$`).FindSubmatch(template)
	require.Len(t, matches, 2)
	providerBytes, err := strconv.Atoi(string(matches[1]))
	require.NoError(t, err)
	require.Equal(t, maxDisplayNameRunes*utf8.UTFMax, providerBytes, "provider byte limit must cover every accepted native Unicode name")
	for _, character := range []string{"A", "界", "🙂"} {
		t.Run(strconv.Itoa(len(character))+"-byte-code-point", func(t *testing.T) {
			name := strings.Repeat(character, maxDisplayNameRunes)
			accepted, err := checkDisplayName(name)
			require.NoError(t, err)
			require.Equal(t, name, accepted)
			require.LessOrEqual(t, len(accepted), providerBytes)
			spec := callrules.ResourceSpec{Label: name, Kind: "synthetic", Audio: true, Video: true, MaxWidth: 1280, MaxHeight: 720, MaxFPS: 15, MaxBitrateKbps: 1000}
			require.True(t, callrules.ValidResourceSpec(spec), "owner publication names use the same Unicode envelope")
			_, err = checkDisplayName(name + character)
			require.Error(t, err, "provider compatibility must not enlarge native name authority")
			spec.Label += character
			require.False(t, callrules.ValidResourceSpec(spec))
		})
	}
	require.Greater(t, len(strings.Repeat("界", maxDisplayNameRunes)), 128, "three-byte names reproduce the old incompatible provider cap")
}
