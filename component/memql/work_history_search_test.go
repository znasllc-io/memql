package memql

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
)

func historySearchEnvelope(t *testing.T, body string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"data": []any{map[string]any{"result": map[string]any{"payload": map[string]any{"body": body, "status": 200}}}}})
	require.NoError(t, err)
	return string(raw)
}

func TestWorkHistorySearchDecodedTextKeepsExactSerializedOffsets(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, needle := range []string{"line one\nline two", `"quoted phrase"`, `C:\source\file`, "tab\tvalue", "🧪 café"} {
			t.Run(needle+map[bool]string{true: "/envelope", false: "/plain"}[nested], func(t *testing.T) {
				content := strings.Repeat("İ文🧪 ", 200) + needle + strings.Repeat(" tail", 100)
				encoded, err := json.Marshal(needle)
				require.NoError(t, err)
				if nested {
					content = historySearchEnvelope(t, content)
					encoded, err = json.Marshal(string(encoded[1 : len(encoded)-1]))
					require.NoError(t, err)
				}
				message := common.ChatMessage{Role: "tool", Name: "readSource", Content: content}
				raw, err := json.Marshal(message)
				require.NoError(t, err)
				match := strings.Index(string(raw), string(encoded[1:len(encoded)-1]))
				require.Positive(t, match)
				require.Equal(t, match, workHistorySearchPosition(string(raw), strings.ToUpper(needle)))
				page, ok := workHistoryPage(message, strings.ToLower(needle), 0, 400, false)
				require.True(t, ok)
				wantOffset := utf8.RuneCount(raw[:match]) - 120
				require.Equal(t, wantOffset, page["offset"])
				require.Equal(t, string([]rune(string(raw))[wantOffset:wantOffset+400]), page["source"])
				page, ok = workHistoryPage(message, strings.ToLower(needle), 0, 100, true)
				require.True(t, ok)
				require.Equal(t, 0, page["offset"], "an explicit page offset is never moved by search")
				require.Equal(t, string([]rune(string(raw))[:100]), page["source"])
			})
		}
	}
}

func TestWorkHistorySearchDoesNotConfuseLiteralEscapesWithCharacters(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, pair := range [][2]string{
			{`alpha\nbeta`, "alpha\nbeta"},
			{"alpha\nbeta", `alpha\nbeta`},
			{`alpha\u0022beta`, `alpha"beta`},
			{`alpha"beta`, `alpha\"beta`},
		} {
			content := pair[0]
			if nested {
				content = historySearchEnvelope(t, content)
			}
			message := common.ChatMessage{Role: "tool", Content: content}
			_, found := workHistoryPage(message, pair[1], 0, 400, false)
			require.False(t, found, "nested=%v literal=%q search=%q", nested, pair[0], pair[1])
			_, found = workHistoryPage(message, pair[0], 0, 400, false)
			require.True(t, found, "exact literal text remains searchable")
		}
	}
}

func TestWorkHistorySearchMapsUnicodeEscapesAndBoundsNestedParsing(t *testing.T) {
	source := `{"Content":"{\"body\":\"\\u0130\\ud83e\\uddea prefix\\n\\\"target\\\"\"}"}`
	match := workHistorySearchPosition(source, "İ🧪 prefix\n\"target\"")
	require.Equal(t, strings.Index(source, `\\u0130`), match)
	match = workHistorySearchPosition(source, "prefix\n\"target\"")
	require.Equal(t, strings.Index(source, "prefix"), match)
	for _, literal := range []string{`"\ud800 x"`, `"\ud800\udc00 x"`, `"\udc00 x"`} {
		require.Equal(t, strings.Index(literal, "x"), workHistoryJSONRuneOffset(literal, 2))
	}
	nested := `{"body":"needle"}`
	for range workHistorySearchDepth {
		nested = historySearchEnvelope(t, nested)
	}
	require.Equal(t, -1, workHistorySearchPosition(nested, "needle"))
	remaining := 10
	require.Equal(t, -1, workHistorySearchJSON(`{"body":"needle"}`, "needle", 0, &remaining))
	require.Equal(t, -1, workHistorySearchPosition(`{"one":"left","two":"right"}`, "left right"))
}
